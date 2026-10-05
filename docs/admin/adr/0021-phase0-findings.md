# ADR-0021 Phase 0 findings

*2026-10-05. Spikes S1 to S5 from [ADR-0021](0021-rollout-facts-and-release-record.md).
S2 and S3 ran on kiac-dev in a throwaway namespace (`spike-rollouts`, label
`hangar.io/spike=adr-0021`) with a request-catcher pod and a probe Rollout. kiac-dev's
`argo-rollouts` Application has no automated sync, so the config below was not reverted by
Argo CD. S1 and S4 read kind-prod; S4 ran one short-lived curl pod there.*

## Verdict

**S2 passes, with three design rules the ADR must adopt** (A, B, C below). The Rollouts
notifications engine delivers the facts we need. It does not guarantee delivery.

## S2: the Rollouts notifications engine (Rollouts v1.9.1)

| Question | Result |
|---|---|
| Is a restart needed after creating `argo-rollouts-notification-configmap`? | **No.** The controller logs "invalidated cache" and picks it up. |
| Subscription by label selector (`subscriptions:` with `selector: hangar.io/release-tracked=true`)? | **Works.** No per-app annotation needed. |
| Header from the secret (`Bearer $catcher-token`)? | **Works.** The catcher received `Authorization: Bearer spike-token`. The secret key was added with `kubectl patch`. |
| `.rollout` annotations and status in the template? | **Works.** `{{toJson .rollout.metadata.annotations}}` and `{{toJson .rollout.status}}` produced valid JSON including `hangar.io/release-id`, `currentPodHash`, `currentStepIndex`, `abort`, `message`, `observedGeneration`. |
| `oncePer` dedupe? | **Works.** The log shows "already sent" for repeats. |
| Extra annotation on the Rollout? | The engine writes `notified.notifications.argoproj.io` on the Rollout (a JSON map, one key per `oncePer` value). It grew to 20 keys over 12 generations and was not pruned. **Not yet checked: whether Argo CD shows the Rollout OutOfSync because of it** (needs a real Argo-managed Rollout). |

**What arrived for each scenario** (generation, phase, step, release-id):

| Scenario | Facts |
|---|---|
| Create | `Progressing`, then `Healthy` |
| Image update with new release-id | `Progressing step=0`, `Paused step=1` ("CanaryPauseStep"), `Healthy step=3` |
| Update back to the earlier image (rollback-shaped) | Same three facts, new release-id, **different `currentPodHash` from the first time** because the template was not byte-identical; the hash is deterministic for an identical template (the 1.28 image produced the same hash both times) |
| Scale only (2 to 3) | `Healthy` and `Progressing` facts at the new generation, **same pod hash** |
| Bad image, then `status.abort=true` | `Progressing step=0` (stays there, no failure fact while the image will not pull), then `Degraded abort=true message="RolloutAborted: Rollout aborted update to revision 4"` |
| `restartAt` on an aborted Rollout | A repeat `Degraded` fact at the new generation |

### Rules this adds to the ADR

**A. Every trigger must require `observedGeneration == string(generation)`.** My first
version did not, and it sent two `Healthy` facts carrying the *new* release-id and the
*old* pod hash within a second of the patch, before the controller had observed the
update. A reducer would have recorded the new release as Healthy before it started. With
the guard added, no stale fact was sent in any later scenario.

**B. The reducer must treat a fact as a release event only when the pod hash or
release-id changed.** Scale and restart produce facts at a new generation with an
unchanged hash. They are ignored (or recorded as a scale), never as a new release. The
pod hash is not an identity for "the same image": use release-id and the image refs.

**C. Delivery is not guaranteed, and a failure is silent.** With the receiver scaled to
zero, a real release (`rel-5`) went `Progressing`, `Paused` and `Healthy`, the engine
logged "Sending notification" for each, logged **no error**, recorded all of them as
notified, and **never redelivered** after the receiver came back (the catcher received
nothing). This is the risk the ADR called out, and it is worse than assumed: not a delay,
a loss. Mitigations, in order:

1. The stall alert (a record merged with no fact for N minutes) becomes mandatory, not
   an extra.
2. **Heartbeat facts:** a trigger that re-sends the current state once per time bucket
   makes the stream level-triggered, so a lost fact becomes a late one. The Rollouts
   controller re-reconciles every rollout every 15 minutes exactly (seen in the logs at
   :14:13, :29:13, :44:13, :59:13), so that is the heartbeat granularity. A poke
   (annotation write) triggers it immediately. See the open item below.
3. If a 15 minute recovery bound is not acceptable, fall back to the prod-side adapter
   named in the ADR, which can queue and retry.

**Also seen:** a stuck rollout (image will not pull) emits no failure fact until
`progressDeadlineSeconds` (default 600 s) expires. The stall alert covers the gap.

## S1: does a hook hold the Argo CD operation open?

Partly confirmed from data already on kind-prod; the indefinite-pause case is not tested.

| App (staging) | Operation `startedAt` to `finishedAt` | Hook Jobs |
|---|---|---|
| baggage-api | 15 m 48 s | SyncFail at +3 m 30 s, then PreSync 3 s and PostSync 4 s at the end |
| boarding-api | 13 m 48 s | SyncFail at +10 m, then PreSync 5 s, PostSync 14 s |
| flight-api | 15 m 18 s | not collected |
| checkin-api | 2 m 16 s | PreSync 7 s |
| gate-api | 4 s | none |

- Each release adds **PreSync 3 to 7 s and PostSync 4 to 14 s** of Job time to the
  operation, and the PostSync Job starts only after the Rollout is progressing and
  healthy (boarding: PreSync done 01:25:12, PostSync started 01:25:28, with a 30 s canary
  pause in between).
- The 14 to 16 minute operations are retry loops after failed syncs (each has a SyncFail
  Job in the middle), during which the operation stays Running. That is the same
  mechanism as the inferred problem: an in-flight operation delays the next sync.
- **Not tested:** a canary that pauses indefinitely or runs a long analysis. The staging
  canaries here pause 30 s. A direct test needs a throwaway Application and a git commit;
  deferred, because the retry-loop evidence already shows the operation staying open.

## S3: facts per scenario

Covered in the S2 table. Not tested: `kubectl argo rollouts undo` under Argo CD selfHeal
(needs an Argo-managed Rollout). The expectation in the ADR stands as unverified.

## S4: reach the dev relay from the Rollouts controller's namespace

A curl pod in `argo-rollouts` on kind-prod reached `http://192.168.1.78:30880/outcome/kind-prod`
(the dev relay NodePort) with `HTTP 405` for GET in 1.6 ms. There is no NetworkPolicy in
that namespace. By IP it works; by `dev.kiac.local` it would not (pods do not resolve
host `/etc/hosts` names). The webhook URL therefore needs the IP (which changes when the
dev VM IP changes, as `refresh-kiac-hosts.sh` already handles for `relayHostAliasIP`) or a
`hostAliases` entry on the single `argo-rollouts` Deployment. A real cluster uses a real
hostname.

## S5: Argo CD notifications for sync failure

kind-prod runs **two** Argo CD instances: `argocd` (platform) and `argocd-apps` (tenant
Applications, where the five release-tracked apps live). `argocd-apps` already has a
running `argocd-apps-notifications-controller` and an `argocd-notifications-cm` holding
only `context`. So the failure-only trigger needs config and a subscription, not a new
controller. It uses the same notifications engine verified in S2. Not tested live: a
sync-failure trigger firing (needs a PR to the cluster gitops repo, or a throwaway
Application).

## What to change in the ADR

1. Add rules A and B to decision 1 (trigger guard; reducer ignores unchanged-hash facts).
2. Replace "Delivery is at-least-once" in decision 6 with: **delivery is at-most-once per
   fact and failure is silent**; heartbeat facts and the stall alert are required, not
   optional.
3. Note two Argo CD instances on kind-prod; the failure trigger goes on `argocd-apps`.
4. Keep status Proposed until the heartbeat redelivery is confirmed and the
   Argo CD OutOfSync question is checked.

## Open items

- **Heartbeat redelivery:** a trigger keyed on `string(time.Now().Unix() / 60)` was
  installed on kiac-dev. A poke produced facts immediately. Whether the 15 minute resync
  sends one on its own, and whether the `notified` annotation keeps growing, is being
  watched for the 23:29 resync.
- **Argo CD OutOfSync from `notified.notifications.argoproj.io`:** needs one real
  Argo-managed Rollout.
- **Spike cleanup on kiac-dev:** `spike-rollouts` namespace, the
  `argo-rollouts-notification-configmap`, and the `catcher-token` key in
  `argo-rollouts-notification-secret`. Left in place while the heartbeat watch runs.
