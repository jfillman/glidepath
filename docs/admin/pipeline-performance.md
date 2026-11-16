# Pipeline performance

*Pipeline performance review, 2026-10-08, implemented and closed 2026-10-09.* What a stage costs now, what
changed and why, and the levers that are still open. The measurements come from Tekton Results: 1,596
PipelineRuns and 7,414 TaskRuns from 2026-09-17 to 2026-10-09, read through the relay proxy (see
[Measuring it again](#measuring-it-again)).

## The shape of a stage

Every stage Pipeline (`build`, `test`, `deploy`, `release`) has the same frame:

| Pod | What it does |
|---|---|
| `preflight` | Mints the chain id and traceparent, begins the stage span, sends `pipelinerun.started`, clones the app repo when the stage has a workspace, validates `cicd.yaml` (or passes forwarded `config_json` through). One step. |
| the stage's own tasks | `build-source`, `build-image`, `run-tests`, `deploy-manifests`, `open-release-pr`, ... |
| `debrief` (finally) | Ends the stage span and, on the flow's last stage, the flow-root span; notifies Slack and Backstage; sends the stage's domain event and `pipelinerun.finished`. Every step is `onError: continue`. |

This replaced eleven single-purpose Tasks per stage (clone, validate, span start and end, notify, events). A
smoke-fn build went from 13 pods and 258-409 s to 4 pods and 129 s.

**Two limits shaped that design.** Keep both in mind when adding work to `preflight` or `debrief`:

- **Task results share the termination message.** The kubelet gives a pod 12 KiB of termination messages in
  total, split across every container including Tekton's two init containers. A Task with N steps gets
  12288/(N+2) bytes per step, not 4 KiB. `preflight` writes 17 results, so it is one step and emits
  `config-json` compactly.
- **A compact JSON step result substitutes as an empty string.** Tekton 1.15 parses a step result that looks
  like a JSON object or array as that type, so `$(steps.x.results.y)` in a string param becomes `""`.
  `debrief` hands JSON between steps base64-encoded (`*-b64` params on the StepActions).

## Shared services and settings

| What | Where | Why |
|---|---|---|
| `trivy-server` | `glidepath-control-plane`, `platform-system` | Holds Trivy's vulnerability DB (~120 MiB). `image-scan` runs one JSON analysis against it and renders the log table with `trivy convert`. If the server is down the scan runs standalone, as before. Scan step: 4 s against the server, 14 s standalone. Its tag must equal `TRIVY_VERSION` in `catalog/toolbox/Dockerfile`. |
| Argo CD refresh after a Ground deploy | `deploy-manifests`; grant in `glidepath-app` `templates/env/argocd-refresh-rbac.yaml` | `argocd-apps` polls git every 120 s plus up to 60 s of jitter. `deploy-manifests` annotates `<app>-<env>` with `argocd.argoproj.io/refresh=hard` after its push; Argo CD picks it up in about 4 s. |
| `pipeline-runner-application-refresh-only` | `glidepath-control-plane` ValidatingAdmissionPolicy | RBAC cannot limit a patch to one field. The policy lets a `pipeline-runner` ServiceAccount change only the refresh and `hangar.io/dora-*` annotations on an Application, never its project, destination, sync operation or labels. |
| `coschedule: disabled` | Cluster repo `50-platform-cicd/tekton-operator/tektonconfig.yaml` | No Affinity Assistant StatefulSet and placeholder pod before each build and test run (10-25 s each). Use `pipelineruns` on a multi-node cluster with network-attached `ReadWriteOnce` storage. |
| Dependency caches | `build-cache-<app>` PVC | One directory per tool (npm, yarn, Maven, ...), not one per lockfile hash, so a lockfile change keeps the warm cache. `run-tests` mounts the npm/yarn cache too. |

[installation.md](installation.md#operational-notes) covers the install-time side of each.

## Step resources

Every Tekton container now carries a CPU and memory request (2026-10-09). Before that no step set one, so
every step pod was BestEffort. The kubelet puts all BestEffort pods in one cgroup with the minimum CPU weight,
so under load the whole pipeline got less CPU than any single pod with a request. The Semgrep scan step ranged
from 5 to 239 s on repos of a few files, following node CPU load.

| Where | Request | Applies to |
|---|---|---|
| TektonConfig `options.configMaps.config-defaults` (cluster repo) | 25m CPU, 32Mi | Every container that sets none, including Tekton's init containers |
| `build-source` `run-build-script`, `run-tests` `unit-test`, `build-artifact` `build` | 250m, 256Mi | The app's own build and test work |
| `build-image` `build` | 500m, 512Mi | Image build to an OCI layout; both platforms in parallel in one step |
| `push-image` `push` | 100m, 128Mi | Validates the layout and copies it to the registry (hashing and upload, no build) |
| `sast-scan` `scan` | 200m, 256Mi | Semgrep |
| `image-scan` `scan` | 100m, 256Mi | Trivy client (the server does the DB work) |
| `generate-sbom` `generate-and-attest` | 100m, 128Mi | Trivy SBOM + cosign |

These are requests only, with no limits, so nothing is throttled or OOM-killed because of them. A request is
a scheduling reservation, though, and a pod's step requests add up even though its steps run one at a time.
The dev node had about 3.2 of its 11 cores unrequested when this was set. One build at its widest
(`build-source`, `unit-test` and `sast-scan` in parallel) reserves about 0.8 cores. Several builds at once can
leave a pod Pending until another finishes. That queueing is the intended trade, and it counts against the
TaskRun timeout. Raise a step's request only together with the node's headroom.

## Release gates

A gitops release PR runs eight gate PipelineRuns.

- **The image gates do not clone.** `sast`, `image-scan`, `sbom` and the image half of `provenance` share the
  `resolve-promoted-image` StepAction: the PR's base, the files changed between that base and the head commit,
  and the one manifest, all over the GitHub API. A release branch with more than one commit resolves too.
  `verify-commit-signature` reads the PR's commits for the trailers the same way, and `validate-values`
  fetches the PR head one commit deep.
- **Release Record PRs do not trigger gates.** Tower's `release-record-*` branches only add
  `releases/<app>@<version>.yaml`. They used to match `startsWith("release-")` and made up 135 of 284 failed
  `extract-promoted-image` runs.
- **`image-promotion` holds no pod.** The polling stub was retired
  ([ADR-0025](adr/0025-retire-image-promotion-stub-check.md)); the real check (ADR-0029, catalog
  `promotion.enabled`) is started by the last required gate's `finally` and runs once, in the catalog namespace
  ([registries.md](registries.md#the-fan-in-phase-3)).

Details are in [release-guardrails.md](release-guardrails.md).

## Queueing gate runs

A release pull request in an app's gitops repository starts ten gate PipelineRuns in the same second
(`clearance-gate`, `policy-validation`, `values`, `itsm`, `image-scan`, `provenance`, `qa`, `sast`, `sbom`,
`api-compat`). On the single-node lab that burst (2026-11-15/16, three times) made task resolution take five minutes,
filled etcd's log with slow-apply warnings, and let several gates reach their PipelineRun timeouts before doing any work.
The same PR retested one gate at a time passed.

`pipelinesAsCode.concurrencyLimit` sets `spec.concurrency_limit` on the app's two Pipelines-as-Code `Repository` CRs (the
app repo and `gitops-<app>`, both in `app-<app>-cicd`). Unset (the default) renders no field, so a cluster behaves as
before. `pipelinesAsCode.appRepoConcurrencyLimit` and `gitopsRepoConcurrencyLimit` override it for one repository;
`extraGitopsRepos` are never limited. A limit is a whole number of at least 1, anything else fails the render.
The control plane's `pipelinesAsCode` values forward the cluster's setting to every app (a forwarded value wins over
one in an app's `cicd.yaml`); Apron sets it from `cluster.yaml` `glidepath.gateConcurrency`.

How it behaves (Pipelines-as-Code v0.52.0, Tekton Pipelines v1.15.0):

- **The limit is per Repository CR**, across all events, not per pull request. The app repo and the gitops repo each
  have their own queue.
- **A queued run is a pending PipelineRun.** With a limit set, PaC creates every run of the repository with
  `spec.status: PipelineRunPending` and the label and annotation `pipelinesascode.tekton.dev/state: queued`. When a slot
  frees, its watcher clears `spec.status` and sets the state to `started`. PaC starts the runs of one event in
  alphabetical order of run name; the names begin with the gate's name, so `api-compat`, `clearance-gate`, `image-scan`
  start first at a limit of 3. Order does not matter to the promotion fan-in, which every gate's `finally` feeds.
- **The timeout clock starts when the run starts.** Tekton sets `status.startTime` on the first reconcile after the run
  is no longer pending, and every timeout (`tasks`, `finally`, `pipeline`) counts from it. Time in the queue does not
  count, so no gate timeout changes. At a limit of 3, ten gates run in about four waves (an estimate, not measured): wall
  time is roughly four times the slowest gate in a wave, and each gate still has its own full budget.
- **The GitHub check reads `queued`** (title "Queued"/"Pending") until the run starts, then "CI has Started".
- **`/retest <gate>-` creates new runs through the same path**, so they queue behind what is running. `/cancel`
  patches `spec.status: CancelledRunFinally`, which Tekton accepts on a pending run, so a queued run can be cancelled and
  its slot goes to the next. PaC's cancel-in-progress is not supported with a concurrency limit and stays off here.
- **A rerun through glidepath-api bypasses the queue (known limit).** The API's rerun drops `spec.status` and the
  Pipelines-as-Code annotations and labels, and the watcher only manages runs that carry the state label. A rerun
  starts at once and is not counted against the limit. Do not rerun ten gates through the API at once; use `/retest`.
- **Tower shows a queued run as running.** Tekton gives a pending run the condition `Succeeded=Unknown`, reason
  `PipelineRunPending`, which glidepath-api's run model reports as status `running` with that reason. There is no
  `queued` status yet.
- **Admission.** `cicd-run-shape` does not look at `spec.status` or the state label, so the pending create, the patches
  and the watcher's start are all admitted (tested in `cicd_run_admission_test.py`).
- **The stalled-pipeline detector does not look at queued runs.** See
  [stalled-pipeline-detector.md](stalled-pipeline-detector.md#what-this-is-not).

### If a repository's queue stalls

The queue lives in the PaC watcher's memory and is rebuilt from the `state` labels when it starts. A leaked slot (the
watcher believes a run is still running when it is not) stops the repository's later runs from starting: queued runs stay
`PipelineRunPending` with nothing running. Recovery is to restart the watcher:

```
kubectl -n tekton-pipelines rollout restart deployment/pipelines-as-code-watcher
```

The watcher has one replica on the lab, so queued runs wait for it to come back (seconds), then the queue is rebuilt from
the labels. Nothing is lost. To look before restarting, set `PAC_ENABLE_QUEUE_DEBUG=true` on the watcher and read
`/debug/queue` (unauthenticated: turn it off afterwards; see the Pipelines-as-Code concurrency docs).

## Other changes

- The write-side Tasks (`deploy-manifests`, `open-release-pr`, `bump-manifest-pr`, both
  `deliver-onboarding-files` clones) clone one commit deep. `resolve-deploy-target` reads `cicd.yaml` over
  the contents API. An unknown revision still fails the Task.
- Semgrep runs with `--metrics=off`. The rule packs are still fetched from the registry. The fetch takes
  under a second. The packs are under the Semgrep Rules License v1.0, so they are not vendored into this
  public repo or the public toolbox image.
- A push whose HEAD is a merge commit no longer hides a real change from Pipelines-as-Code. The push
  exemption reads every commit in the push (`body.commits`), not PaC's HEAD-only `files.all`.
- The merge of an app's first onboarding PR builds (2026-10-17): it is recognised by a commit adding
  `.tekton/onboarding-resync.yaml`, which only the first onboarding does. Before, every file in it was exempt and a
  new app waited for someone to push code. `hack/ci/build-exemption-cel` checks the expression with cel-go against
  15 push payloads in catalog-ci, including that a `cicd.yaml` change never builds, directly or through its resync.

## Still open

- **amd64 builds under QEMU** (review item 1, partly addressed). `build-image` moved from kaniko to BuildKit
  on 2026-10-17 ([ADR-0026](adr/0026-buildkit-image-builds.md)): both platforms build in parallel in one
  step, and a Containerfile that compiles in a `FROM --platform=$BUILDPLATFORM` stage cross-compiles natively
  instead of emulating (a Go scaffold-shaped build for both platforms took 37 s). RUN steps in target-platform
  stages still emulate, so an app that builds inside its Containerfile without `$BUILDPLATFORM` (Backstage:
  11 of its 25 minutes in `build-image` under kaniko) is still slow until its Containerfile opts in. Deriving
  the platforms from the clusters an app deploys to still needs an `arch` field in the cluster registry
  ([ADR-0024](adr/0024-cluster-taxonomy-zone-roles-tier.md)).
- **The dev cluster's control plane is under pressure.** `kube-scheduler` and `kube-controller-manager`
  restart on lease timeouts with node memory around 86%. That sets the floor under every number here.
- **Queueing is off until the cluster sets it.** A burst of pushes starts every build at once, and a release PR starts ten
  gates at once. See [Queueing gate runs](#queueing-gate-runs) for the setting. On a small node (the lab's is one node,
  11 CPUs) set it: `glidepath.gateConcurrency: 3` in Apron's `cluster.yaml` (the dev profile sets it).

## Measuring it again

Tekton Results holds every finished run; the relay in `platform-system` proxies its REST API:

```bash
kubectl get --raw "/api/v1/namespaces/platform-system/services/tekton-results-relay:8080/proxy/apis/results.tekton.dev/v1alpha2/parents/-/results/-/records?page_size=500&filter=data_type=='tekton.dev/v1.PipelineRun'"
```

Records are base64 JSON in `.records[].data.value`. Page with `page_token=<next_page_token>`. Use
`data_type=='tekton.dev/v1.TaskRun'` for per-task and per-step timings
(`.status.steps[].terminated.startedAt/finishedAt`).
