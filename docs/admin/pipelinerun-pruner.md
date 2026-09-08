# PipelineRun pruner

Deletes old, completed PipelineRuns (and their TaskRuns) so this platform's Tekton
objects don't accumulate forever. Tekton never garbage-collects a completed
PipelineRun on its own, and PaC's `pipelinesascode.tekton.dev/max-keep-runs`
annotation only prunes PaC-triggered PipelineRuns - a flow's git-rooted first step.
Every *event-chained* step (everything `charts/glidepath-app/templates/triggers/
flow-triggers.yaml`'s Tekton Triggers create - which is most PipelineRuns in a typical
multi-stage flow) had no pruning at all until this CronJob. A single heavy
flow-testing session can leave 100+ completed PipelineRuns behind, and this cluster's
single-node max-pods-per-node ceiling has already been hit by exactly that kind of
accumulation more than once (see [installation.md](installation.md)).

## Retention rule

PipelineRuns are grouped by `(namespace, tekton.dev/pipeline label)` - Tekton's own
automatic label naming which Pipeline produced a run (`build`, `test`, `deploy`,
`release`, `onboarding-resync`, `sast-check`, ...). No new labeling was needed; this was
confirmed live against real PipelineRuns before relying on it.

Within each group:

- The single **newest** completed run is never pruned, regardless of age - there's
  always at least one recent run per pipeline to inspect.
- Every other run in that group is deleted once it's older than `RETENTION_HOURS`
  (default 24).
- A still-**Running** PipelineRun can never be selected - only runs with
  `status.completionTime` set are candidates at all, so this is true by construction,
  not by a status check that could be wrong.
- `completionTime` is set for both `Succeeded` and `Failed` terminal conditions, so
  both count as prunable. This differs from
  [stalled-pipeline-detector.md](stalled-pipeline-detector.md), which deliberately
  treats a failed run as a separate, already-visible condition it doesn't alert on -
  that distinction doesn't matter for disk/pod-count cleanup.

## Deletion mechanics

TaskRuns are deleted explicitly, by Tekton's own `tekton.dev/pipelineRun` label,
**before** the owning PipelineRun - not left to ownerReference cascade-GC alone. A
prior heavy-testing session found cascade deletion unreliable under load (deleting the
PipelineRun didn't always clean up already-orphaned TaskRun pods); explicit TaskRun
deletion is what actually cleared them.

## Scope and schedule

Cluster-wide (`pipelineruns.tekton.dev`/`taskruns.tekton.dev` across every namespace),
same unavoidable exception `stalled-pipeline-detector-cronjob.yaml` already documents:
no Application-namespace-discovery label exists anywhere in this platform yet. Runs
hourly (`0 * * * *`) - fine-grained enough relative to a 24h retention window without
adding load.

## RBAC

`get`/`list`/`delete` on `pipelineruns.tekton.dev` and `taskruns.tekton.dev`. Nothing
else - no Secrets, no other resource types, no write access to a PipelineRun's own
spec/status (contrast `stalled-pipeline-detector`, which patches a dedup label - this
job only ever deletes or does nothing).

## Verification

- `kubectl get pipelinerun -A -o json | jq ...` run manually against the live cluster
  before writing the CronJob, confirming `tekton.dev/pipeline` is really present and
  correctly valued on real PipelineRuns (not assumed from Tekton's docs).
- The grouping/retention jq logic was extracted and run against a real snapshot of the
  cluster's PipelineRuns before deploying, cross-checked in Python (the age-threshold
  arithmetic, not `date -d`, since the validating shell was macOS/BSD - the CronJob
  itself runs GNU `date -u -d` inside the toolbox image, the same call already proven
  live in `stalled-pipeline-detector-cronjob.yaml`).
- `helm template`/`helm lint` the control-plane chart - CronJob/RBAC render correctly.
- Deployed live (`helm upgrade --install glidepath-control-plane`) and manually
  triggered (`kubectl create job --from=cronjob/pipelinerun-pruner ...`) against the
  real cluster. Result: pruned exactly 165 PipelineRuns, matching the pre-deploy
  prediction - `platform-cicd-demo` went from up to dozens of completed runs per
  pipeline down to exactly 1 (the newest) per `(namespace, tekton.dev/pipeline)` group,
  for every one of `build`/`test`/`deploy`/`release`/`sast-check`/`image-scan-check`/
  `provenance-check` (named `policy-check` at the time)/`sbom-check`. Confirmed the 54
  TaskRuns left behind all belong to one
  of those 8 retained PipelineRuns (zero orphans) - grouped by
  `tekton.dev/pipelineRun` label and cross-checked against `kubectl get pipelinerun`.
- Total cluster pod count dropped from 235 to 109 as a direct result (this cluster's
  single-node max-pods-per-node ceiling is ~110 - see
  [stalled-pipeline-detector.md](stalled-pipeline-detector.md)'s and this platform's
  bootstrap notes on that recurring failure mode). The stuck-scheduling state this
  caused for an unrelated pre-existing Job (`chains-config-patch`, a Helm post-upgrade
  hook) resolved immediately once cleared.
