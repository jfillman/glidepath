# The contract, glidepath validate, and what an agent may change

Glidepath's contract is one generated file, [`contract/glidepath-contract.json`](../../contract/glidepath-contract.json),
that says everything a client needs to use Glidepath without reading its source: every `cicd.yaml`
field and who may change it, the release gates, the fixed state vocabularies, and the failures
Glidepath knows by name. People, Tower and agents read the same file. It is the first phase of the
[Glidepath API spec](https://hangarplatform.dev/library/glidepath-api/), and it follows the pattern
Airframe's contract set.

## What's in it

| Section | What it says | Source |
|---|---|---|
| `cicd.fields` | Every `cicd.yaml` field: type, enum, default, description, plus `owner`, `risk` and `agentScope` | [`schemas/cicd.schema.json`](../../schemas/cicd.schema.json) + [`schemas/cicd.meta.yaml`](../../schemas/cicd.meta.yaml) |
| `scope` | The rules an agent's change is checked against (below) | `schemas/cicd.meta.yaml` |
| `gates` | Each release gate: what it checks, `required` or advisory, `real` or `stub` | `releaseGuardrails` in [`charts/glidepath-catalog/values.yaml`](../../charts/glidepath-catalog/values.yaml) ([release-guardrails.md](release-guardrails.md)) |
| `states` | The run states and the release states, with their meanings | `schemas/cicd.meta.yaml` ([release-state.md](release-state.md)) |
| `knownFailures` | Failures with a stable id, the log line they match, a hint and, when there is one, the field to change | `schemas/cicd.meta.yaml` |
| `api` | Every API operation and its tier, once the API exists | `api/openapi.yaml` |

Regenerate it after changing any source, and commit the result:

```shell
python3 tools/gen_contract.py          # write it
python3 tools/gen_contract.py --check  # what CI runs (contract-ci.yaml)
```

The generator fails if a schema field matches no pattern in `cicd.meta.yaml`, so a new field can't
ship until someone has decided whether an agent may touch it.

`cicd.meta.yaml` is not read by any pipeline. The pipeline still validates `cicd.yaml` against the
schema baked into the toolbox image, so a change to the sidecar needs no toolbox rebuild. A change
to `cicd.schema.json` still does.

## glidepath validate

`tools/glidepath-validate` checks a `cicd.yaml` before it reaches a pipeline, in one pass:

```shell
tools/glidepath-validate cicd.yaml
tools/glidepath-validate cicd.yaml --format json
tools/glidepath-validate cicd.yaml --agent --base-ref origin/main   # also the GP-SCOPE rules
```

| Rule | Severity | What it catches |
|---|---|---|
| `GP-YAML-001` | error | Not YAML, or not a mapping |
| `GP-SCHEMA-001` | error | An unknown key, with the closest real one |
| `GP-SCHEMA-002` | error | A wrong type or value |
| `GP-ENV-001` | warning | An environment named like a pipeline stage |
| `GP-ENV-002` | error | Two environments with the same name |
| `GP-ENV-003` | error | A Ground environment sets `cluster` |
| `GP-ENV-004` | error | `production: true` on a Ground environment |
| `GP-ENV-005` | error | `deploy.lowerEnvironments`, `upperEnvironments` or `promotionOrder`, retired by `deploy.environments` |
| `GP-PIPE-001` | error | A deploy or release step names an environment `deploy.environments` doesn't list |
| `GP-PIPE-002` | error | A release step names a Ground environment |
| `GP-PIPE-003` | error | A flow has more than one check-ride step |
| `GP-PIPE-004` | error | A check-ride step sets `testName` |
| `GP-CHECKS-001` | error | A checks-only file (no `build`) sets `pipelines`, `test`, `deploy`, `api` or `ephemeralEnvironments` |

The `GP-ENV`, `GP-PIPE` and `GP-CHECKS` rules are constraints the pipeline already enforces when it runs
([cicd-yaml-reference.md](../user/cicd-yaml-reference.md)). Here they fail in seconds instead of
partway through a flow. Exit 0 means no errors, 1 means at least one, 2 means bad usage. It needs
python3, pyyaml and jsonschema.

## What an agent may change

An agent changes `cicd.yaml` only through a pull request that a human merges. On top of that,
every field has an `agentScope`:

| Scope | Meaning | Fields |
|---|---|---|
| `open` | An agent may change it | `build`, `test`, `ephemeralEnvironments`, `notifications`, `deploy.strategy`, Ground entries in `deploy.environments`, `pipelines` |
| `tighten` | Only in the stricter direction | `governance.sast`, `imageScan`, `policyCheck`, `sbom` (off to on), `governance.githubMerge.mode` (`""` < `merger` < `strict`), `githubMerge.requiredApprovals` (up), a test step's `required` (false to true) |
| `propose` | Only in a PR that a person must merge ([below](#propose-what-an-agent-may-change-only-in-a-pull-request-a-person-merges)) | A Flight environment added or changed in `deploy.environments`, the release step into one, a **new** environment's `cluster` and `production` |
| `human` | Never in an agent's PR | `secrets`, `governance.allowedCommitSigners`, `deploy.target`, `deploy.releaseFile`, `deploy.chart`, the cloud blocks (`ecs`, `lambda`, `azureContainerApps`, globally or per environment), an **existing** environment's `cluster` and `production`, `api`, a step's `cluster`, `gitopsRepo` and `manifestPath`, removing a Flight environment, any tier change |

The rule behind the split is that an agent never edits a control that constrains it. Secrets,
signers, cloud deploy identity and the gates are those controls. A build flag isn't.

Three rules check an agent's change against the file it started from:

| Rule | What it refuses |
|---|---|
| `GP-SCOPE-001` | A `human` field added, removed or changed |
| `GP-SCOPE-002` | A `tighten` field moved the loose way (an unset field counts as its default), or a flow that lost its check-ride step or one of its required test steps |
| `GP-SCOPE-003` | A Flight environment removed, or any environment's tier changed. (Adding or changing one is `propose`: not refused, listed as `needsPerson`.) |

Environments are compared by name, so reordering them is not a change. Pipeline steps are matched
by content first, so inserting a step doesn't make every later step look changed.

The rules are [`tools/cicd_scope.py`](../../tools/cicd_scope.py), standard library only.

### `propose`: what an agent may change only in a pull request a person merges

Contract version 2 (2026-11-12, owner decision: an agent asked to configure an app configures every environment the
person names, and a person approves anything that touches prod). An agent may add or change a Flight environment, the
release step into it, and (in the app's gitops repo) the Flight values an AppSpec sets, but the change is a
*proposal*: the pull request that carries it must be merged by a person.

What is `propose`, and only this (deny by default: every other field keeps its class):

| Change | Why it is propose, and what is not |
|---|---|
| A Flight environment **added** or **changed** (`/deploy/environments/<name>`) | The agent declares `{name, tier: flight, cluster, production?}`. Its `chart`, `ecs`, `lambda`, `azureContainerApps` stay `human` (GP-SCOPE-001): they are the deploy identity. |
| A **new** environment's `cluster` and `production` | Declaring prod on `hangar-prod` needs both. They are `propose` with `proposeOn: [added]`: on an environment that already exists, changing or removing them stays `human`, because that re-homes or re-classifies something already serving, which is a person's decision, not a proposal. |
| A release step into a Flight environment, added or changed | The `release` step with `env:` naming a Flight environment. A step's `cluster`, `gitopsRepo` and `manifestPath` stay `human`. |
| Removing a Flight environment, any `tier` change | Refused: GP-SCOPE-003. |

Never `propose`, whatever the change: `secrets`, `governance.allowedCommitSigners`, the gates (`tighten` rules), `deploy.target`,
`deploy.chart`, the cloud blocks, `api`, `gitopsRepo`, `manifestPath`. An agent never edits a control that constrains it.

The shape. A rule's `agentScope` may be `propose`, with an optional `proposeOn` (`added`, `changed`, `removed`): the kinds
of change that are propose; any other kind of change to that field reads `human`. The validator keeps its two answers
apart: `problems` is what is refused, and `needsPerson` (`[{pointer, change, message}]`) is what is allowed only for a
person's merge. A change with a problem is refused whole, whatever it also lists. The `cicd-validate-` check on an agent's PR
passes such a change and prints `needs a person to merge: <pointers>`; `glidepath validate --agent` and
`POST /v1/apps/{app}/cicd/validations` say the same (`needsPerson`). `contractVersion` is 2 because a consumer that does not
know the class would read an unknown `agentScope`: Clearance's policy bundle names the versions it is written for and
stops the core on any other, so roll out in the order [below](#rolling-out-propose).

**How "a person must merge" is enforced**, given there are no GitHub rulesets (private repos on the free plan, so no branch
protection can require anything). Four layers, none of which is a ruleset:

1. *Nothing an agent can call merges.* The agents' tools open a branch and a PR and nothing else (`airframe.apply`'s adapter
   sends only GETs and the POSTs that build a PR; Autopilot's tests read every request a run makes). There is no merge, review
   or push-to-existing-branch tool.
2. *Marked at the source.* The tools that carry a propose change (`glidepath.cicd.propose`, `airframe.apply`) mark the
   PR human-merge: a `Hangar-Merge: person` commit trailer and, in Clearance's receipt for the call, `merge: person` and the
   `needsPerson` paths. The generic file tool, `repo.pr.open`, cannot carry one: Clearance's R021 refuses a propose field
   there, because that tool has no way to mark the PR. The mark is made by the tool, never by the agent.
3. *Clearance's `Clearance / agent-scope` check* on such a PR is **neutral**, titled `needs a person: <paths>`, never
   success, until a merge. When the PR is merged by a login that is not a bot (GitHub's account type) and not one of the
   agents' logins, Clearance posts success; when it is merged by a bot or an agents' login, it posts failure (AS005).
4. *Glidepath's readers do not treat that neutral as success.* Since C-7 a neutral Clearance check reads as success (the
   `unattested` warning). The needs-a-person neutral is the exception: the promotion fan-in, `PROM-03`,
   `wait-for-release-guardrails`, the release log and the release PR's governance table keep it as neutral, so a commit
   whose agent-scope is still "needs a person" is held, and a failure after a bot's merge holds it too. `detect-bypass-merge`
   (every gitops repo PR close) reads it as success only when `merged_by` is a person (type `User`, not a `[bot]` login, not in
   `agentGates.agentLogins`); otherwise the merge is reported as a break-glass bypass (Slack, the release log's `bypass`).
   **App repos too:** every app repo gets `.tekton/push-propose-merge-check.yaml` (onboarding, like the agent-release file), a push-to-main
   run of the `propose-merge-check` Pipeline: `resolve-merged-pr`, then `detect-propose-merge`. It reads the merged PR's head commit for the
   `Hangar-Merge: person` trailer (or Clearance's agent-scope check titled "needs a person") and, if the merger is a bot, an agents'
   login, an account that is not a `User`, or unnamed, alerts Slack (always, not silenceable) naming the PR and the merger. A person's merge
   and every unmarked PR are quiet; the run never fails.

Layers 3 and 4 need the Clearance GitHub App's checks (`agentGates.clearanceChecks: true`). On a cluster in the fallback
(`agent-gates-`), layers 1 and 2 hold and the merge is not checked afterwards.

What this does not do: GitHub still lets any collaborator with write access press merge (an agents' App token has pull-request
write). The controls are that no tool presses it, that a merge by anything but a person turns a required check red and
alerts, and that the release path refuses to carry it on. A repo on a paid plan can additionally require the Clearance
checks and a review in a ruleset ([agent-gates.md](agent-gates.md#making-it-block)). App repos have the same detective check
(`detect-propose-merge`) as gitops repos, so a bot's merge there is alerted too, besides Clearance's check turning red.

#### Rolling out `propose`

Clearance first (its policy engine learns `propose`, the check learns `needs-person`), then Autopilot (its bundle names
contract version 2, the pinned Clearance is the new one, the tools mark PRs), then Glidepath (the contract becomes version 2, the
`cicd-validate` image and the catalog with the new readers). Glidepath last: while its contract is still version 1, an
agent's Flight proposal is refused (GP-SCOPE-003 as before), so the new Clearance and Autopilot are inert until it ships.

Clearance applies the same rules before an agent opens a pull request, and vendors nothing (Clearance
v0.29.0, R-5). Hangar's policy bundle (autopilot `policy/hangar/bundle.json`) reads the contract's
`scope.rules` live from glidepath-api's `GET /v1/contract` when the Clearance core starts, and checks
`contractVersion` against the versions the bundle is written for. So:

- **A change to the pointer rules** (`scope.rules`: a field moved between `open`, `tighten` and `human`, a
  new order or default) reaches the gate an agent meets when the Clearance core next restarts. No
  Clearance release, no Autopilot pin.
- **A `contractVersion` bump** stops the Clearance core from starting until Hangar's bundle names the new
  version (an Autopilot change to `policy/hangar/bundle.json`, deployed by Apron). Bump it only when the
  scope's shape changes (version 2: the `propose` class and `proposeOn`).
- **A change to the structural rules** (`GP-SCOPE-002` check rides and required tests, `GP-SCOPE-003`
  removing a Flight environment and tiers; adding or changing a Flight environment is `propose`, which the bundle states as the
  `keyed` invariant's protected change, refused on the generic file path) is code in `cicd_scope.py`, and the bundle states the same rules as
  data (its `keyed` and `counted` invariants). Change both: the bundle is in autopilot, not Clearance.

The same rules answer `POST /v1/apps/{app}/cicd/validations` on glidepath-api
([api.md](api.md#validate-a-cicdyaml-change)): a caller sends a patch or the whole file and gets the
verdict the check will give, plus every field the change touches, before opening the pull request.

### Enforced in app pull requests: the `cicd-validate` check

Since 2026-10-31 every onboarded app repo runs `glidepath validate` as a pull-request check,
**`Glidepath / cicd-validate-`** (`.tekton/pull-request-cicd-validate.yaml`, delivered by
onboarding like the agent gates' file; Pipeline `cicd-validate-check`, Task `cicd-validate`). Before it,
a broken `cicd.yaml` was only caught by `stage-preflight`, on the first build after the merge.

- It runs on every PR to `main`, a person's too, because a required check that only ran when
  `cicd.yaml` changed would block every other PR. A PR that leaves `cicd.yaml` alone passes in seconds
  as "unchanged", with no comment.
- When the PR changes `cicd.yaml`, the check reads the file at the PR head through the platform's
  GitHub App token and runs the schema, `GP-ENV` and `GP-PIPE` rules. It fails on any error and posts
  the findings as a PR comment, each with its rule, field and fix.
- An agent's PR (authored by an `agentGates.agentLogins` login, or carrying `Hangar-*` commit
  trailers) also meets the `GP-SCOPE` rules, against the file at the PR's merge base (not the base
  branch's tip, so `main` moving on is not read as the agent's change). A PR that adds the first
  `cicd.yaml` is compared with an empty file, so every field counts as added. Whether the PR really is
  the agent's is the [agent gates'](agent-gates.md) question; this check applies the stricter rules to
  either claim.
- A PR that removes `cicd.yaml` fails: every app repo needs one at its root.

The validator runs from the `glidepath-validate` image (`tools/validate.Containerfile`: the validator,
`cicd_scope.py`, the schema and the contract of one commit; `validate-image-ci` publishes
`ghcr.io/<owner>/glidepath-validate:sha-<short>` on every change and sets `cicdValidatorImage` in the
catalog values). The toolbox image stays bash-only. The check blocks a merge only as a required status
check: `hack/apply-agent-gates-ruleset.sh [-f <values>] <owner>/<repo>` adds it to the app repo's ruleset, once the
repo has the `.tekton` file (a required check nothing reports would block every PR).

Clearance still runs the scope rules before an agent opens the PR (R021), so the agent learns of a
refusal before anything exists on GitHub; the check is what holds whoever opened the PR, however they
opened it.
