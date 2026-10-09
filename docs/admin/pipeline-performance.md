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
- **There is no `image-promotion` gate.** It was a stub that polled for the others and could not block a
  merge. [ADR-0025](adr/0025-retire-image-promotion-stub-check.md) retired it and records where a real
  promotion belongs.

Details are in [release-guardrails.md](release-guardrails.md).

## Other changes

- The write-side Tasks (`deploy-manifests`, `open-release-pr`, `bump-manifest-pr`, both
  `deliver-onboarding-files` clones) clone one commit deep. `resolve-deploy-target` reads `cicd.yaml` over
  the contents API. An unknown revision still fails the Task.
- Semgrep runs with `--metrics=off`. The rule packs are still fetched from the registry. The fetch takes
  under a second. The packs are under the Semgrep Rules License v1.0, so they are not vendored into this
  public repo or the public toolbox image.
- A push whose HEAD is a merge commit no longer hides a real change from Pipelines-as-Code. The push
  exemption reads every commit in the push (`body.commits`), not PaC's HEAD-only `files.all`.

## Still open

- **amd64 builds under QEMU** (review item 1, deferred). Every image is built for both architectures, and
  the amd64 half runs emulated on the arm64 build node. A Backstage build spends 11 of its 25 minutes in
  `build-image`. The fix derives the platforms from the clusters an app actually deploys to and needs an
  `arch` field in the cluster registry ([ADR-0024](adr/0024-cluster-taxonomy-zone-roles-tier.md)).
- **Step pods are BestEffort.** No Tekton container sets requests, so under load they get the smallest CPU
  share. The Semgrep scan step varies from 5 to 239 s on repos of a few files, and that spread follows node
  CPU contention. Setting `default-container-resource-requirements` in Tekton's `config-defaults`, or
  requests on the scan steps, is the next lever.
- **The dev cluster's control plane is under pressure.** `kube-scheduler` and `kube-controller-manager`
  restart on lease timeouts with node memory around 86%. That sets the floor under every number here.
- **No PaC `concurrency_limit`.** A burst of pushes starts every build at once.

## Measuring it again

Tekton Results holds every finished run; the relay in `platform-system` proxies its REST API:

```bash
kubectl get --raw "/api/v1/namespaces/platform-system/services/tekton-results-relay:8080/proxy/apis/results.tekton.dev/v1alpha2/parents/-/results/-/records?page_size=500&filter=data_type=='tekton.dev/v1.PipelineRun'"
```

Records are base64 JSON in `.records[].data.value`. Page with `page_token=<next_page_token>`. Use
`data_type=='tekton.dev/v1.TaskRun'` for per-task and per-step timings
(`.status.steps[].terminated.startedAt/finishedAt`).
