# Release state

Every release to an upper-cluster environment has a **record** on the dev cluster that says
where the release is: proposed, merged, rolling out, healthy, failed, superseded. The record
is what turns a stream of low-level facts from the prod cluster (a Rollout changed phase, a
sync failed) into the release events the rest of Glidepath consumes, and it is what lets the
platform tell a release from a scale, a restart or a manual edit.

This page is the reference. The decision and its history are in
[ADR-0021](adr/0021-rollout-facts-and-release-record.md) (with
[the phase 0 findings](adr/0021-phase0-findings.md) and
[phase 1 results](adr/0021-phase1-results.md)); the event vocabulary is
[ADR-0022](adr/0022-cdevents-conformance-and-vocabulary.md).

![The release record: who writes it, who reads it](diagrams/release-record-lifecycle.svg)

## The record

One ConfigMap per release, `release-tracking-<chain-id>`, in the Application's own namespace
(`app-<name>-cicd`) on the dev cluster. Labels: `hangar.io/subcomponent=release-tracking`,
`hangar.io/app`, `hangar.io/env`, `hangar.io/cluster`. The release is identified by
`releaseId`, `<chain-id>:<cluster>/<env>`, which also rides on the Rollout as the
`hangar.io/release-id` annotation so a fact can be joined to its record.

| Key | Written by | Meaning |
|---|---|---|
| `prUrl`, `prCreatedAt` | `open-release-pr` | The gitops release PR and when it opened. |
| `releaseId`, `kind` | `open-release-pr` | The release's identity; `promote`, or `rollback` (ADR-0021 phase 4). |
| `image` | `open-release-pr` | The image the release deploys. Rollback eligibility matches on it; records written before phase 4 have none and never qualify. |
| `rollbackOf`, `rollbackReason` | `open-release-pr` | A rollback only: the release-id it replaces, and why. |
| `appNamespace`, `appName`, `env`, `cluster` | `open-release-pr` | What was released and where. |
| `gitUrl`, `gitRevision`, `flowStartTime`, `configJson` | `open-release-pr` | The context the outcome events and the lead-time anchor need. |
| `postPrError`, `changeLinkedAt` | `open-release-pr` | `postPrError`: only when something after the PR opened failed (below): what, and why; cleared when a re-run completes the job. `changeLinkedAt`: when the change tool took the PR link; a re-run links again until it is set. No reader looks at either. |
| `state`, `stateAt` | `open-release-pr` (`proposed`), `mark-release-merged` (`merged`, `closed`), the relay (everything after) | Where the release is, and since when. |
| `mergedAt` | `mark-release-merged` | When the PR merged, recorded whatever state the release is in (Argo CD and the Rollout can start faster than the Task does, so the record may already be `progressing`: seen live). The stall alert's clock for a merged release, and the merge-to-deploy latency. |
| `lastFactAt`, `lastFactPhase` | relay | The last fact that arrived for this release. The stall alert's clock for a progressing release. |
| `podHash` | relay | The pod template hash of the release's first fact. A different hash later is drift. |
| `drift` | relay | The last drift description, if any. |
| `lastError` | relay | The last Argo CD sync error for this release. |
| `emittedLive`, `emittedShadow` | relay | The CDEvent kinds already sent, per mode (below). |
| `supersededBy` | relay | The release that replaced this one. |
| `healthyAt` | relay | When the release first reached `healthy`. Kept through `superseded` and `rolled-back`: "this image ran healthy here" is what rollback eligibility reads. |
| `rolledBackBy` | relay | The rollback release that replaced this one. |
| `changeRequestId`, `changeRequestNumber`, `changeRequestUrl` | `open-release-pr` | The release's change request in the change tool ([change-management.md](change-management.md)); empty without a provider. |
| `changeRequestState`, `changeRequestApproval`, `rolledBackByChange` | `itsm-gate`, `itsm-change` | The change as last seen by the gate, then `implementing`, `implementing-failed`, `closed-successful` or `closed-unsuccessful` (with the rollback's emergency change). |
| `originChainId`, `originChainSlug`, `originTraceId` | `open-release-pr` | Promotion lineage ([api.md](api.md#promotion-lineage)): the flow that built the image a promotion or rollback releases, read from the annotations glidepath-api stamped on the run (never a parameter), shape-checked, empty when there is none. The release keeps its own chain id. `GET /v1/apps/{app}/releases/{chainId}` returns them as `origin: {chainId, chainSlug, traceId}`; `GET /v1/apps/{app}/releases?fromChain=<chain>` lists what came from a build. |
| `reissueOf`, `reissuedBy` | `open-release-pr` | [Reissue](#reissue): `reissueOf` on the new release names the chain id of the closed one it replaces (read from the run's `glidepath.io/reissue-of` annotation, which glidepath-api stamps, never a parameter); `reissuedBy` on the closed one names the new chain. `GET /v1/apps/{app}/releases/{chainId}` returns them as `reissue: {of, by}`. |
| `clearanceId`, `clearanceTree` | `clearance-gate` (G-ARGO-6) | The Clearance change the release's PR was registered as, and the manifest-tree digest it was registered for. Written right after `POST /v1/clearances` answers; absent for a release to an unprotected environment. `GET /v1/apps/{app}/releases/{chainId}` returns them as `clearance: {id, tree}`, and `release-log-emit` copies them into the release log as `clearance_id` and `clearance_tree`. |
| label `hangar.io/stall-alerted` | sweeper | The state a `ReleaseStalled` Event was already raised for. |

The record is **not deleted when its outcome is read** (it used to be). The state machine needs
it afterwards, and the sweeper removes it past its retention.

Outside Glidepath, Tower's backend reads the records (`list` with the labels above, as its
read identity on the dev cluster) for three things: the releases a rollback may target, the
images an image prune must keep, and each release record's **outcome** (clean, rolled back,
replaced before healthy, stopped before production), from `state`, `healthyAt`,
`supersededBy`, `rolledBackBy`, `rollbackReason` and `image`. Tower writes the outcome into
the release record in `gitops-<app>` once it is final, so it outlives the sweep; a release
whose records were swept first, or written before phase 4 (no `image`), has no outcome. See
Tower's `docs/release-records/index.md`.

## The state machine

![One release, from PR to terminal state](diagrams/release-state-machine.svg)

| Move | Caused by | Written by |
|---|---|---|
| (new) → `proposed` | The release PR opens | `open-release-pr` (also for Tower's Promote, see below) |
| `proposed` → `merged` | The gitops PR merges | `mark-release-merged`, run by `bypass-merge-check`, which is triggered by the **push to main the merge produces** (`resolve-merged-pr` turns the pushed commit back into the PR number; a push that is not a PR merge skips it). It only moves `proposed` (or a record with no state), never a state the relay owns. |
| `merged` → `progressing` | The Rollout reports `Progressing` or `Paused` | relay |
| `merged` → `sync-failed` | Argo CD reports a failing sync | relay |
| `sync-failed` → `progressing` | Argo's retry succeeded and the Rollout reports | relay |
| `progressing` → `healthy` | The Rollout reports `Healthy` | relay |
| `progressing` → `aborted` / `degraded` | The Rollout reports `Degraded` (`aborted` if the abort flag is set) | relay |
| `aborted` / `degraded` → `healthy` | The Rollout recovers | relay |
| `aborted` → `progressing` | Tower's *Retry*: a `Progressing` fact with the abort flag clear (no second `deploying` event; a later `Healthy` reports success, another abort goes back to `aborted`) | relay |
| `healthy` → `superseded` | A newer release of the same app, environment and cluster reports its first fact | relay |
| any ran state → `rolled-back` | A rollback release naming this one in `rollbackOf` turns healthy; one `service.rolledback` event is sent. Only a release of the same app, environment and cluster can be marked. | relay |

Tower's Promote to a Flight environment runs the same `release` Pipeline, from the
`promote-release` TriggerTemplate glidepath-app renders into each Kubernetes app's `-cicd`
namespace; Backstage fills its params and creates the PipelineRun. Until 2026-10-08 the
Backstage backend opened the release PR itself and wrote no record, so the relay dropped every
fact for a Tower-promoted release as `no-record`.

`sync-failed` is not terminal. `healthy`, `aborted`, `degraded`, `superseded`, `rolled-back`
and `closed` are.

### What a fact does

Facts are at-most-once and arrive repeated, out of order, and describing things that are not
releases (all of it observed on a real release). The reducer (`reducer.go`, a pure function, so
every row below is a plain test) handles it like this:

| Fact | Result |
|---|---|
| First `Progressing` or `Paused` for a release | State `progressing`; one `environment.deploying` event. |
| Further `Progressing`, `Paused`, or a heartbeat | Nothing. |
| `Healthy` | State `healthy`; one `environment.deployed` (success) event. A repeat, or a heartbeat, emits nothing. |
| `Degraded` | State `aborted` or `degraded`; one `environment.deployed` (failure) event. |
| The first fact seen is already `Healthy` or `Degraded` | The `deploying` event that was lost before it is sent first, then the outcome. |
| `Progressing` after the release is terminal | Ignored (Phase 1 saw one arrive in the same second as `Healthy`). |
| **Same release-id, different pod template hash** | **Drift**: a `ReleaseDrift` Event on the record, state unchanged, no CDEvent. A manual edit, or an `undo` that selfHeal has not yet reverted. A scale, a restart or a heartbeat keep the hash, so they are not drift and not releases. |
| `Degraded` after `Healthy` was already reported | Drift, not a failed deploy: the workload failed after a good release. |
| An Argo CD sync failure on a merged, progressing or sync-failed release | State `sync-failed`; `deploying` and a failure event, once per release however many retries. |
| An Argo CD sync failure with no such release in flight (a hand edit to `values.yaml`) | Ignored. |
| A release's first fact, while an older release of the same app, environment and cluster is live | The older one becomes `superseded` (best effort). |

The pod template hash is deterministic for a given template, but it is not an identity for
"the same image": rolling back to an earlier image can produce a different hash. The release-id,
not the hash, identifies a release.

## How a fact is applied

![How a fact is applied](diagrams/release-fact-delivery.svg)

- **Events go out before state is written.** If the write then fails, the next fact repeats the
  same deterministic event ids (derived from the release-id, event type and outcome), which the
  broker's Triggers collapse. The other order could lose an event for good.
- **A failed forward returns 502 and writes nothing.** The sender does not retry, but the
  Rollouts engine re-sends current state every 15 minutes (its heartbeat), so the retry is the
  next heartbeat. Rollouts notifications are at-most-once and fail silently (phase 0); the
  heartbeat, and the prune CronJob that resets the engine's own state, are what make the stream
  recover.
- **A write conflict re-reads and re-reduces.** There are two relay replicas.
- **A record the relay cannot write** (no Role yet, or it vanished) degrades to the stateless
  behaviour of phase 1: the deterministic ids still make repeats harmless.
- **The hook Jobs are gone** (phase 3b/3c). While both ran, emit mode dropped the hook Jobs'
  copy of an event when the facts owned the release, and forwarded it as a fallback when they
  had not; that code, the `/outcome` endpoint and the hook script were deleted with the Jobs.
  A release whose facts are lost now waits for the next heartbeat (15 minutes), and the sweeper
  raises `ReleaseStalled` if none arrives.
- **Shadow and emit keep separate sent-sets** (`emittedShadow`, `emittedLive`). In `shadow`
  (opt-in; `outcomeRelay.factsMode` defaults to `emit`) the relay logs the event it would send
  as `shadow-event` and forwards nothing. Flipping to `emit` then sends the history instead of
  believing shadow already did.

### The two sources

| | Rollouts notifications (`POST /facts/<cluster>`) | Argo CD notifications (`POST /argocd/<cluster>`) |
|---|---|---|
| Reports | A tracked Rollout's phase, step and pod hash | A tenant Application's failing sync |
| Fires | On each phase change, plus a heartbeat every 15 minutes | On the first failed attempt (`retryCount > 0`) and on the final failure |
| Why | The Rollout is where the release's meaning lives | The Rollout never sees a sync rejected before it changes |
| Joined by | `hangar.io/release-id` on the Rollout | `hangar.io/app` and `hangar.io/env` labels, newest in-flight release |

Tenant Applications retry (limit 5, backoff up to 10 minutes), about 15 minutes to a final
`Failed`, and `operationState.phase` stays `Running` through the retries. A trigger on
`phase == Failed` alone would be 15 minutes late, which is why the first failed attempt fires.

## Stalls and retention

`release-record-sweeper` (control plane, every 10 minutes at minute 2, `cronSchedules.releaseRecordSweeper`):

- raises a `ReleaseStalled` Event on a record that is `merged` with no fact for 45 minutes
  (the sync never started, or facts are not arriving), or `progressing` with no fact for 45
  minutes (the heartbeat arrives every 15), once per state;
- deletes records past 14 days: terminal and closed records by `stateAt`, proposed and
  state-less legacy records by creation time. A `merged` or `progressing` record is never swept,
  and neither are the newest five records per app, environment and cluster that reached
  `healthy` (the rollback-eligibility window, below).

The knobs are env vars on the CronJob (`RELEASE_STALL_MINUTES`, `RECORD_TTL_DAYS`, `KEEP_HEALTHY`).

## Reissue

A release PR can be closed without merging (the gitops repo's checks could never start on it, or it was opened against stale
files). Closing makes no push, so the record stays `proposed` until a later release PR for the same app, environment and cluster
opens and `open-release-pr` closes it. `POST /v1/apps/{app}/releases/{chainId}/reissue` ([api.md](api.md#reissue-a-release-pr))
starts a **new release** of the closed one's own image and commit: a new chain id, a new record, a new PR cut from the current
default branch of the gitops repo. It does not keep the chain id: a record is one chain, one environment and one PR, and every
reader of `prUrl`, `prCreatedAt` and `state` (the rollback plan's "newest ran release", the sweeper's clocks, the stall alert,
`close_stale_proposals`, the fleet views, Tower's Ops Wall) would otherwise have to cope with a record whose PR changed under it.
With a new chain no existing reader changes, and each record stays a true account of one PR.

| Record | State from reissue | Fields |
|---|---|---|
| the closed release | `proposed` becomes `closed` (by the reissued run's `open-release-pr`, as `close_stale_proposals` already does for any closed PR); a `closed` one stays | `reissuedBy` = the new chain id |
| the new release | `proposed`, then the normal machine | `reissueOf` = the closed chain id; everything else as any promotion's |

Why `closed` and not `superseded`: `superseded` is the relay's word for a release that ran healthy and was replaced
(rollback eligibility reads it). The closed release never ran; its PR was closed, which is what `closed` says.

**States a reissue is accepted from:** `proposed` (the usual one) and `closed`, and only while the record is the newest for its
app, environment and cluster. **Refused from:** `merged`, `progressing`, `healthy`, `sync-failed`, `aborted`, `degraded` (the
release happened or is happening), `superseded`, `rolled-back` (it ran and was replaced). There are no `pending` or `blocked`
record states: a release whose gates cannot start is a `proposed` record with a PR checks never ran on.

## Rollback (ADR-0021 phase 4)

A rollback is a release of an earlier image: Tower's *Roll back* starts the app's
`rollback-release` TriggerTemplate (glidepath-app), the same `release` Pipeline as Promote with
`release-kind: rollback`, `rollback-of` and `rollback-reason`, and `config-revision` so
`cicd.yaml` is read at the app repo's `main` while provenance still checks the image's own
commit. `open-release-pr` records the kind, opens branch `release-rollback-<env>-<sha8>` and titles
the PR `Rollback:`.

The content gates (`sast`, `image-scan`, `sbom`) work out for themselves whether the promoted
image is an *eligible* rollback target: `extract-promoted-image`'s `rollback-eligibility` step
reads this app/environment/cluster's records and answers yes when one of the newest five that
reached `healthy` ran the same image. Then the gate runs its check as `<task>-advisory` with
`onError: continue`: the check run passes and the PR comment shows what it found. Integrity
gates (provenance, values, commit signing) and approvals are unchanged. The PR's own text and
trailers are never trusted for this.

## Permissions

Per Application namespace (`glidepath-app`, `identity/release-record-relay.yaml`), not
cluster-wide:

| Identity | Verbs on ConfigMaps in `app-<name>-cicd` | Also |
|---|---|---|
| `glidepath-relay` (platform-system) | get, list, update | create Events |
| `release-record-sweeper` (platform-system) | get, list, patch, delete | create Events |
| `pipeline-runner` (the Tekton tasks) | get, list, create, patch, delete (list added for `mark-release-merged`) | |

The relay's cluster-wide `get` on ConfigMaps from phase 1 (`glidepath-relay-records`)
remains until every namespace has the Role, then goes. The two infra apps (`skyport-auth`,
`skyport-broker`) have no release flow and no Role; the relay and sweeper skip them.

## Operating it

```bash
# every release's state, newest last
kubectl get cm -A -l hangar.io/subcomponent=release-tracking \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,STATE:.data.state,SINCE:.data.stateAt,LAST_FACT:.data.lastFactAt

# one release in full
kubectl -n app-gate-api-cicd get cm release-tracking-<chain-id> -o yaml

# drift and stalls
kubectl get events -A --field-selector reason=ReleaseDrift
kubectl get events -A --field-selector reason=ReleaseStalled

# what the relay would send, if factsMode is shadow
kubectl -n platform-system logs -l app=glidepath-relay --prefix | grep shadow-event
```

## Tests

- `glidepath/broker`: `go test ./cmd/glidepath-relay`. The reducer's rows above, state
  persistence, a failed forward, a write conflict, an unwritable record, drift, supersede,
  shadow-then-emit, and the Argo CD endpoint.
- `charts/glidepath-catalog/tests/mark_release_merged_test.sh`,
  `charts/glidepath-catalog/tests/rollback_eligibility_test.sh` and
  `charts/glidepath-control-plane/tests/release_record_sweeper_test.sh` run the real Task and
  sweeper scripts against a stub `kubectl`.

## Not built yet

Moving the events onto the spec's vocabulary
([ADR-0022](adr/0022-cdevents-conformance-and-vocabulary.md)). Recording on the release that a
Tower *Promote full* skipped analysis (Backstage audits and announces it; the fact carries no
such field yet).

## The record survives the calls after the PR

`open-release-pr` writes the record **immediately after the PR is created, before any other network call** (the change tool's
link, the PR's change line, closing stale proposals, linking a reissue). 2026-11-16, backstage 1.0.2 to prod: the PR, its change
request and its branch existed, the change tool's link call failed during an overload, the step died, and a release reached prod
with no record (no Tower row, no release log entry, no lineage, no lead time, nothing for the relay to mark merged or healthy).

Why after, not before: a record written before the PR would name a PR that may never exist. Readers match a record to a PR by its
`prUrl` (every gate, `mark-release-merged`, `itsm-change`), list `proposed` records as awaiting approval (the Ops Wall), and a
reissue refuses a record with no PR; an orphan `proposed` record with no PR would sit in those lists until retention. The
state vocabulary is unchanged: no new state, and the one new key, `postPrError`, is ignored by every reader.

| After the PR exists | If it fails |
|---|---|
| The record write | Retried; if it never succeeds the step fails naming the PR, and a re-run finds the PR and writes it. |
| The change tool's PR link (a work note) | Retried with backoff (5 attempts, 2/4/8/16 s; a definite 4xx other than 408/429 is not retried). Still failing: the record stays, `postPrError` says so, the log says so, **the step succeeds**. The link is not required: the itsm gate decides on the change's approval and finds its id on the record and in the PR body's marker, never from the note. What is lost is the work note on the change (a webhook adapter that keeps the PR URL from `link` would not have it); a re-run retries the link. |
| Closing stale proposals, linking a reissue | Best-effort, as before. |

A **PR call whose answer was lost** is checked against the git host before it is called a failure: the PR is adopted when it exists.

**Re-running** the release (a Tekton retry, a glidepath-api rerun, the same chain) finds the open PR from the release branch and
finishes the job: no second PR, no second change request (the change id is read from the PR body's marker), the record written
when missing (state `proposed`, with the PR's own `created_at`) or completed with a merge patch when present (a record the relay has
moved on keeps its `state`, `stateAt` and `mergedAt`), the missed link retried, and a `postPrError` from an earlier run cleared when
it succeeds. A change created for a PR that never had one is the only call a re-run sends once and never retries (its answer
may have been lost); with a change tool configured and no change at all the step still fails, after the record is written.

**The chain id of a re-run.** The relay joins on the chain id in the PR's commit (`releaseTracking.releaseId` in the release file on the PR's head). A Tekton retry of the same run keeps it, but glidepath-api's rerun copies the run's params and a run that started its chain (empty `chain-id` param) mints a new one in `stage-preflight`, so a re-run can carry a different id. The reuse path therefore reads the chain id from the PR's commit and completes the record under it (logging both ids). That file is writable by anyone who can push to the branch, so the id is accepted only as `<lowercase uuid>:<cluster>/<env>` for this release's cluster and environment, and, when a record of that chain exists, only if its app, environment and cluster labels are this run's; anything else fails the run (nothing adopted, no fallback), as does a release file GitHub could not be asked for (re-run later). Only a readable file with no `releaseId` falls back to the run's own id. The record-held change id is read only after these checks, so it is this run's. Fields that come from the new run (`flowStartTime`, the origin annotations) are the new run's: a completed record's lead-time anchor is the re-run's start. A merge patch never writes an empty value, so a run that could not read its annotations cannot blank what the record holds.

**Where a failure is seen.** The step logs it (with the tool's text), writes `postPrError` as `[<reason classes>] <text>` (at most 600 characters), and posts one PR comment: a fixed sentence and a reason class (connection refused, connection reset, timeout, HTTP <code>), never the tool's URL or response body. The comment is posted once per set of classes: a re-run that fails the same way stays silent. If both the PR call and the branch lookup fail, the step says it could not confirm whether the PR was opened (check the branch, re-run) and exits 1.

Test: `charts/glidepath-catalog/tests/release_record_survives_test.py` runs the whole rendered step against fakes with a failure
injected at each call after the PR.

## A release PR that is already merged

The branch's PRs are looked up in every state. If the branch's newest merged PR **is this release** (its base is the default branch; its release file is for this `<cluster>/<env>` and carries this run's image tag; and the environment's release file on the default branch still carries that tag), a re-run **repairs** it: it opens nothing, pushes nothing and creates no change request. With no record it writes one under the chain the PR's commit names (validated as above): state `merged`, `mergedAt` = GitHub's `merged_at` (what `mark-release-merged` would have written at the merge; nothing invented), `repairedAt`, and a `repairNote` saying the merge-time steps did not run. With a record present it is a no-op. The relay carries the record on from the next Rollout fact (its 15-minute heartbeat re-sends the state; a first fact that is already `Healthy` sends the missed `deploying` event first). What is never done for it: `mark-release-merged` and the change tool's `implement` (`itsm-change`) ran at the merge and found no record, so the change request is not moved to implementation or closed. A merged PR that fails any of those checks (another cluster with the same environment name, another image, another base, an environment that has moved on, as when a revision is promoted again after a rollback) is not this release and the normal path runs and no record is repaired. A PR closed unmerged is never reopened; the normal path opens a new one under the reissue rules. A release file GitHub cannot be asked for fails the run (re-run later).

## A PR closed without merging

The merge signal is the push to main, and an unmerged close produces no push, so `mark-release-merged`
never sees one. Instead, when `open-release-pr` opens a new release PR, it closes out the earlier
`proposed` records of the same app, environment and cluster (`close_stale_proposals`): each whose PR
GitHub reports closed and unmerged becomes `closed`. A record still open, merged, or unreadable is left
alone, and this never fails the release. That covers the common case, a release replaced by a newer
one (2026-10-30: five such records made Tower's Ops Wall show releases awaiting approval whose PRs were
long closed). A PR closed with no later release of that environment still stays `proposed` until
retention deletes it (14 days).

## Live verification (2026-10-06)

Every scenario above was exercised against the real clusters, not only in unit tests: real
gate-api releases on kind-prod, and a synthetic Application namespace on kiac-dev driven
through the relay's real endpoints (with the real `mark-release-merged` Task and the real
sweeper CronJob).

| Scenario | How | Result |
|---|---|---|
| Merge marker, three cases (`proposed`, already `progressing`, already has `mergedAt`) | real Task run via the cluster resolver | `merged` + `mergedAt`; `mergedAt` only; untouched |
| Happy path with noise: repeated `Progressing`, `Paused`, `Healthy`, scale facts, heartbeats | synthetic facts | one `deploying` + one success, nothing more; state `healthy` |
| Late `Progressing` after `Healthy` | synthetic | ignored, reported in the response header |
| Scale, then restart, on a real healthy release | real Rollout on kind-prod | no CDEvent, no drift, state unchanged |
| Pod template edited by hand | real Rollout on kind-prod | `ReleaseDrift` Event naming `old -> new` hash, state unchanged, no CDEvent. **Raised 9 Events from one edit** (fixed: once per change) |
| `Degraded` after `Healthy` reported | synthetic | drift, no failure event |
| Abort, repeats, then recovery | synthetic | state `aborted`, one failure; recovery sends the success |
| First fact is already `Healthy` | synthetic | `deploying` sent first, then success |
| Newer release | real gate-api release, and synthetic with older/newer/unmerged neighbours | older live one `superseded` with `supersededBy`; an unmerged proposal and a newer record left alone; a superseded release's further facts send nothing |
| Rejected or ignored requests | synthetic | bad token 401; untracked, stale generation, unknown record, a namespace outside `app-*` all 202 and ignored; another cluster's release-id, a malformed id, and an env that differs from the record all 400 |
| Argo CD sync failure, first attempt through final failure | synthetic, through `/argocd/<cluster>` | `sync-failed`, one `deploying` + one failure however many retries, `lastError` kept; the Rollout then recovers it to `healthy` with the success sent; an unmerged proposal, another env, unlabelled apps and a healthy release are ignored |
| Stall alert and retention | real sweeper CronJob run | `merged` 2 h with no fact and `progressing` 90 min silent raise `ReleaseStalled` once; a recent progressing release does not; a healthy record past 14 days is deleted; a merged record is never swept; a second run raises nothing new |
| Prune CronJob | real, on kind-prod | the Rollout's `notified` annotation went from **90 keys to 4**, the engine resent state at once, no CDEvent, state unchanged, Argo CD still `Synced` |
| A namespace the relay has no Role in | synthetic | fact accepted, events still produced, a log line says it did not persist |

Not exercised live: a forward failure and the 502 retry (needs `emit` mode, unit-tested), a
write conflict between the two relay replicas (unit-tested), and a PR closed unmerged with no later
release (the gap above; `close_stale_proposals` is covered by its script test). The retention of proposed and state-less legacy records is covered by the
sweeper's script test, because a record's creation time cannot be back-dated.
