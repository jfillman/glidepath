# Change management: the `itsm` gate

Glidepath does not run a change tool. It wraps the one a company already has: a change request is created for
every release, the `itsm` gate holds the release PR until the change is approved or scheduled, and the change
follows the release to its end (implementing on merge, closed successful when the release is healthy, closed
unsuccessful when it is rolled back, with the rollback's emergency change linked). ServiceNow is the first
provider; a `webhook` provider fronts any other tool with one endpoint. With no provider configured the gate
stays the loud stub it was ([governance-stubs.md](governance-stubs.md), ADR-0003) and every hook is a no-op.

Whether the gate blocks is the registry's business, not the provider's: `releaseGuardrails[itsm].required`
([release-guardrails.md](release-guardrails.md), "Required and advisory gates"), `true` by default once a
provider is set. On GitHub Free a red check still cannot block a merge (GATE-03); a merge past a red `itsm`
check is a detected bypass (GATE-04).

## The lifecycle

| When | Who | What happens to the change request | On the release record |
|---|---|---|---|
| The release PR opens | `open-release-pr` | **Created**: a `normal` change for a promote, an `emergency` change for a rollback (its `parent` is the change of the release it rolls back). Carries app, environment, image, release id (`correlation_id`) and, once the PR exists, the PR link (a work note). Or **linked**: the caller's `change-request-id` param, or the id a reused open PR's body already names. The PR body gets `**Change request**: CHG... (provider, state ...)` and a hidden `<!-- glidepath:change-request:<id> -->` marker. | `changeRequestId`, `changeRequestNumber`, `changeRequestUrl`, `changeRequestState` |
| Every gate run on the PR | `itsm-gate` (Pipeline `itsm-check`) | **Read**: the change must exist and be approved or scheduled. Fails naming the change, its state and its approval; the PR comment says how to fix it and `/retest itsm-`. The record first, the PR body's marker as the fallback (a swept record, or a PR made by hand with a change someone linked). | `changeRequestState`, `changeRequestApproval` as last seen |
| The PR merges | `itsm-change implement` (Pipeline `bypass-merge-check`, after `mark-release-merged`) | State **Implement** (`-1`), work note with the PR and what is deploying where. | `changeRequestState: implementing` |
| The relay reports `environment.deployed` success | `itsm-change close-outcome` (Pipeline `release-outcome-notify`) | **Closed**, `close_code: successful`. | `changeRequestState: closed-successful` |
| The relay reports `environment.deployed` failure | same | A work note; the change stays open. A degraded release can still recover (the success then closes it) or be rolled back (below). | `changeRequestState: implementing-failed` |
| The relay reports `service.rolledback` | `itsm-change close-rolled-back` (Pipeline `release-rollback-notify`) | The change of the release that was **rolled back** is closed `unsuccessful`, naming the reason and the rollback's emergency change as the related change. The rollback release's own change closes successful through the row above when it turns healthy. | on the rolled-back release's record: `changeRequestState: closed-unsuccessful`, `rolledBackByChange` |

A release that was healthy (its change closed successful) and is rolled back later gets its change **re-closed
unsuccessful** by the rollback: the close is a `PATCH` on an already-closed change. A ServiceNow instance whose
rules forbid updating a closed change fails that step visibly (`onError: continue`, the TaskRun is red); the
rollback's emergency change still names the original as its `parent`, so the relationship is recorded either
way.

A change tool that is down fails `open-release-pr` before a PR exists (a release without its change is what
change management exists to prevent), fails the gate (loudly, with the HTTP status), and is tolerated by the
later hooks (`onError: continue`: the failed TaskRun shows, the bypass check and the outcome notifications
still run). `resolve-release-tracking` exposes the change on its results (`change-request-id`,
`change-request-number`, `change-request-url`) for anything that reads the release outcome, and the record
fields are what Tower's Release Record can show (Tower work is separate).

## Configuration

Catalog values (`charts/glidepath-catalog/values.yaml`, overridden per cluster in the catalog Application):

```yaml
itsm:
  provider: servicenow            # none | servicenow | webhook
  credentialsSecret: itsm-credentials
  servicenow:
    instanceUrl: https://acme.service-now.com
    approvedStates: ["-2", "-1"] # Scheduled, Implement (ServiceNow's default change model)
  webhook:
    url: https://change-adapter.internal/glidepath
```

`releaseGuardrailOverrides: { itsm: { required: false } }` makes an unapproved change advisory (the check
stays green and the PR comment says what it found) while a company gets used to the flow.

**Credentials** come from a platform Secret in each app's `-cicd` namespace, never a value: the control plane's
`itsm.credentials` renders a `ClusterExternalSecret` that fans `itsm-credentials` out of the platform's
Infisical project into every managed namespace ([secrets-management.md](secrets-management.md)), from the
keys `itsm-token` (a bearer token) or `itsm-username` + `itsm-password` (basic auth), planted by hand in
Infisical once per cluster. The Tasks mount the Secret optional and warn when it is missing, which is what a
lab's throwaway endpoint wants.

```yaml
# glidepath-control-plane values
itsm:
  credentials:
    enabled: true
    keys: [username, password]
```

## The provider contract

`charts/glidepath-catalog/files/lib/itsm.sh` is the whole client, spliced into each Task's script by the chart
(a Pod cannot mount a ConfigMap from the catalog's namespace) and sourced as-is by the tests. Every call prints
one JSON object and nothing else:

```json
{"id": "<provider id>", "number": "CHG0012345", "url": "<link>", "state": "-2", "approval": "approved", "approved": true}
```

| Function | ServiceNow (Table API, `change_request`) | Webhook (`POST` the url, `action` in the body) |
|---|---|---|
| `itsm_create kind app env image release-id pr-url [rollback-of-change]` | `POST /api/now/table/change_request` with `type`, `short_description`, `description`, `category`, `correlation_id` (the release id), `correlation_display: glidepath`, `parent` for a rollback | `create` with `release: {kind, app, env, image, release_id, pr_url, rollback_of_change}` |
| `itsm_get id` | `GET .../change_request/<sys_id>`, or `?sysparm_query=number=CHG...` for a number | `get` with `change_id` |
| `itsm_link_pr id pr-url` | `PATCH` `work_notes` | `link` with `release.pr_url` |
| `itsm_implement id note` | `PATCH` `state: -1`, `work_notes` | `implement` with `note` |
| `itsm_note id note` | `PATCH` `work_notes` | `note` |
| `itsm_close id successful\|unsuccessful note [related]` | `PATCH` `state: 3`, `close_code`, `close_notes`, `work_notes` | `close` with `close_code`, `note`, `related_change_id` |
| `itsm_approved json` | `approval == approved`, or `state` in `approvedStates` | the adapter's own `approved` |

`approved` is the one field a gate decides on. A ServiceNow instance with a customised change model (states,
mandatory fields, an approval that must be requested first) is adapted here, in one place: the states through
`approvedStates`, anything else by editing the provider's few lines or by putting a `webhook` adapter in front
of the instance.

**Webhook adapter**: one endpoint, one JSON body per call as in the table, the same `Authorization` header
(bearer or basic from the Secret), answering the object above. `hack/lab/itsm_mock_server.py` is a reference
adapter and a ServiceNow look-alike in one file, and the lab's throwaway endpoint.

## Verifying it

- `charts/glidepath-catalog/tests/itsm_test.sh` (catalog CI, job `change-management`) runs the client for both
  providers against the mock, then the rendered `itsm-gate` and `itsm-change` scripts with a stub kubectl holding
  the release records, through the whole lifecycle: create, gate fails naming the change and its state, approve,
  gate passes, implement, failed outcome leaves it open, healthy closes it successful, a rollback closes the
  original unsuccessful with the emergency change related. It also checks the renderings: `none` is the stub
  with no lifecycle tasks, `servicenow` renders the gate and the hooks, `required: false` renders the advisory
  copy, and a mock that wants a bearer token refuses an unauthenticated call loudly.
- Live, in a lab: run the mock in the cluster. `hack/lab` is a kustomization (the Deployment, its Service, and
  a ConfigMap generated from the script), so a cluster repo points an Argo CD Application at it and a cluster
  rebuild brings the mock back (hangar-dev: `50-glidepath/itsm-mock`); point `itsm.webhook.url` at its Service, release, approve with `POST /_mock/approve/<id>`, `/retest itsm-`,
  merge, and read `changeRequestState` on the record as the relay moves it. The 2026-10-19 run (promote,
  gate red then green, implementing, closed successful; rollback with an emergency change, original re-closed
  unsuccessful) is recorded in the comments on
  [glidepath#140](https://github.com/jfillman/glidepath/pull/140).

## What a change request does not do

It does not pick the gate set, the environment or the image, and the gate never creates or moves a change: the
release flow decides what is released and the change tool decides whether it may be. A person who wants a
release held opens nothing in Glidepath; they leave the change unapproved. A person who wants a release past an
unapproved change is a break-glass merge, detected and announced (GATE-04).
