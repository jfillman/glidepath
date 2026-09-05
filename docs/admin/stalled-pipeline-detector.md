# Stalled-pipeline detector

Detects a stage that completed successfully but whose expected next-stage PipelineRun
never appeared - meaning a CDEvent got lost somewhere between that stage's `finally`
block and the broker's Trigger actually firing. This session hit exactly this failure
class repeatedly and manually (the recurring cloudflared tunnel dying with nothing
downstream to notice a PR sat unbuilt) - this closes that gap with an automated check.

## What this is not

The architecture plan originally bundled this with "CDEvents idempotency/dedup." Live
code inspection found the dedup half was already built during Phase 1:
`charts/platform-cicd-app/templates/triggers/*.yaml` names every triggered
PipelineRun deterministically (`ci-1-test-brave-otter-a1b2c3d4`, etc. - see
docs/admin/naming-conventions.md), and the trailing `context.id` segment is itself
`sha256(emitting-PipelineRun-name:event-type)[:8]`, computed in
`catalog/lib/cdevents.sh`'s `cdevent_send()`. At-least-once redelivery of the same
CDEvent therefore either creates the PipelineRun once or hits a harmless
`AlreadyExists`. This detector covers the opposite failure: a stage finishing and
**nothing** happening next.

## Detection mechanism

Uses two labels every flow-generated PipelineRun carries - `platform.io/flow` and
`platform.io/step-index` (stamped by `deliver-onboarding-files.yaml` for a flow's
git-rooted first step, and `flow-triggers.yaml` for every event-chained step after it -
see docs/tracing.md's "Which stage closes the flow-root span") - plus the
`is-flow-terminal` Pipeline param every `catalog/pipelines/*.yaml` accepts. For each
completed stage that is NOT flagged `is-flow-terminal: "true"` (a terminal step
correctly has no successor - not a stall), the detector checks whether any PipelineRun
exists in the same namespace labeled with this flow's name and `step-index + 1`. No name
prediction, no hash reconstruction - just "did the next step run."

A successor is only ever expected when the completed PipelineRun's own Tekton condition
is `status: "True"`, reason `Succeeded`/`Completed` - matching exactly what already gates
`send-cdevent` in each stage Pipeline. A **failed** stage is a different, already-visible
condition (shows up directly as a Failed PipelineRun, and `notify-slack` already fires on
it) - not a silent stall, so it's deliberately excluded.

**Real bug, found and fixed 2026-08-11**: the original version of this detector *did*
reconstruct the expected successor's name, as `sha256("<this-run-name>:<event-
type>")[:20]` prefixed with a hardcoded next-stage name (`build`->`test`, `test`-
>`deploy`, `deploy`->`release`) - `cdevent_send()`'s own hash, duplicated here since that
script hard-requires several env vars this CronJob has no reason to set. That scheme
predates Phase 3 item 7's multi-flow work, which changed the real event-chained naming
scheme to `<flowName>-<index>-<stageName>-<eventID>` (to avoid collisions between flows
and repeated stages within one flow) - the CronJob's guess never matched either that or a
git-rooted PaC `generateName`, so `kubectl get pipelinerun "${expected_name}"` always
came back empty and **every** completed build/test/deploy run got falsely flagged
`platform.io/stall-alerted: "true"`, not just genuinely-stalled ones - confirmed live,
including runs with real, existing successors. The label-based check isn't coupled to
`cdevent_send()`'s naming scheme at all any more, and it also now covers `release`
(previously never checked, even though an event-chained release can have real
successors - e.g. `release -> test`).

## Scan scope, threshold, and dedup

Scans **cluster-wide** (`pipelineruns.tekton.dev` across every namespace) rather than
discovering Application namespaces via a label, because no such label convention exists
anywhere in this platform yet - the broker's own `EventListener` already takes the same
`namespaceSelector: {matchNames: ["*"]}` approach (`eventlistener.yaml`), relying on
RBAC/Trigger-CR scoping rather than a namespace allowlist. This follows that precedent.

A candidate stage must be older than `STALL_THRESHOLD_MINUTES` (default 10, by
`.status.completionTime`) before being flagged - long enough that ordinary image-pull/
scheduling latency doesn't false-positive.

Once alerted, the detector labels the *stalled* (predecessor) PipelineRun
`platform.io/stall-alerted: "true"` and skips already-labeled runs on future scans -
state lives on the object itself, no new datastore.

## Alerting - deliberately not the per-app Slack webhook

`notify-slack.yaml` reads each Application's own `slack-webhook-url` Secret, mounted into that
Application's own pipeline pods. Reusing it here would mean granting this cluster-scoped
detector `get` on Secrets across *every* Application's namespace just to fetch webhook URLs - a
real, avoidable widening of blast radius this platform has been careful about everywhere
else (TokenReview-scoped brokering, per-app impersonation, read-only Dashboard RBAC),
for a payoff that isn't worth that cost.

Instead, a genuine stall produces:
1. A plain Kubernetes `Event` (core `v1`, `involvedObject` pointing at the stalled
   PipelineRun) - visible directly via `kubectl describe pipelinerun <name>` and
   `kubectl get events -n <app-namespace>`.
2. A structured `STALL DETECTED` log line to stdout, which the platform's existing Loki
   collection already ingests - e.g. `{namespace="platform-system", pod=~"stalled-
   pipeline-detector.*"} |= "STALL DETECTED"` surfaces every stall across every Application in
   one Grafana Explore query, with no new wiring.

Piping this into an Application's Slack channel is a small, clearly separable follow-up (e.g. a
Grafana Loki alert rule) - not built now, since it would reopen the Secrets-RBAC question
above for a feature nobody's asked for yet.

## RBAC

Cluster-scoped by necessity (PipelineRuns live in every Application's namespace, same
unavoidable exception as the TTL sweep's namespace access), narrow by verb: `get`/`list`/
`patch` on `pipelineruns.tekton.dev` (patch only for the dedup label), `create` on
`events`. Nothing else - no Secrets, no other resource types, no write access to a
PipelineRun's actual spec/status.

## Verification

- Live-tested by temporarily appending `&& false` to the `on-build-success` Trigger's CEL
  filter in an Application namespace (not by scaling the shared `EventListener` to zero -
  that would make `send-cdevent`'s own `curl` fail, which by Tekton's default
  `finally`-task semantics flips the *whole* PipelineRun to `Failed`, which this detector
  correctly excludes as an already-visible condition, not a silent stall - so that
  approach wouldn't actually exercise the detector at all). This makes the broker accept
  and 200 the CDEvent while silently dropping the match - the real target failure mode.
- Confirmed a real `build` PipelineRun completes `Succeeded` with no corresponding
  `test-*` PipelineRun ever appearing.
- Manually triggered the CronJob (`kubectl create job --from=cronjob/stalled-pipeline-
  detector ...`); confirmed it detects the stall, creates the `Event`, and applies the
  dedup label.
- Re-ran immediately; confirmed the same stall does **not** re-alert.
- Restored the Trigger's original CEL filter afterward.
- False-positive check (original version, since found wrong - see "Real bug" above): at
  the time, verifying the hash recomputation against a couple of real `build`->`test`
  pairs raised zero alerts. That check didn't survive Phase 3 item 7's naming-scheme
  change and should have been re-run after - every completed build/test/deploy run was
  actually being falsely flagged by the time this was caught live, 2026-08-11.
- RBAC check: `kubectl auth can-i --list
  --as=system:serviceaccount:platform-system:stalled-pipeline-detector` shows exactly
  `get`/`list`/`patch` on `pipelineruns.tekton.dev` and `create` on `events`.
