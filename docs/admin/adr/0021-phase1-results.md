# ADR-0021 Phase 1 results

*2026-10-06. gate-api, `kind-prod/staging`, one real release (`1.1.2-5d5c8fc`, release id
`89608529-26f1-429e-b2f0-f93bce2aa2f3:kind-prod/staging`). Relay in `FACTS_MODE=shadow`,
hook Jobs still live, so both paths reported the same release.*

## Exit criterion: met

The phase 1 check was that both paths emit equal CDEvents for one real release. Normalized
(id, timestamp and `finishedAt` dropped), **the fact path's events were identical to the
hook path's**:

| Event | Hook path | Fact path | Identical |
|---|---|---|---|
| `environment.deploying` (`Syncing`) | 1 at 00:57:15 | 8 at 00:57:29 to 00:59:49 | yes, all 8 |
| `environment.deployed` (`Succeeded`, `success`) | 1 at 00:59:56 | 1 at 00:59:49 | yes |

The 8 `deploying` facts are the Rollout re-reporting `Progressing` after each pause step and
the heartbeat. They carry one deterministic id, so downstream they collapse to one
PipelineRun, which is the idempotency design working. A `deploying` fact also arrived in the
same second as the `deployed` one (00:59:49): a reducer must ignore a `Progressing` after the
release is terminal. Phase 2 does.

## What the real release showed

- **The chain works end to end.** `open-release-pr` wrote the full release record on dev and
  `releaseTracking.releaseId` into `release.yaml`; airframe v0.3.117 rendered
  `hangar.io/release-id` and the `release-tracked` label on the Rollout; the Rollouts engine
  on kind-prod posted facts to the dev relay through the merged ExternalSecret token; the
  relay joined them to the record and rebuilt the events. No errors in the Rollouts
  controller or the relay.
- **The Rollout stayed `Synced` in Argo CD** with 11 `notified...` keys on it, as in phase 0.
- **Hook cost, measured.** The Argo operation ran 00:57:01 to 01:00:00. PreSync ran
  00:57:07 to 00:57:18 (11 s before the Rollout was touched) and PostSync 00:59:50 to
  00:59:59 (the operation closed 11 s after the Rollout was Healthy at 00:59:49). The fact
  path reported success **7 s before** the hook did, and the operation sat `Running` while
  the Rollout was `Paused`, which is the hold ADR-0021 removes.
- **The deploying fact is later than the hook's** (00:57:29 against 00:57:15) because it is
  sent when the Rollout starts progressing, after PreSync and apply, not when the sync starts.
  Lead-time and "time to start deploying" figures will shift by that gap when the hooks go.

## Not yet verified

- **A scale or self-heal produces no new event id on a live Rollout.** Covered by the relay's
  unit tests and by phase 0 on kiac-dev; not repeated on gate-api.
- **A failed or aborted release through the relay.** Covered by the same.
- **The prune CronJob's first scheduled run** (`17 */6 * * *`, first at 06:17 UTC).

## Next

Phase 2: the retained ReleaseRecord and reducer state machine (ignore a late `Progressing`
after a terminal state, stall alert, drift fact), then cut the five apps over (phase 3) and
rename the relay. The Argo CD sync-failed trigger is not installed on kind-prod yet; it
belongs with phase 2 (decision 4 of the ADR, with the `finishedAt` and retry notes from phase 0).
