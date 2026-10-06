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
| `releaseId`, `kind` | `open-release-pr` | The release's identity; `promote` today, `rollback` in phase 4. |
| `appNamespace`, `appName`, `env`, `cluster` | `open-release-pr` | What was released and where. |
| `gitUrl`, `gitRevision`, `flowStartTime`, `configJson` | `open-release-pr` | The context the outcome events and the lead-time anchor need. |
| `state`, `stateAt` | `open-release-pr` (`proposed`), `mark-release-merged` (`merged`, `closed`), the relay (everything after) | Where the release is, and since when. |
| `mergedAt` | `mark-release-merged` | When the PR merged, recorded whatever state the release is in (Argo CD and the Rollout can start faster than the Task does, so the record may already be `progressing`: seen live). The stall alert's clock for a merged release, and the merge-to-deploy latency. |
| `lastFactAt`, `lastFactPhase` | relay | The last fact that arrived for this release. The stall alert's clock for a progressing release. |
| `podHash` | relay | The pod template hash of the release's first fact. A different hash later is drift. |
| `drift` | relay | The last drift description, if any. |
| `lastError` | relay | The last Argo CD sync error for this release. |
| `emittedLive`, `emittedShadow` | relay | The CDEvent kinds already sent, per mode (below). |
| `supersededBy` | relay | The release that replaced this one. |
| label `hangar.io/stall-alerted` | sweeper | The state a `ReleaseStalled` Event was already raised for. |

The record is **not deleted when its outcome is read** (it used to be). The state machine needs
it afterwards, and the sweeper removes it past its retention.

## The state machine

![One release, from PR to terminal state](diagrams/release-state-machine.svg)

| Move | Caused by | Written by |
|---|---|---|
| (new) → `proposed` | The release PR opens | `open-release-pr` |
| `proposed` → `merged` | The gitops PR merges | `mark-release-merged`, run by `bypass-merge-check`, which is triggered by the **push to main the merge produces** (`resolve-merged-pr` turns the pushed commit back into the PR number; a push that is not a PR merge skips it). It only moves `proposed` (or a record with no state), never a state the relay owns. |
| `merged` → `progressing` | The Rollout reports `Progressing` or `Paused` | relay |
| `merged` → `sync-failed` | Argo CD reports a failing sync | relay |
| `sync-failed` → `progressing` | Argo's retry succeeded and the Rollout reports | relay |
| `progressing` → `healthy` | The Rollout reports `Healthy` | relay |
| `progressing` → `aborted` / `degraded` | The Rollout reports `Degraded` (`aborted` if the abort flag is set) | relay |
| `aborted` / `degraded` → `healthy` | The Rollout recovers | relay |
| `healthy` → `superseded` | A newer release of the same app, environment and cluster reports its first fact | relay |
| `healthy` → `rolled-back` | A rollback release turns healthy (phase 4, not built) | relay |

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
- **Shadow and emit keep separate sent-sets** (`emittedShadow`, `emittedLive`). In `shadow`
  (the default, `outcomeRelay.factsMode`) the relay logs the event it would send as
  `shadow-event` and forwards nothing, so it can run beside the hook Jobs without reporting a
  release twice. Flipping to `emit` then sends the history instead of believing shadow already
  did.

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

`release-record-sweeper` (control plane, every 10 minutes):

- raises a `ReleaseStalled` Event on a record that is `merged` with no fact for 45 minutes
  (the sync never started, or facts are not arriving), or `progressing` with no fact for 45
  minutes (the heartbeat arrives every 15), once per state;
- deletes records past 14 days: terminal and closed records by `stateAt`, proposed and
  state-less legacy records by creation time. A `merged` or `progressing` record is never swept.

Both knobs are env vars on the CronJob (`RELEASE_STALL_MINUTES`, `RECORD_TTL_DAYS`).

## Permissions

Per Application namespace (`glidepath-app`, `identity/release-record-relay.yaml`), not
cluster-wide:

| Identity | Verbs on ConfigMaps in `app-<name>-cicd` | Also |
|---|---|---|
| `argocd-outcome-relay` (platform-system) | get, list, update | create Events |
| `release-record-sweeper` (platform-system) | get, list, patch, delete | create Events |
| `pipeline-runner` (the Tekton tasks) | get, list, create, patch, delete (list added for `mark-release-merged`) | |

The relay's cluster-wide `get` on ConfigMaps from phase 1 (`argocd-outcome-relay-records`)
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

# what the fact path would send vs what the hooks sent (shadow mode)
kubectl -n platform-system logs -l app=argocd-outcome-relay --prefix | grep -E 'shadow-event|hook-event'
```

Flipping a cluster from `shadow` to `emit` is `outcomeRelay.factsMode: emit` on the control
plane chart, done only once the hook Jobs for the apps on that cluster are gone (phase 3).

## Tests

- `glidepath/broker`: `go test ./cmd/argocd-outcome-relay`. The reducer's rows above, state
  persistence, a failed forward, a write conflict, an unwritable record, drift, supersede,
  shadow-then-emit, the Argo CD endpoint, and one test that runs the real
  `argocd-outcome-hook.sh` and asserts the fact path builds an identical event.
- `charts/glidepath-catalog/tests/mark_release_merged_test.sh` and
  `charts/glidepath-control-plane/tests/release_record_sweeper_test.sh` run the real Task and
  sweeper scripts against a stub `kubectl`.

## Not built yet

Rollback (a release with `rollbackOf`, `rolled-back`, the `service.rolledback` event, the gate
policy for it), deleting the hook Jobs and the per-app identity chart (phase 3, when `emit`
goes on and the relay is renamed), moving the events onto the spec's vocabulary
([ADR-0022](adr/0022-cdevents-conformance-and-vocabulary.md)).

## Known gap: a PR closed without merging

`closed` (a PR closed unmerged) is in the state machine but nothing sets it yet. The merge signal is
the push to main, and an unmerged close produces no push. The record stays `proposed` until
retention deletes it (14 days). Nothing downstream depends on it.
