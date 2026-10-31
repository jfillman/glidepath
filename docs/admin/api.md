# glidepath-api

`glidepath-api` answers questions about an app's CI/CD for people, Tower and agents: its runs,
their logs, why a run failed, its release records, the supply-chain evidence on its images, its test
runs, and Glidepath's [contract](contract.md); it can [re-run and cancel](#writes) an app's
PipelineRuns; and it can [propose a promotion or a rollback](#promote-and-roll-back) of a Flight environment.
Phases 1 and 2 of the [Glidepath API spec](https://hangarplatform.dev/library/glidepath-api/) are the reads
(tier T0). Phase 3's first writes (tier T1) are re-run and cancel; a `cicd.yaml` change is
[validated](#validate-a-cicdyaml-change) here before anyone opens the pull request (API 0.7.0), and previews are
still to come. Phase 4 (tier T2, propose) is promote and roll back: each only starts the app's own `release`
Pipeline, whose gitops PR a human merges. Nothing else is written. Phase 5 starts with the [event stream](#events):
every CDEvent the shared broker carries, re-verified and served as server-sent events with resume. Phase 6b adds the
[live reads](#live-and-archived-reads) Tower's views of runs in flight need, so Tower no longer reads Tekton or pod
logs through its own Kubernetes proxy, and the fleet's run history across every namespace, and (API 0.6.0) the
fleet's [release records and release Events](#the-fleets-release-records) for Tower's Ops Wall and drift view.

**Since phase 6 (2026-10-30) this API is Tower's only CI/CD path.** Tower's backend asks it for every run, log,
app release record, piece of evidence and test run, and sends every re-run, cancel, Flight promote and rollback
through it, for apps and InfraServices alike. There is no fallback: an API error or timeout goes back to the person
(a 4xx keeps its status, anything else is a 502). Tower no longer reads Tekton Results, Testkube or Rekor itself,
verifies no signatures of its own, and its identities can't create or patch a PipelineRun (the cluster refuses it,
[below](#who-may-write)). The Ops Wall's fleet release records and release Events, the last reads Tower made through
its Kubernetes proxy, are served from API 0.6.0 (`GET /v1/releases`, `GET /v1/releases/events`); until Tower's
side ships, Tower still reads the `release-tracking` ConfigMaps and Events itself.

**Not offered, by design:** promoting to a Ground environment and cloud pins. Both are a commit to a repo
(Tower's Ground promote commits straight to the app repo's release file; a pin opens a PR itself), so both
need a GitHub write identity, and the API holds none. They stay in Tower.

The operations are [`api/openapi.yaml`](../../api/openapi.yaml). The contract lists every
operation with its tier, and [`tools/test_glidepath_validate.py`](../../tools/test_glidepath_validate.py)
fails if the server's routes and the spec ever differ.

| Call | Tier | Needs | Returns |
|---|---|---|---|
| `GET /v1/whoami` | T0 | a token | Who the API thinks you are |
| `GET /v1/contract` | T0 | a token | `contract/glidepath-contract.json` |
| `GET /v1/apps/{app}/runs?sinceHours=&limit=&view=` | T0 | `list runs` | Runs newest first; `view=full` adds each PipelineRun and TaskRuns |
| `GET /v1/apps/{app}/runs/{result}` | T0 | `get runs` | One run (the id is the PipelineRun's uid) |
| `GET /v1/apps/{app}/runs/{result}/logs/{taskRun}` | T0 | `get runs/logs` | One TaskRun's step logs, as text |
| `GET /v1/apps/{app}/runs/{result}/explain` | T0 | `get runs/logs` | Each failed TaskRun's failing step, its [known failure](#explain) id, hint and field, and the line that matched |
| `GET /v1/apps/{app}/live` | T0 | `list runs` | Every [live](#live-and-archived-reads) PipelineRun and TaskRun in the app's namespace, as the cluster lists them |
| `GET /v1/apps/{app}/live/taskruns/{taskRun}/logs/{container}` | T0 | `get runs/logs` | One container's log from a live TaskRun's pod, with timestamps, as text |
| `GET /v1/runs?app=&sinceHours=&limit=` | T0 | `list runs` (cluster-wide without `app=`) | Finished runs across every namespace, newest first, each with `namespace` and `app` |
| `GET /v1/live?app=` | T0 | `list runs` (cluster-wide without `app=`) | Every live PipelineRun in the cluster |
| `GET /v1/live/taskruns?app=&pipelineRuns=` | T0 | `list runs` (cluster-wide without `app=`) | The live TaskRuns of the named PipelineRuns |
| `GET /v1/releases?app=` | T0 | `list releases` (cluster-wide without `app=`) | Every [release record](#the-fleets-release-records) in the cluster, each with `namespace` and `labels` |
| `GET /v1/releases/events?app=` | T0 | `list releases` (cluster-wide without `app=`) | The `ReleaseStalled` and `ReleaseDrift` Events about release records |
| `GET /v1/apps/{app}/releases` | T0 | `list releases` | Every `release-tracking-*` record of the app |
| `GET /v1/apps/{app}/releases/{chainId}` | T0 | `get releases` | One record |
| `GET /v1/apps/{app}/evidence?image=` | T0 | `get evidence` | Every signature and attestation on the image, each [re-verified](#evidence) |
| `GET /v1/apps/{app}/tests?limit=` | T0 | `list tests` | The app's [test runs](#tests) from Testkube, newest first |
| `GET /v1/apps/{app}/tests/{id}/report` | T0 | `get tests` | A run's JUnit report, as XML |
| `POST /v1/apps/{app}/pipelineruns/{name}/rerun` | T1 | `rerun runs` | [Re-run](#re-run) a PipelineRun, once per `Idempotency-Key` |
| `POST /v1/apps/{app}/pipelineruns/{name}/cancel` | T1 | `cancel runs` | [Cancel](#cancel) a PipelineRun, or say it already finished |
| `GET /v1/apps/{app}/environments` | T0 | `list environments` | The app's [release plan](#release-plan): environments, tiers, promotion order |
| `GET /v1/apps/{app}/environments/{env}/rollback-plan?cluster=` | T0 | `get environments` | The environment's current release and its eligible [rollback targets](#rollback-plan) |
| `POST /v1/apps/{app}/environments/{env}/promote` | T2 | `promote environments` | [Promote](#promote) to a Flight environment: start the release run, once per `Idempotency-Key` |
| `POST /v1/apps/{app}/environments/{env}/rollback` | T2 | `rollback environments` | [Roll back](#roll-back) a Flight environment to a plan target, once per `Idempotency-Key` |
| `GET /v1/events?app=&type=&since=` | T0 | `list events` | The [CDEvents stream](#events), as server-sent events, resumable with `Last-Event-ID` |
| `GET /v1/events/recent?app=&type=&since=&limit=` | T0 | `list events` | The same events from the buffer, as JSON |

Runs come from Tekton Results, so they cover 30 days and never go back past the creation of the
app's current `-cicd` namespace (an app deleted and recreated under the same name doesn't inherit
the old one's runs). The shapes are what Tower's backend served itself (runs since 2026-10-08, evidence
and tests since 2026-10-28), so Tower's views take these answers unchanged; until phase 6 (2026-10-30) Tower
compared its own answers against these field for field.

## Where it runs

`charts/glidepath-control-plane/templates/api/glidepath-api.yaml`: two replicas in
`platform-system` on the control-plane cluster, Service `glidepath-api:8080`. It renders nothing
until `apiImage` is set; `api-ci.yaml` builds a multi-arch image on every merge to main that
touches the API or the contract and sets `apiImage` to its `sha-` tag, the same loop the broker
images use. The contract is baked into the image, so `GET /v1/contract` always matches the code.

Tekton Results is read through `tekton-results-relay`, the hop Tower used itself until phase 6 (2026-10-30). The
relay holds the Results read grant; the API decides who may ask before it asks.

Each replica caches what it reads in memory (nothing persisted; Results stays the record). A
finished run never changes, so it is kept 7 days; run lists for a minute, step logs for an hour. A
cold `view=full` read of a busy app's window is several MB from Results and can take longer than a
caller waits (Tower gives up at 10 s), so a cache-filling read is detached from the request: it
finishes within 90 s and fills the cache even when its caller has gone, and concurrent callers
asking for the same thing share one read. At start-up each replica reads every app's last 7 days of runs in the
background, one app at a time (`cmd/glidepath-api/warm.go`; it needs `list` on namespaces to find them), so its first
answers after a restart or a deploy are cached ones.

## Calling it

In the cluster, send a ServiceAccount token:

```shell
curl -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
  http://glidepath-api.platform-system.svc.cluster.local:8080/v1/apps/boarding-api/runs
```

From outside, go through the API server's service proxy. The API server authenticates you and then
drops `Authorization` before forwarding (a Kubernetes limit, the one `tekton-results-relay` exists
for), so send the token again in `X-Glidepath-Authorization`:

```shell
TOKEN=$(kubectl create token <sa> -n <ns>)
APISERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
curl --cacert <the cluster's CA file> -H "Authorization: Bearer $TOKEN" -H "X-Glidepath-Authorization: Bearer $TOKEN" \
  "$APISERVER/api/v1/namespaces/platform-system/services/glidepath-api:8080/proxy/v1/whoami"
```

The caller needs `services/proxy` on `glidepath-api` for that path, and the API then checks the
token itself, so the proxy grant alone reads nothing.

## Who may read what

**Who you are** is a TokenReview by the cluster, so anything the API server trusts works: an
in-cluster ServiceAccount, or a federated identity from another cluster (Tower's identities from
the prod cluster arrive as `hangar:<cluster>:<serviceaccount>`).

**What you may read** is a SubjectAccessReview in the app's `-cicd` namespace, on a group nothing
serves: `glidepath.hangar.io`, resources `runs`, `runs/logs`, `releases`, `evidence`, `tests`, `environments`,
`events` and `cicd` (verb `validate`). RBAC
only needs the strings, so access is ordinary RBAC:

- `api.readers` binds the chart's `glidepath-api-reader` ClusterRole cluster-wide (users, groups,
  ServiceAccounts). Empty by default: a fresh install grants nobody.
- To let one identity read one app, bind the same ClusterRole with a RoleBinding in that app's
  `-cicd` namespace.

An app that doesn't exist answers 403 to a caller who couldn't read it if it did, and 404 only to
one who could, so the API doesn't tell strangers which apps exist.

**On behalf of.** A delegate, such as Tower, may name the person it acts for in
`X-Glidepath-On-Behalf-Of`. Only usernames in `api.delegates` may send it; anyone else gets
`GP-AUTH-003`. Every call, read or write, is authorized as the caller itself; the person is recorded in
the audit line. A delegate's write must name the person ([who may write](#who-may-write)).

## Writes

The T1 writes, both on a live PipelineRun in the app's `-cicd` namespace. The namespace always comes from
the app in the path (the same resolver the reads use), never from the request. They replaced Tower's
own Re-run and Cancel (backstage `packages/backend/src/tektonActions.ts`, removed in phase 6, 2026-10-30) and build
the same objects; Tower's Re-run and Cancel now call them.

### Re-run

`POST /v1/apps/{app}/pipelineruns/{name}/rerun` with an `Idempotency-Key` header. It reads the live
run `name` and creates a copy, exactly as Tower's `rerunBody` did: the same `spec` without
`spec.status` (re-running a cancelled run must not start it cancelled), and only the
`hangar.io/app`/`hangar.io/flow` labels (or the pre-rebrand `platform.io/` pair), so the Pipelines-as-Code
annotations and trigger label are dropped and PaC never reports a commit status for it. Tekton has no
restart; this is `tkn pipelinerun start --use-pipelinerun`.

Two things differ from Tower's body, both for idempotency: the run has a `name` instead of a
`generateName`, and two annotations.

```json
{"metadata": {"name": "ci-0-build-abc-rerun-3f9a1",
  "annotations": {"glidepath.hangar.io/rerun-of": "ci-0-build-abc",
                  "glidepath.hangar.io/idempotency-key-sha256": "3f9a1c..."}}}
```

**Idempotency.** "A re-run creates exactly one PipelineRun" holds across retries and both replicas
with no shared memory: the API server is the lock. The name is `<name>-rerun-` (cut at 58 characters,
as the API server cuts a `generateName` prefix) plus the first 5 hex characters of
`sha256(app + "/" + name + "/" + key)`, so it is as long as the name Tower's `generateName` got. A
second create with the same key fails with AlreadyExists; the API reads the run that holds the name and
answers it as a replay when its annotations say it is the re-run of `name` with this key's full hash,
and `GP-API-409` otherwise (another key that shares the 5 characters, one in a million, or any other
object). A retry after the source was pruned (finished runs go about an hour after completion) still
answers with the run it made. The key is the caller's: 1 to 128 characters of `[A-Za-z0-9._:-]`, the
same on every retry of one re-run and new for the next. A missing or malformed key is `GP-API-400`.

| Answer | Status | Body |
|---|---|---|
| Created | 201 | `{"name": "<new>", "replayed": false}` |
| The same key again | 200 | `{"name": "<new>", "replayed": true}` |
| Name taken by something else | 409 | `GP-API-409` |
| No such run (and no re-run for this key) | 404 | `GP-API-404` |

### Cancel

`POST /v1/apps/{app}/pipelineruns/{name}/cancel` sends the JSON merge patch
`{"spec":{"status":"CancelledRunFinally"}}`, the status Tower's own Cancel sent: the pipeline's `finally`
tasks still run (notify-backstage, notify-slack), so a cancel is announced like a failure. It reads the
run first, and a run that needs nothing is answered, not refused, so a retry is always safe:

| Run | Status | Body |
|---|---|---|
| Running or pending | 200 | `{"name": "...", "status": "cancelled"}` (patched) |
| `spec.status` already `Cancelled` or `CancelledRunFinally`, or its `Succeeded` condition reason is a cancel | 200 | `"status": "already-cancelled"` (not patched) |
| `Succeeded` condition `True` or `False` | 200 | `"status": "already-finished"` (not patched) |
| No such run | 404 | `GP-API-404` |

A run stopping gracefully (`StoppedRunFinally`) is still cancelled. A run that finishes between the
read and the patch answers `cancelled`; Tekton ignores the status on a finished run.

### Dry run

`?dryRun=true` on either does every read and check, then the create or patch as a Kubernetes
server-side dry run (`dryRun: All`): admission runs, including the policy below, and nothing is
stored. The answer is 200 and carries what a real call would write (how Tower compared its own body with the
API's before its writes moved here):

```json
{"name": "ci-0-build-abc-rerun-3f9a1", "replayed": false, "dryRun": true, "object": {"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "metadata": {...}, "spec": {...}}}
{"name": "ci-0-build-abc", "status": "cancelled", "dryRun": true, "patch": {"spec": {"status": "CancelledRunFinally"}}}
```

A dry run of a key already used answers the replay, with the run it found as `object`. A dry run is
audited with `"dryRun": true`.

Through the API server's service proxy (how Tower calls), ask with the header `X-Glidepath-Dry-Run: true`
instead: the API server refuses a proxied request with `dryRun` in its query ("dryRun is not supported").
Query and header may both be given only if they agree.

### Who may write

**Authorization is a SubjectAccessReview on the caller**, as for reads: `rerun` or `cancel` on
`runs.glidepath.hangar.io` in the app's namespace. The chart's `glidepath-api-operator` ClusterRole holds
both verbs; `api.operators` binds it cluster-wide (users, groups, ServiceAccounts, empty by default), and
a RoleBinding to it in one app's `-cicd` namespace grants that app alone. It holds no read, and read
access alone is `GP-AUTHZ-001` on a write.

**Both identities are on every write.** When the caller is a delegate (Tower), the write must carry
`X-Glidepath-On-Behalf-Of`, or it is refused with `GP-AUTH-004` before anything is read. The person is
**not** authorized by the API: they are a Backstage identity, not a Kubernetes one, so no
SubjectAccessReview can be asked about them. The delegate is trusted to have checked the person, which
Tower does with its owner permission before it calls. The API authorizes the delegate, and records the
person. So `api.operators` should list a delegate only when that delegate does its own per-person
check.

**The cluster's shape lock.** glidepath-api's own ServiceAccount holds `get`, `create` and `patch` on
`tekton.dev` `pipelineruns` cluster-wide (apps are onboarded after install), which RBAC can't narrow to
Glidepath's shape. The admission policy `glidepath-api-pipelinerun-shape` (`tower-pipelinerun-shape` until phase 6,
2026-10-30; gitops `30-policy/kyverno-policies/tower-identity-writes.yaml`, rendered by Apron) holds
`system:serviceaccount:platform-system:glidepath-api` to it: an app's or InfraService's `-cicd` namespace
(`^(app|infra)-[a-z0-9-]+-cicd$`), a cluster-resolver `pipelineRef` into `platform-catalog`, SA `pipeline-runner`, no
inline spec, no `taskRunSpecs`, a hostAliases-only podTemplate, `emptyDir` or `volumeClaimTemplate` workspaces, no
PaC annotations, and an update may only set `spec.status` to `Cancelled`, `CancelledRunFinally` or
`StoppedRunFinally`. It also matches Tower's writer identity (`tower-actor`), only to refuse any PipelineRun write
from it: Tower writes PipelineRuns only through this API. A write the cluster refuses, there or in RBAC, is
`GP-API-422` and is not worth retrying.

## Promote and roll back

Tier T2, propose. Both writes do one thing in the cluster: create the app's `release` PipelineRun from the
TriggerTemplate glidepath-app renders into its `-cicd` namespace (`promote-release` or `rollback-release`), the
run Tower's Promote and Roll back start today. That pipeline's gates run, its `open-release-pr` opens the gitops PR
with the platform's GitHub App and writes the release record, and **a human merges the PR**. The API cannot merge,
push, or open a PR: its GitHub token reads only ([below](#the-github-read-token)). So an agent can propose a
staging promotion and watch the release record reach `healthy` after a person merges it, and has no way to merge
it itself.

They port Tower's (backstage `packages/backend/src/glidepathPromote.ts`, `glidepathReleaseRun.ts`,
`glidepathRollback.ts`, `releasePlan.ts`; Tower's copies were removed in phase 6, 2026-10-30, and its Promote and
Roll back to a Flight environment call these), with three differences: a Ground target is refused, the run's name
comes from the `Idempotency-Key`, and a rollback does not wait for its PR.

### Release plan

`GET /v1/apps/{app}/environments` is Tower's `fetchReleasePlan`: the app's `cicd.yaml` read from GitHub (the
source repo's, or an InfraService's `gitops-infra-<name>`), and the same JSON:

```json
{"order": ["dev", "staging", "prod"], "orderSource": "promotionOrder", "lower": ["dev"],
 "upper": [{"name": "staging", "cluster": "hangar-prod"}, {"name": "prod", "cluster": "hangar-prod", "production": true}]}
```

`order` is `deploy.environments` in order when every entry is Ground or Flight (`promotionOrder`); otherwise the
order of the deploy/release steps of whichever pipeline has the most of them, the first-defined winning a tie
(`pipeline-steps`). `lower` is Ground, `upper` Flight. `releaseFile` appears when `deploy.releaseFile` is set. An
app with no `cicd.yaml` yet gets an empty plan, not an error. A `cicd.yaml` with a duplicate key is refused.

### Rollback plan

`GET /v1/apps/{app}/environments/{env}/rollback-plan?cluster=<cluster>` is Tower's `fetchRollbackPlan`, byte for
byte: from the release records labelled with this app, environment and cluster, `current` is the newest that ran
(states progressing, healthy, aborted, degraded, sync-failed; by `prCreatedAt`), and `targets` the newest five that
reached `healthy` (by `healthyAt`), less records written before ADR-0021 phase 4, the current release, its image,
and repeats of an image, newest first. This is the window the release gates check.

```json
{"current": {"releaseId": "rel-3", "image": "ghcr.io/o/shop:1.2.0-3333333", "state": "healthy", "stateAt": "..."},
 "targets": [{"releaseId": "rel-2", "image": "ghcr.io/o/shop:1.1.0-2222222", "gitRevision": "2222222...",
              "gitUrl": "https://github.com/o/shop", "healthyAt": "...", "state": "superseded"}]}
```

`current` is absent when nothing has run there, and a field the record lacks is absent, as in Tower's.

### Promote

`POST /v1/apps/{app}/environments/{env}/promote` with an `Idempotency-Key` and the body Tower's `/promote` takes,
less what the path says:

```json
{"cluster": "hangar-prod", "sourceEnv": "dev", "sourceCluster": "hangar-dev"}
{"cluster": "hangar-prod", "sourceImageRepo": "ghcr.io/o/shop", "sourceImageTag": "1.0.0-abc1234"}
```

Exactly one source pair, as Tower requires (`GP-API-400` otherwise). Then, in Tower's order:

1. **Order.** The target must be the environment immediately after `sourceEnv` in the release plan (compared
   case-insensitively), or for a first deploy (the image pair) the app's first environment. Anything else is
   `GP-API-422`: one stage at a time.
2. **Flight only.** A Ground target is `GP-API-422`. Tower promotes to Ground with a direct commit to the app repo's
   `glidepath/envs/<env>.release.yaml`, authored as the deploy bot, which needs a GitHub write identity the API does
   not hold.
3. **Source**, resolved server-side exactly as Tower's `resolveSource` does, never from what the client or the
   cluster says is running. A Ground source: `glidepath/envs/<env>.release.yaml`'s `release.image`, then
   `glidepath/envs/<env>.yaml`'s `release.image`, then its `rollout.image`, from the app repo; the commit is the
   tag's trailing sha expanded against the app repo. A Flight source: `<cluster>/<env>/release.yaml` in
   `gitops-<app>`, then `values.yaml`, each `release.image` falling back to `rollout.image`; the commit is
   `releaseTracking.gitRevision` when the file still has one, else the tag's sha. A first deploy uses the client's
   image only once its tag's sha resolves to a real commit of the app repo. Nothing recorded is `GP-API-422`; a
   file with invalid YAML (a duplicate key) is an error naming the file.
4. **Dedupe.** If the PR from `release-<env>-<sha8>` (the branch `open-release-pr` uses) is open in `gitops-<app>`,
   the answer is that PR and nothing starts: `200 {"mode": "pr", "prUrl": "...", "alreadyOpen": true}`.
5. **Start.** The `promote-release` template's PipelineRun with Tower's params (`env`, `cluster`, `git-url`,
   `git-revision`, `image-ref`, `initiator`), created in the app's namespace:
   `201 {"mode": "pipeline", "pipelineRun": "promote-staging-abc1234d-3f9a1c"}`.

`initiator` is the person (`X-Glidepath-On-Behalf-Of`) when a delegate calls, else the caller's username. It lands
in the run's `hangar.io/initiator` annotation and the release PR, as Tower's does.

### Roll back

`POST /v1/apps/{app}/environments/{env}/rollback` with an `Idempotency-Key` and
`{"cluster": "...", "targetReleaseId": "...", "reason": "..."}` (`reason` required, trimmed, at most 1024
characters). Tower's `startRollback`: the rollback plan is computed for the environment and cluster; nothing
released there yet, or a `targetReleaseId` that is not one of its `targets`, is `GP-API-422`. If the PR from
`release-rollback-<env>-<sha8>` is open, the answer is it. Otherwise the `rollback-release` template's PipelineRun
starts with `rollback-of` the current release's id, `rollback-reason`, and `config-revision: main`:

```json
{"alreadyOpen": false, "pipelineRun": "rollback-prod-2222222b-7c01d2", "target": {...}, "replaces": "rel-3"}
```

Tower held its request up to 90 s for the run's PR; the API answers as soon as the run exists, and the PR
appears where Tower's release tracker already follows the run (`GET /release-run` accepts these names).

### Following a release run

`GET /v1/apps/{app}/releases/runs/{name}` (API 0.8.0, tier T0, `get` on `releases`) is the read that follows the run a
promote or rollback just started (its `pipelineRun`), from the moment of the click until the PR shows and beyond:

```json
{"name": "promote-prod-abc1234-0a1b2c", "kind": "release", "env": "prod", "cluster": "hangar-prod",
 "initiator": "user:default/jfillman", "status": "running", "startTime": "2026-10-31T10:00:00Z",
 "tasks": [{"name": "preflight", "taskRun": "...-preflight", "status": "succeeded", "startTime": "..."},
           {"name": "open-release-pr", "taskRun": "...-open-release-pr", "status": "running", "startTime": "..."}],
 "chainId": "7200a685-...", "prUrl": "", "changeRequest": "", "phase": "running", "record": null}
```

`tasks` is every task of the Pipeline in its own order (`status.pipelineSpec`), then its finally tasks, so one not
started yet is `pending` where it will run; `params` are the run's string params (`image-ref`, `git-url`,
`git-revision`, ...). The run and its TaskRuns come from the cluster (never cached: it is followed one run at a time); the chain id is
the preflight task's `chain-id` result, the PR and change request open-release-pr's `pr-url` and
`change-request-number` once it has them, and `record` the release record that chain id names
(`release-tracking-<chain-id>`). `phase` is the record's `state` as soon as there is one (`proposed`, `merged`,
`progressing`, `healthy`, ...), else the run's own: `running`, `failed`, `cancelled`, or `finished` (it succeeded
without writing a record). A PipelineRun that isn't a release run (`hangar.io/stage=release`) is `GP-API-400`. Once
Tekton has deleted the run, about an hour after it finished, it is `GP-API-404`: the record stays, and `GET
/v1/apps/{app}/releases` has it.

### Idempotency and dry run

As for a re-run: the run's name is `<promote|rollback>-<env>-<sha8>-` (Tower's `runName`) plus the first 6 hex
characters of `sha256(app + "/" + op + "/" + env + "/" + cluster + "/" + key)` in place of Tower's 3 random
bytes, cut at 63 characters, and the run carries `glidepath.hangar.io/idempotency-key-sha256` with the full hash.
The same key again hits AlreadyExists and answers `200` with `"replayed": true`; a name held by anything else is
`GP-API-409`. A retry after the PR opened answers the PR. `replayed` and `dryRun` are absent unless true, so the rest
of the answer compares field for field with Tower's `PromoteResult` and `RollbackResult`.

`?dryRun=true` or `X-Glidepath-Dry-Run: true` does every read and check, then the create as a server-side dry run
(admission, including `glidepath-api-pipelinerun-shape`, runs; nothing is stored), and answers `200` with the run as
`object`.

### The GitHub read token

The plan, the source and the dedupe read GitHub, and glidepath-api has no GitHub credential. For each request it
asks the broker (`cdevents-broker-auth`, `POST /github-read-token`, [release.md](release.md#the-github-read-token))
for a token, once per repo the request reads, authenticating with its own projected ServiceAccount token (audience
`github-read-token`, mounted at `/var/run/secrets/glidepath/broker/token`, read from disk on every mint). The broker
serves only the ServiceAccounts in `githubRead.callers` (default glidepath-api's), only for the app's own repos
(`<app>`, `gitops-<app>`, `gitops-infra-<app>`, each matched to a PaC Repository CR in the app's namespace and its
owner), and mints an installation token down-scoped to that one repository and to `contents`, `pull_requests` and
`metadata`, all read. The token lives for the request and is dropped with it: it is never cached, logged, returned,
or put in an error.

Each mint is a broker TokenReview and two GitHub calls. A promotion reads the gitops repo only after the plan and
the source, so it asks for that token at the start, alongside the plan's read; a rollback asks for it
while it reads the release records. A read that needs a token still being minted waits for that mint instead of
asking again, and a failed mint is not kept, so the request's next read of that repo asks again.

`GITHUB_OWNER` (the chart's `githubOwner`, from `platformCicdRepoUrl`) is the owner of every app repo; the API
never takes an owner from the request.

### Who may promote

`promote` and `rollback` on `environments.glidepath.hangar.io` in the app's namespace, held by the chart's
`glidepath-api-releaser` ClusterRole and bound cluster-wide by `api.releasers` (empty by default), or app by app with
a RoleBinding. It holds no read and no re-run: an operator is not a releaser, and a releaser that should read the
plans needs `glidepath-api-reader`. As for every write, a delegate must name the person (`GP-AUTH-004`) and the API
trusts the delegate's own check of them (Tower's owner permission).

glidepath-api's own ServiceAccount needs `get` on `triggertemplates.triggers.tekton.dev` (new) and the `create` on
`pipelineruns` and `list` on ConfigMaps it already holds. The runs are Glidepath's shape (a cluster-resolver
`pipelineRef` into `platform-catalog`, SA `pipeline-runner`, an `emptyDir` workspace, a hostAliases-only podTemplate,
no PaC annotations), so the [shape policy](#who-may-write) admits them unchanged (checked 2026-10-29 with a
server-side dry run on hangar-dev as `system:serviceaccount:platform-system:glidepath-api`). Since phase 6 it admits an
InfraService's `infra-<name>-cicd` namespace as well as an app's.

## Validate a cicd.yaml change

`POST /v1/apps/{app}/cicd/validations` (API 0.7.0, tier T0) says what a change to the app's `cicd.yaml` would mean,
before anyone opens the pull request: the verdict the
[`cicd-validate` check](contract.md#enforced-in-app-pull-requests-the-cicd-validate-check) will give that PR. It
needs `validate` on `cicd.glidepath.hangar.io` in the app's namespace, which `glidepath-api-reader` grants. It writes
nothing: a POST only because it carries a body.

Send exactly one of:

- `patch`: an RFC 7386 JSON merge patch on the file as it is (keys to set, `null` to delete), or
- `content`: the whole file after the change, as YAML text;

with `baseRef` (a branch, tag or commit; unset is the default branch) and `agent` (`true` also applies the
`GP-SCOPE` rules, what an agent may change). The API reads `cicd.yaml` from the app's repo (an InfraService's
gitops repo) at that ref with the [broker's read-only token](#the-github-read-token), applies the patch, and hands
the result and the base to the validator. The answer is 200 whether or not the result is valid:

```json
{"valid": false,
 "problems": [{"pointer": "/governance/sast", "rule": "GP-SCOPE-002", "severity": "error",
               "message": "/governance/sast loosened (true -> false)",
               "hint": "An agent may turn a gate on or make it stricter, never the other way."}],
 "changes": [{"pointer": "/governance/sast", "change": "changed", "before": true, "after": false, "agentScope": "tighten"}],
 "summary": "1 error(s): GP-SCOPE-002; 1 change(s): /governance/sast true -> false",
 "base": {"repo": "jfillman/gate-api", "ref": "HEAD", "sha": "7f3c…", "present": true},
 "resulting": "apiVersion: platform/v1\n…",
 "contractVersion": 1}
```

`changes` is every field the change touches, each with the contract's `agentScope`, so a caller can tell a person
what it is about to propose. A file missing at the base is compared with an empty one (every field counts as
added). The patch is applied to the file's own text (the YAML edited as a node tree, blank lines put back), so
`resulting` is the file as it was with only the patched lines changed: comments, key order and quoting stay, a
removed key goes with its comments, and a new key lands at the end of its mapping. A caller can commit it as it is.

**The validator is a sidecar.** The API image is distroless, and the rules are Python (`tools/glidepath-validate`
and `tools/cicd_scope.py`, the files Clearance vendors and the PR check runs). The chart runs the
`glidepath-validate` image (`cicdValidatorImage`, kept current by `validate-image-ci`) beside the API as
`glidepath validate --serve 127.0.0.1:8081`, and the API posts to it on the pod's loopback (`VALIDATOR_URL`). So the
API carries no second copy of the rules, and a contract change reaches the check and the API in one bump. When the
sidecar doesn't answer, the call is `GP-API-502`.

Refusals are `GP-API-400`: neither or both of `patch` and `content`, a patch that isn't an object, a `baseRef` that
isn't a ref of the repo, or a base file a patch can't be applied to (not a YAML mapping).

## Live and archived reads

A PipelineRun lives in the cluster while it runs and for about an hour after it finishes, until Tekton deletes it;
Tekton Results archives it for 30 days. The API reads each from where it is:

- **Archived** (`GET /v1/apps/{app}/runs...`, `GET /v1/runs`): Tekton Results, through `tekton-results-relay`.
  Finished runs only, trimmed to what Tower shows, cached (lists a minute, runs 7 days, logs an hour).
- **Live** (`GET /v1/apps/{app}/live...`, `GET /v1/live...`): the API server, with glidepath-api's own
  ServiceAccount. Every PipelineRun and TaskRun as the cluster lists them right now, `metadata.managedFields`
  dropped and nothing else changed. Lists are cached 2 s at most, so several people watching the same view cost one
  list; logs are never cached.

A run in flight is only live; a run older than an hour is only archived; for the hour in between it is both, and a
client that merges the two dedupes by the PipelineRun's uid (the archived run's `result`).

**Live step logs.** `live/taskruns/{taskRun}/logs/{container}` reads the TaskRun in the app's namespace, takes its
`status.podName`, and returns that pod's log for `container` (a step is `step-<name>`) with the container runtime's
timestamps, as `kubectl logs --timestamps` does. The namespace always comes from the app; the pod always from the
TaskRun, never from the request. A container the pod doesn't have, or one that hasn't started yet, is `GP-API-400`.
A TaskRun or pod that is gone, or a TaskRun with no pod yet, is `GP-LIVE-404`: once the run has finished, read the
archived log instead.

**Pushed, not polled.** `GET /v1/apps/{app}/live/stream` (API 0.8.0, `list` on `runs`, like `live`) is the same
answer as server-sent events: `event: snapshot` with every PipelineRun and TaskRun when it opens (the `live`
shape), then `event: change` for each object that appears or moves, `{"type":"upsert","kind":"TaskRun","name":
"...","object":{...}}`, or goes, `{"type":"delete","kind":"PipelineRun","name":"..."}`. A comment line every 15 s
keeps proxies from closing it. There is no resume: a reconnect gets a fresh snapshot, which is the whole state,
and a viewer that falls behind (256 changes queued) is closed with a `: lagged` comment and reconnects. Until
2026-10-31 Tower's CI/CD tab polled `live` every 6 s per open tab, about 760 calls an hour each, every one through
the dev cluster's API server proxy; a stream is one connection.

On each replica one watch per namespace serves every viewer of it: it lists the namespace's PipelineRuns and
TaskRuns every 2 s (through the same 2 s cache the polling reads use) and diffs by `resourceVersion`. It starts
with the namespace's first viewer and stops with its last, so a namespace nobody watches costs nothing. A list
rather than a Kubernetes watch keeps glidepath-api's RBAC as it is. A replica serves at most 200 streams
(`LIVE_MAX_STREAMS`); one more is `GP-API-503`, and the viewer polls `live` instead.

**The fleet.** `GET /v1/live` is every PipelineRun in the cluster; `GET /v1/live/taskruns?pipelineRuns=a,b` is the
TaskRuns of just those runs (by their `tekton.dev/pipelineRun` label; at most 200 distinct names, each a lower-case
DNS name of at most 63 characters; none asked, `{"taskRuns":[]}`). `GET /v1/runs` is `GET /v1/apps/{app}/runs` for
every namespace at once (Results' `parents/-`), the same rows plus `namespace` (the Result's parent) and `app` (from
`app-<app>-cicd` or `infra-<app>-cicd`; absent for any other namespace). All three take `app=` to keep to one app's
namespace, and are authorized like the [events](#events) routes: cluster-wide without `app=`, in the app's namespace
with it. `glidepath-api-reader` bound through `api.readers` is cluster-wide, so Tower's read identity passes.

**What glidepath-api itself may read for this.** Its ClusterRole adds `list` on `pipelineruns`, `get` and `list` on
`taskruns`, and `get` on `pods` and `pods/log`, all cluster-wide (apps are onboarded after install). Nothing else:
no pod list, no exec, no secrets beyond the existing `registry-credentials`.

### The fleet's release records

`GET /v1/releases` is every `release-tracking` ConfigMap (label `hangar.io/subcomponent=release-tracking`) in the
cluster, each as `{"name", "namespace", "labels", "data"}` (`labels` and `data` are `{}` when empty, never null), so
Tower's `toReleaseRecord` can fall back to the `hangar.io/app`, `env` and `cluster` labels as it does now.
`GET /v1/releases/events` is the Events Glidepath's sweeper writes about them: reason `ReleaseStalled` or
`ReleaseDrift` (one list per reason, by field selector), involvedObject a ConfigMap named `release-tracking-*`, each as
`{"reason", "namespace", "recordName", "message", "at"}`: `namespace` is the involvedObject's (else the Event's),
`at` is `lastTimestamp`, else `eventTime`, else `firstTimestamp`, else `creationTimestamp`, in RFC 3339 UTC, and
absent when none is set. Both take `app=` (that app's namespace; the records also only that app's
`hangar.io/app`) and are authorized like the other fleet reads: `list releases` cluster-wide without `app=`, in the
app's namespace with it. Neither is cached (one ConfigMap list, two Event lists). `GET /v1/apps/{app}/releases`
is unchanged. For this glidepath-api's ClusterRole adds `list` on core `events`, cluster-wide (it already lists
ConfigMaps for the per-app records).

## Explain

`explain` reads the run and each failed TaskRun's archived log. For each one, it takes the first step
that exited non-zero and tests that step's lines, plus the TaskRun's condition message, against the
contract's `knownFailures` (`schemas/cicd.meta.yaml`), in contract order. The first match wins:

```json
{"taskRun": "itsm-bh5bn-verify-change", "task": "verify-change", "step": "verify",
 "knownFailure": {"id": "ItsmChangeNotApproved", "hint": "..."},
 "evidence": "error: change CHG0010035 for ... is in state '-5' with approval 'not requested' - not approved or scheduled. ...",
 "log": "archived"}
```

`knownFailure` is `null` when nothing matched, and `evidence` is then the step's last 15 lines. `log`
is `missing` when Results stored no log (see [tekton-results.md](tekton-results.md#when-a-runs-logs-are-missing));
the condition message is still classified, which catches timeouts. A log Results can't serve fails
the call with 502, so "nothing matched" always means the log was read. The patterns compile when
the API starts; one that doesn't compile stops it. A new failure gets an id by adding it to
`knownFailures` and regenerating the contract.

## Evidence

`evidence` is Tower's supply-chain panel as an API: everything signed about one image, and whether
each signature still verifies against this cluster's own trust roots. It reads the image from GHCR
in the three places an attestation can live, in this order, and returns all of them:

1. the legacy tag `<repo>:sha256-<digest>.att`: Tekton Chains' SLSA provenance, one DSSE envelope
   per layer, with the certificate and the Rekor receipt in the layer's annotations;
2. OCI 1.1 referrers (GHCR's referrers-tag fallback, `<repo>:sha256-<digest>`): cosign 3's
   attestations (the SBOM, the API spec), each a complete sigstore bundle with an inclusion proof;
3. the signature tag `<repo>:sha256-<digest>.sig`: the image signature itself.

Each one is verified the way Tower's `@sigstore/verify` check did until phase 6 (2026-10-30; Tower now
shows this answer), with the same checks in the same order: the statement names this image's digest,
the signing certificate chains to the Fulcio root and was valid when Rekor logged the entry, the log entry's receipt is signed by Rekor's key (or, for
a bundle checked on its own, its Merkle inclusion proof reaches a checkpoint Rekor signed), the
entry records this signature over this content, and the signature verifies with the certificate's
key. Each receipt is re-read from Rekor by its log index first, so it is signed by Rekor's current
key; a different body at that index is `log-entry-mismatch` and never verifies. The signature tag
carries no receipt (Chains logs it without attaching one), so its entry is found among the entries
next to the attestations' and kept only if it records exactly this payload hash and signature.

One attestation is the exception: the Flight promotion attestation (predicate
`https://hangar.io/attestations/promotion/v1`, [registries.md](registries.md)) on an image the
promoter copied to Flight. The promoter workflow signs it keyless on **public Sigstore**, not on
this cluster's Fulcio, so it is verified the way an upper cluster's admission does:

- against the Sigstore public-good trusted root built into glidepath-api (TUF target
  `trusted_root.json`, sha256 `6494e21e…`, the same bytes Tower embedded until phase 6), so the check is offline;
- with everything that root requires: the certificate chains to public Fulcio, carries an SCT from a
  trusted CT log, is dated by Rekor's receipt or the RFC 3161 timestamp, and the entry's inclusion
  proof reaches a checkpoint public Rekor signed;
- and only for the exact promoter identity: SAN
  `https://github.com/<promoterRepo>/.github/workflows/promote.yaml@refs/heads/main` (anchored, so
  no other branch, repository or workflow) and Fulcio issuer
  `https://token.actions.githubusercontent.com`.

Its log index belongs to public Rekor, so it is never looked up in this cluster's Rekor, and its
`transparencyLog` carries no `logId`; `logSource` is `embedded`. The promoter repository is
`api.promoterRepo` (default: `promoter.dispatchRepo`, the repository the promote pipeline already
dispatches). With neither set, promotion attestations come back unverified with
`no promoter configured`.

Act on `verified` and `verificationCode` (`verified`, `no-log-entry`, `log-entry-mismatch`,
`stale-receipt`, `digest-mismatch`, `signature-invalid`, `trust-root-unavailable`, `incomplete`).
`verificationError` is the verifier's own message, kept identical to what Tower's own check said (Tower shows
it); display it, don't parse it. The certificate's identity is shown, not checked against the
release gate's identity policy: that is the gate's job (`verify-image-provenance`).

Where the inputs come from:

| Input | Source | Kept |
|---|---|---|
| The image, its manifests and blobs | GHCR, with a pull token for that one repository | per request |
| The GHCR credential | the app's own `registry-credentials` Secret (dockerconfigjson, `auths["ghcr.io"]`) in its `-cicd` namespace; for a Flight image (`ghcr.io/<promoter repository owner>/...`), the Flight registry's read credential `flight-registry-read` in the catalog namespace (`FLIGHT_PULL_SECRET`, set when the chart's promoter is on) | per request |
| Fulcio root | ConfigMap `fulcio-root-ca` (`cert.pem`) in `platform-catalog` | 10 minutes |
| Rekor's public key | `rekor-server.rekor-system` `/api/v1/log/publicKey` | 10 minutes |
| Rekor entries | `rekor-server.rekor-system` `/api/v1/log/entries?logIndex=` (GET only) | per request |
| Public Sigstore root (promotion attestations only) | built into the binary; refresh it when Sigstore rotates a key (`cosign initialize`, then `~/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json`) | for the build |

The registry credential is read for each request and used only for GHCR's token exchange
(`/token?scope=repository:<repo>:pull`). It is never cached, logged, returned, or put in an error;
a refused exchange reports GHCR's status code, never its body. The pull token GHCR returns is not
cached either. A missing Secret, or one without a `ghcr.io` entry, is an anonymous exchange, which
only public packages allow. The chart grants glidepath-api `get` on Secrets named
`registry-credentials`, and on the one Secret `flight-registry-read` in the catalog namespace (Role
`glidepath-api-flight-read`), and nothing else among Secrets. An app's `registry-credentials` is the
Ground owner's, which the Flight registry refuses (403) once that owner has no access to the Flight
account (2026-10-30), hence the separate Flight credential; the Fulcio ConfigMap is readable by every
ServiceAccount already (`templates/sigstore/fulcio-root-configmap.yaml`).

A Fulcio root that can't be read doesn't fail the call: every attestation comes back unverified with
`trust-root-unavailable`. A Rekor key that can't be read is softer still: verification runs without
the log and fails with `no-log-entry`, since nothing then shows the ten-minute certificate was valid
when it was used. Only `ghcr.io/...` images are accepted (`GP-API-400`). Tower's identity carries
`get evidence` through `glidepath-api-reader` like every other read; whoever can read an app's
evidence can ask about any image that app's credential can pull.

## Tests

`tests` lists the app's Testkube executions (every run Glidepath tagged `app=<app>`, newest first, up
to `limit`, default 50, at most 200) from `testkube-api-server.testkube:8088` in the cluster. Testkube's
list returns summaries only, so each run is read in full, four at a time; a run whose full read fails
is shown from its summary. A run the agent reported `aborted` while every step passed is counted as
`passed` and says so in `statusNote` (a known race on short runs). `tests/{id}/report` returns the
JUnit report the check-ride templates leave (`data/artifacts/junit.xml`), and only for a run tagged
with this app (Tower's own route, before phase 6, didn't check that).

## Events

`GET /v1/events` streams the CDEvents Glidepath's pipelines and the release relay send to the shared broker
(`cdevents-broker`, [chaining.md](chaining.md)): builds, tests, deploys, releases, promotion-ready, pipeline runs
started and finished. Each is one server-sent event:

```text
id: 1761742812345678-3f2a9c01b7de
data: {"id":"1761742812345678-3f2a9c01b7de","app":"gate-api","routingKey":"gate-api.pipelinerun.finished","receivedAt":"2026-10-29T12:00:12.345678Z","event":{...the CDEvent...}}
```

- `app` is the app the sender's verified identity speaks for; `event` is the CDEvent exactly as sent.
- `routingKey` is `<app>.<subject>.<predicate>` from the CDEvent type, lower case. It has AMQP topic semantics, so
  `gate-api.pipelinerun.*` or `*.release.#` select events the way Autopilot's event triggers do
  (`autopilot.triggers.amqp_topic_match`).
- `app=` keeps one app's events, `type=` a CDEvent type prefix (`dev.cdevents.pipelinerun.`).
- A comment line (`: ping`) every 15 s keeps an idle stream open through proxies, including the API server's
  service proxy. A replica serves at most 100 streams (`api.events.maxStreams`); one more is `GP-API-503`.

**Resume.** Ids are the first receiving replica's receive time (unix microseconds) and a hash of the event's
`context.source` and `context.id`, so they mean the same on every replica. Reconnect with `Last-Event-ID` (what an
EventSource sends by itself) or `?since=<id>` and the replica first replays what its buffer holds after that id, plus
anything received up to 10 s before it: replicas add each other's events slightly out of order, and replaying a
little more is how a resume misses nothing. A subscriber dedupes by `id`. `?since=` also takes an RFC 3339 time. A
client that falls 256 events behind has its stream ended; it reconnects with `Last-Event-ID`.

**Best-effort, not durable.** Each replica keeps the last 2000 events, none older than 24 h (`api.events.maxEvents`,
`api.events.maxAge`), in memory. A replica that starts copies its peers' buffers, so a rolling deploy keeps them, but
when every replica restarts at once the buffer is empty, and a resume from before what the buffer holds silently
starts at its oldest event. For history, read runs and release records; the stream is for reacting.

`GET /v1/events/recent` answers the same filters from the buffer as JSON (`{"events":[...]}`, oldest first, the
newest `limit`, default 100): for tests, agents, and callers that poll with `since=` instead of holding a stream.

**Who may read events.** `list` on `events.glidepath.hangar.io` (one verb for both routes; `watch` is not used). With
`app=` it is checked in that app's `-cicd` namespace like every other read; without `app=` it is checked
cluster-wide, so a caller with only per-app access must pass `app=` (`GP-AUTHZ-001` says so). `glidepath-api-reader`
includes `events`, so `api.readers` can read the stream.

**Why SSE and not AMQP.** The Glidepath API spec's phase 5 says "AMQP topics for Autopilot". The platform runs no
RabbitMQ for Glidepath, and adding one for a single subscriber costs more than it buys, so the API serves SSE with
resume instead. `routingKey` keeps the topic semantics, so an AMQP bridge (stream in, publish with that key) can be
added later without any subscriber changing what it matches.

### How events get in

```
pipeline / glidepath-relay --POST--> el-cdevents-broker (EventListener, every Trigger in every namespace)
                                         |
                                         +-- app Triggers: cdevents-broker-auth, then their CEL filters
                                         +-- Trigger glidepath-api-events (platform-system)
                                               |  ClusterInterceptor glidepath-api-events
                                               v
                                   glidepath-api /internal/events/tap   (always continue: false)
                                               |  re-verify, store, forward
                                               v
                                   the other replicas /internal/events/peer
```

The tap is a Trigger in `platform-system` whose only interceptor is `glidepath-api-events` (glidepath-api's
`/internal/events/tap`). The interceptor always answers `continue: false`, so the Trigger never creates anything
(its TriggerTemplate exists only because a Trigger must name one, and the broker's ServiceAccount couldn't create
its ConfigMap anyway).

**The tap trusts nobody.** Anything in the cluster can POST to `glidepath-api:8080`, so the tap never assumes the
broker or `cdevents-broker-auth` ran. It re-checks each event's `Authorization` header the way
`cdevents-broker-auth` and the app Triggers' filters do together:

- a TokenReview for the broker's audience (`cdevents-broker`; the audience in the interceptor request is ignored),
  and the identity must be a ServiceAccount;
- a pipeline's event (`context.source` `/glidepath/<namespace>/<run>`) must come from a ServiceAccount in that same
  namespace, an app's `<type>-<app>-cicd` namespace; that names the app, and a `subject.content.appName` that says
  otherwise is refused;
- only `glidepath-relay` (`system:serviceaccount:platform-system:glidepath-relay`) may speak for any app, as
  `release-outcome-trigger.yaml` allows: its event's `subject.content.appNamespace` must be the named app's `-cicd`
  namespace and the source's.

Anything else is dropped and logged (`events.tap drop` with the reason, type, source and event id; never a token or
a header). An event that passes is logged as `events.tap keep`.

**Fan-out.** The broker's interceptor call reaches one replica. That replica stores the event and posts it to every
other address in the `glidepath-api` Service's EndpointSlices (a Role in `platform-system`: `list` and `watch` on
`endpointslices`, nothing else), at `/internal/events/peer`, with a second projected token (audience
`glidepath-api-peer`). A replica accepts a peer call only when TokenReview says that token is
`system:serviceaccount:platform-system:glidepath-api` for that audience. Each replica dedupes by `context.source` +
`context.id`, so a retried send or a peer's copy is kept once.

```shell
# live, through the service proxy, as a reader
curl -N --cacert <ca> -H "Authorization: Bearer $TOKEN" -H "X-Glidepath-Authorization: Bearer $TOKEN" \
  "$APISERVER/api/v1/namespaces/platform-system/services/glidepath-api:8080/proxy/v1/events?app=gate-api"
kubectl logs -n platform-system deploy/glidepath-api | grep '"msg":"events.tap'
```

## Errors

Every refusal or failure is JSON with a stable `rule` and, where there is one, a `hint`:

| Rule | Status | Meaning |
|---|---|---|
| `GP-AUTH-001` | 401 | No bearer token |
| `GP-AUTH-002` | 401 | The cluster didn't accept the token |
| `GP-AUTH-003` | 403 | On-behalf-of from a caller that isn't a delegate |
| `GP-AUTH-004` | 400 | A delegate's write must name the person (`X-Glidepath-On-Behalf-Of`) |
| `GP-AUTHZ-001` | 403 | Not allowed to read or write this in this app's namespace |
| `GP-API-400` | 400 | A malformed app name, run id, chain id, test run id, PipelineRun name, environment or cluster name, `Idempotency-Key`, `dryRun` or write body, an image outside `ghcr.io`, an event `since` or `limit`, a live TaskRun, container or `pipelineRuns` name, or a container the pod doesn't have or that hasn't started |
| `GP-API-404` | 404 | No such app, run, PipelineRun, release record, or test report |
| `GP-API-409` | 409 | The name a write's key derives is held by something that isn't that write's run; send a new key |
| `GP-API-422` | 422 | The write can't be done as asked: out of order, a Ground target, an ineligible rollback target, nothing recorded to promote, no TriggerTemplate, or the cluster refused it (an admission policy or glidepath-api's RBAC). The `hint` says what would work; don't retry as is |
| `GP-LIVE-404` | 404 | The live TaskRun or its pod is gone (Tekton deleted the finished run), or the TaskRun has no pod yet; read the archived log |
| `GP-LOG-404` | 404 | The run exists, but Results archived no log for that TaskRun ([tekton-results.md](tekton-results.md#when-a-runs-logs-are-missing)) |
| `GP-API-503` | 503 | This replica already serves its maximum number of event streams (retry, or read `GET /v1/events/recent`) or live streams (poll `GET /v1/apps/{app}/live`) |
| `GP-API-502` | 502 | A source failed (the API server, Tekton Results, GHCR, Rekor, Testkube, GitHub or the token broker); retry |

## Audit

Every call writes one JSON line to the pod's stdout: `op`, `tier`, `app`, `caller`, `onBehalfOf`,
`decision` (`allow` or `deny`), `status`, `ms`, and the error if there was one. A write adds `name` (the
run in the path) and `dryRun`; a re-run adds `idempotencyKeySha256` (the first 12 hex characters of
the hash in the run's annotation, never the key), `created` and `replayed`; a cancel adds `result`
(`cancelled`, `already-cancelled`, `already-finished`). A promote adds `env`, `cluster`, `idempotencyKeySha256`,
`mode`, `created`, `replayed` and `prUrl`; a rollback `env`, `cluster`, `idempotencyKeySha256`, `target`,
`replaces`, `created`, `replayed` and `prUrl`; a validation `valid`, `errors`, `changes`, `agent` and `base` (the
short sha). No GitHub token is ever in an audit line. An event stream writes its
audit line when it ends, so its `ms` is how long it was open.

```shell
kubectl logs -n platform-system deploy/glidepath-api | grep '"msg":"audit"'
```
