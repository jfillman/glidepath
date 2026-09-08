# DORA metrics

Deployment Frequency, Lead Time for Changes, Change Failure Rate, and MTTR, computed by
`platform/dora-exporter` from ArgoCD's own confirmed release outcomes, exposed as
Prometheus metrics, and visualized in Grafana's `dora.json` dashboard.

## Why this reads ArgoCD, not the CDEvents stream

The architecture plan originally assumed the DORA exporter would be "a small stateless
Go service that subscribes as another consumer off the same shared broker" - a CDEvents
HTTP consumer, matching how every other stage of this platform's chain works. Working
through the actual metric definitions surfaced a real problem with that: CDEvents, as
this platform emits them today, can tell us a release **PR was opened**
(`charts/glidepath-catalog/templates/pipelines/release.yaml`'s `open-release-pr` task succeeding) - but nothing
confirms whether that PR was ever merged, or whether ArgoCD's sync of it actually
succeeded. All four DORA metrics need that confirmed outcome: Deployment Frequency and
Lead Time need to know a deploy really happened, not just that one was proposed; Change
Failure Rate and MTTR need to know whether it succeeded or failed. Counting "PR opened"
as "deployed" would silently count every abandoned or rejected promotion as a successful
deployment - worse than not having the metric.

Checked live, not assumed, whether something better than a guess already existed:

- **ArgoCD `Application.status`** is real and already wired into this release path.
  Confirmed against the actual running `nodejs-demo-app-staging` Application:
  `status.operationState.phase` (`Succeeded`/`Failed`/`Error`/`Running`),
  `.startedAt`/`.finishedAt`, and `status.sync.revision` are all populated and accurate.
- **Argo Rollouts**: the CRD (`rollouts.argoproj.io`) is installed cluster-wide, but
  `nodejs-demo-app-staging` deploys as a plain `Deployment`, not a `Rollout` - confirmed
  via `kubectl get deployment,rollout -n platform-cicd-demo-staging`. No Rollout object
  exists in this flow to read status from. Worth reconsidering if this platform ever
  adopts progressive delivery (canary/blue-green) - `Rollout.status.phase` would then be
  a strictly better signal than `Application.status`. Not built against that today
  because that infrastructure isn't actually part of this release path.
- **Crossplane XRD**: Crossplane is installed, but `kubectl get managed` returns nothing
  anywhere on the cluster, and the only `CompositeResourceDefinition` present
  (`applications.apps.example.org`) is an unrelated, inert example CRD (the same one
  that caused a `kubectl get application` naming collision earlier in this platform's
  build - see `docs/release.md`). The architecture plan's own Crossplane `Application`
  XRD is Phase 3 and doesn't exist yet.

So: **the DORA exporter watches `applications.argoproj.io` directly**, not the CDEvents
broker. This is a meaningfully simpler design than the original CDEvents-subscriber idea
- no new HTTP ingestion endpoint, no new TokenReview-audience plumbing, and critically,
**no changes anywhere near `catalog/lib/cdevents.sh` or `send-cdevent.yaml`** - the one
shared library every pipeline stage's `finally` block depends on for actual chaining.
Touching that for a metrics feature would be real blast-radius risk for no payoff; not
touching it removes the risk entirely.

## The correlation mechanism: annotations, not CDEvents or image matching

The exporter needs to know, for a given Application status update: "is this the sync I'm
waiting for, and what was its lead-time start anchor?" `Application.status.summary.images`
looked like the natural place to read the deployed image for correlation - checked live
against the real Application and it's empty (`{}`) on this ArgoCD install, not reliable.
Git revision doesn't work either: `open-release-pr.yaml` only knows the SHA of the branch
it pushed, not the merge-commit SHA GitHub assigns on merge.

Instead: **the release Pipeline stamps tracking annotations directly onto its own
Application's ArgoCD Application object**, and the exporter reads those back off the same object it's
already watching - no separate correlation store, no guessing. `charts/glidepath-catalog/templates/tasks/mark-
release-pending.yaml`, wired into `release.yaml` right after `open-release-pr` succeeds
(`runAfter: [open-release-pr]` - only runs if the PR was actually opened, via standard
Tekton DAG failure propagation, no `when` guard needed), does:

```bash
baseline="$(kubectl get application.argoproj.io "${app}" -n argocd \
  -o jsonpath='{.status.operationState.startedAt}' 2>/dev/null || echo "")"

kubectl annotate application.argoproj.io "${app}" -n argocd \
  hangar.io/dora-pending=true \
  hangar.io/dora-flow-start-time="${FLOW_START_TIME}" \
  hangar.io/dora-baseline-started-at="${baseline}" \
  hangar.io/dora-app-namespace="${APP_NAMESPACE}" \
  hangar.io/dora-app="${APP_NAME}" \
  --overwrite
```

`hangar.io/dora-baseline-started-at` is the detail that makes this correct rather than
approximately-correct: ArgoCD's `operationState` gets overwritten by *any* sync,
including its own unrelated `selfHeal` drift-correction syncs that run continuously
regardless of this pipeline. The exporter must not react to a sync that was already
sitting there, finished, before this particular promotion even started. Stamping the
*current* `operationState.startedAt` as a baseline at pending-time, and only reacting
once a *strictly newer* `startedAt` appears with a terminal phase, makes this precise
regardless of how many unrelated syncs happen in between.

On each Application watch event (`platform/dora-exporter/cmd/dora-exporter/main.go`'s
`reconcile()`), if `hangar.io/dora-pending: "true"` is present and
`status.operationState.startedAt` is after `dora-baseline-started-at` and `phase` is
terminal:

- **`Succeeded`**: record a successful deployment (see below), then patch the
  Application to clear `dora-pending` (and `dora-last-failure-time`, if present - see
  MTTR below). This clearing is the dedup mechanism - the same shape as the stalled-
  pipeline detector's own `hangar.io/stall-alerted` label
  (`docs/stalled-pipeline-detector.md`), just annotations on a different resource. Once
  cleared, the next watch event for that same object carries no `dora-pending`
  annotation, so `reconcile()`'s own early return makes it a no-op - no separate
  in-memory dedup tracking needed.
- **`Failed`/`Error`**: record a failed release, patch to clear `dora-pending` and set
  `hangar.io/dora-last-failure-time` to `status.operationState.finishedAt` (consumed by
  the *next* confirmed success, for MTTR).

RBAC for `mark-release-pending` is `pipeline-runner` (the Application's own SA) granted
`get`+`patch` on exactly its own `<app-name>-staging` Application, `resourceNames`-
scoped, added to `charts/glidepath-app/templates/argocd/release-application.yaml` (the file that
already sets up this Application's release-stage ArgoCD RBAC) rather than a new template file.
That same file also adds `hangar.io/dora-track: "true"` to the `Application` resource
itself - a stable, explicit marker the exporter's informer filters on
(`LabelSelector: "hangar.io/dora-track=true"`), so it never processes unrelated
Applications in the `argocd` namespace (e.g. the pre-existing `podinfo-demo-app`
Application, which isn't part of this platform's Application model at all).

Separately, and **not load-bearing for any of this**: `release.yaml` also now sends a
`dev.cdevents.change.created.0.3.0` event (CDEvents' actual vocabulary for "a change/PR
was created", not a reuse of `service.deployed` for something that isn't deployed yet) in
its `finally` block, purely so "release" shows up in Tempo/`pipeline-detail.json` the same
way build/test/deploy already do. The DORA exporter does not consume this event - it's a
trace/dashboard completeness addition, kept deliberately separate so the two mechanisms
never get conflated.

## How each of the 4 metrics is actually computed

**1. Deployment Frequency** - "how often does this app successfully deploy." Every
confirmed `Succeeded` outcome increments `dora_deployments_total{app_namespace, app}`, a plain
Counter. The exporter does no rate/frequency math itself - Grafana computes actual
frequency via `increase(dora_deployments_total[...])` over whatever window a panel picks
(the dashboard uses daily buckets over a 30-day window), which is the normal way a
counter becomes "how often" in Prometheus, not something to precompute and bake in.

**2. Lead Time for Changes** - "time from commit to running in production." The start
anchor is `flow-start-time`, established once at `build`'s `start-flow-root-span` (the
moment the very first stage of this commit's whole flow began) and threaded unchanged
through every CDEvent's `customData.platform.flow_start_time` since -
`mark-release-pending` just copies a value that already exists all the way from
`release.yaml`'s own `$(params.flow-start-time)`, no new plumbing needed to compute it.
The end anchor is `status.operationState.finishedAt` at the moment of confirmed
`Succeeded`. Sampled into `dora_lead_time_seconds{app_namespace, app}`, a **Histogram** (not a
gauge or summary) with bucket boundaries deliberately aligned to DORA's own published
elite/high/medium/low bands, so `histogram_quantile()` in Grafana directly shows which
band most changes fall into: `3600` (1h), `86400` (1d), `604800` (1w), `2592000` (1mo).

**3. Change Failure Rate** - "what fraction of releases require remediation." Every
confirmed terminal outcome (both branches above) increments
`dora_releases_total{app_namespace, app, outcome="succeeded"|"failed"}` - one counter with an
`outcome` label, rather than two separately-named counters that would need summing
anyway. Grafana computes the percentage:
`sum(increase(dora_releases_total{outcome="failed"}[...])) /
sum(increase(dora_releases_total[...]))`. **Caveat, shown directly on the dashboard, not
just here**: this measures **release-process failure** (the GitOps promotion itself
failed to apply) - this platform has no incident-tracking or production-health-signal
system, so it structurally cannot detect "the release succeeded but the code itself
caused a production problem," which is DORA's fuller definition of change failure. The
same honest gap the architecture plan already called out for MTTR now applies
consistently to this metric too, rather than being swept under the rug.

**4. MTTR (Mean Time to Restore)** - already flagged in the original architecture plan as
best-effort/experimental, because "a manual-rollback-outside-the-pipeline blind spot"
means this platform can't see a real incident or a manual fix applied outside a pipeline
run. What it *can* see: the gap between a confirmed `Failed` release for an app and the
*next* confirmed `Succeeded` one for that same app - a real, if approximate, "time to
next green" proxy. On a confirmed success, if a prior failure is on record,
`finishedAt - <last failure time>` is sampled into
`dora_time_to_restore_seconds_experimental{app_namespace, app}` before that record is
cleared. The `_experimental` suffix is deliberate and structural, not just a dashboard
note - matches this platform's existing "make reduced-confidence data loud in the data
itself, not just in code comments" precedent (the `governance.stub=true` span attribute
from Phase 1). Buckets are sized for human-response-time scale, not deploy-time scale:
`300` (5m), `1800` (30m), `3600` (1h), `14400` (4h), `86400` (1d), `604800` (1w).

**Where "last failure time" is recorded differs by path** - see "MTTR for
cluster-mapped envs" below for why, and for the real gap this closed 2026-08-22 (MTTR
used to be dead for cluster-mapped apps entirely).

## MTTR for cluster-mapped envs (fixed 2026-08-22)

Path 2's `/argocd-outcome` handler used to pass an empty `lastFailureTimeStr` into
`recordOutcome` unconditionally - MTTR was a known, dead gap for every cluster-mapped
app (`checkout-api`/`kind-prod` included), because `dora-exporter` runs on the dev
cluster and has no live API access to a remote cluster's `Application` object to patch
a `dora-last-failure-time` annotation onto, the way path 1 does.

Fixed not by getting remote API access (that would repeat the exact "dev-cluster-
resident credential capable of touching an upper cluster" blast-radius shape this
platform's broker/relay design has rejected everywhere else, see
[multi-cluster.md](multi-cluster.md)) but by giving `dora-exporter` its own state store
on the cluster it's *actually* running on: `dora-cluster-mapped-state`, a ConfigMap in
`platform-system` it owns itself, keyed `<appNamespace>.<appName>`. On a confirmed
`Failed`/`Error`, `handleArgoCDOutcome` patches that key to the finish time; on a
confirmed `Succeeded`, it reads the key back (same `recordOutcome` MTTR-sampling logic
path 1 already used), then clears it. Same JSON-merge-patch idempotency pattern as
`patchAnnotations` below, just against a ConfigMap's `data` map instead of an
Application's annotations.

The ConfigMap itself is rendered by the Helm chart with metadata only, deliberately no
`data:` field at all (not even `data: {}`) - see that manifest's own comment for why
declaring the field, even empty, would let a later `kubectl apply`/ArgoCD sync prune
every key `dora-exporter`'s own PATCH calls have added since.

This only works because the call into `/argocd-outcome` now happens as a Tekton Task
(`update-dora-metrics.yaml`, run from `release-outcome-notify`'s own PipelineRun) rather
than a direct HTTP call from `argocd-outcome-relay` itself - not because a Task can do
anything a Go service couldn't, but because moving the call is what prompted noticing
`dora-exporter` already had everything it needed to close this gap on its own cluster,
and gave a natural home to the code that did it. See `multi-cluster.md`'s
"relay generified" section for the full reasoning on why the call moved.

## Durable counters, not just in-memory ones (fixed 2026-08-22)

Every metric this service exposes (`dora_deployments_total`, `dora_lead_time_seconds`,
`dora_releases_total`, `dora_time_to_restore_seconds_experimental`) is an ordinary
`client_golang` collector - it only ever lived in this process's own memory. Nothing
behind it was durable except the MTTR fix above, and that only covers "last failure
time," not the counters/histograms themselves. A routine pod replacement - a redeploy,
a node drain, an image update - silently zeroed every app's entire history, for both
input paths, with no error anywhere: Prometheus only ever scrapes whatever this process
currently holds (pull, not push - see "Accessing the metrics and dashboard" below), so
once the process restarts, there's nothing left to scrape but zero.

**Found live, the same day as the MTTR fix above**: `checkout-api` had two real,
ArgoCD-confirmed releases earlier that day, both correctly recorded by the
then-running exporter pod - and both vanished from the dashboard the moment this
Deployment rolled a new `ReplicaSet` (deploying the MTTR fix itself), because
Prometheus's own scrape history for the *old* pod's `instance` label doesn't carry
forward to a new pod with a new IP, and the new pod's in-memory counters started over
at zero. Only the one release that happened to land *after* the restart showed up.

Fixed the same way the MTTR gap above was fixed: a second ConfigMap this service owns,
`dora-metrics-state` (`platform-system`, same `<appNamespace>.<appName>` key shape as
`dora-cluster-mapped-state`), durably mirrors every counter increment and histogram
observation for **both** input paths - not just the cluster-mapped one. Each value is a
small JSON blob (`deployments`, `releasesSucceeded`, `releasesFailed`, plus a
`leadTimeObservations`/`mttrObservations` array of raw historical values). The array
shape, not just a running sum, is deliberate: `client_golang`'s `HistogramVec` has no
public API to set a histogram's cumulative bucket/sum/count state directly, so the only
way to reconstruct the exact same distribution after a restart is to persist every raw
observation and replay it through `Observe()` again - `primeMetricsFromState`
(`cmd/dora-exporter/main.go`) does exactly that, once at startup, for every app it
finds, **before** the informer or the HTTP server can process a single live event (so a
fresh event and a priming read can never race and double-count the same observation).
Unbounded array growth is a real, accepted tradeoff - real release volume on this
platform is a handful of events per app per day, nowhere near where a ConfigMap's 1MiB
limit would become a concern.

Same "no `data:` field at all" rendering as `dora-cluster-mapped-state` - see that
ConfigMap's own comment (`charts/glidepath-control-plane/templates/dora-exporter/deployment.yaml`)
for why declaring the field, even empty, would let a later sync prune every key this
service's own PATCH calls have added since.

Live-verified on `kind-dev`: posted a synthetic outcome at `/argocd-outcome`, confirmed
it landed in both `/metrics` and `dora-metrics-state`, restarted the exporter pod
(`kubectl rollout restart deployment dora-exporter`), and confirmed `/metrics` showed
the full pre-restart counts intact - deployment count, release outcome, and the lead
time histogram's exact sum/count - **before** any new event arrived, with the pod's own
startup log (`rehydrated 1 deployment(s), 1 succeeded/0 failed release(s) from
dora-metrics-state`) confirming the replay path actually ran. One real limitation this
surfaced: rehydration only ever recovers what a *post-fix* exporter itself wrote to
`dora-metrics-state` - `checkout-api`'s and `smoketest`'s in-memory counts from earlier
2026-08-22 (recorded by the pre-fix binary, which never wrote to this ConfigMap) did not
carry forward across the restart that deployed this fix. Going forward, every outcome is
durable; the specific counts lost before this fix shipped are not recoverable.

## RBAC

`dora-exporter`'s own ServiceAccount (in `platform-system`, matching where the other
shared platform-level components live) gets two namespaced `Role`s, not `ClusterRole`s -
genuinely narrower than the stalled-pipeline detector's necessarily cluster-scoped RBAC:

- In `argocd`: `get`/`list`/`watch`/`patch` on `applications.argoproj.io`, since every
  tracked Application lives in that single namespace regardless of which Application it
  belongs to. `patch` is a deliberate widening beyond read-only, same honest framing as
  the stalled-pipeline detector's own dedup-label `patch` grant
  (`docs/stalled-pipeline-detector.md`): Kubernetes RBAC can't scope `patch` down to
  "only annotations," so this identity can technically modify any field on any
  Application in `argocd` - narrow by namespace and resource type, not by field.
- In `platform-system`: `get`+`patch` on `configmaps`, `resourceNames`-scoped to exactly
  `dora-cluster-mapped-state` (the MTTR state store above) and `dora-metrics-state` (the
  durable counters store above) - nothing else. No `create`: both ConfigMaps are always
  pre-rendered by this same chart, so this identity never needs it.

`pipeline-runner` (each Application's own SA) additionally gets `get`+`patch` on exactly its
own `<app-name>-staging` Application via `resourceNames` - nothing else, no other
Application's ArgoCD Application object, no other resource type.

## Accessing the metrics and dashboard

- `kubectl port-forward -n platform-system svc/dora-exporter 8080:8080` then `curl
  localhost:8080/metrics` for the raw Prometheus exposition.
- Grafana: "CI/CD Platform - DORA Metrics" dashboard (`dora.json`), same
  `$app_namespace`/`$app` template-variable pattern as `pipelines-overview.json`.
- A standalone `ServiceMonitor` (`charts/glidepath-control-plane/templates/dora-exporter/servicemonitor.yaml`)
  registers the scrape target - no Helm-chart wiring needed, since every real
  Prometheus CR this platform has run against has empty
  `serviceMonitorSelector`/`serviceMonitorNamespaceSelector` (matches everything
  cluster-wide), the same precedent already established for Tekton's own controller
  metrics (`gitops-cluster-dev/50-platform-cicd/tekton-operator/tekton-servicemonitor.yaml`).
  **A real bug caught live here**: `ServiceMonitor.spec.selector`
  matches against the target `Service`'s own `metadata.labels`, not its
  `spec.selector` (which only selects the *pods* it fronts) - the first version of
  `deployment.yaml` set `spec.selector: {app: dora-exporter}` but never labeled the
  `Service` object itself, so every real scrape target was silently dropped despite
  matching on every other criterion (endpoint port name, namespace, readiness). Confirmed
  via Prometheus's own `/api/v1/status/config`: the generated scrape job's first
  `relabel_configs` rule keeps only targets where
  `__meta_kubernetes_service_label_app=dora-exporter`, sourced from the Service's labels.
  Fixed by adding `metadata.labels: {app: dora-exporter}` to the `Service` - confirmed
  live afterward via `up{job="dora-exporter"} == 1` and real `dora_*` values flowing
  through Grafana's own datasource proxy, not just raw Prometheus.

## Verification

- Unit-level: manually stamp the pending annotations on the real
  `nodejs-demo-app-staging` Application with a synthetic `flow-start-time`, force an
  ArgoCD hard-refresh/sync, confirm the exporter's logs show it reacting exactly once,
  confirm the annotations get cleared afterward, and confirm `/metrics` shows the
  expected counter/histogram observations.
- Failure-path test: same mechanism, against a deliberately broken sync (e.g. an invalid
  image tag), confirming `Failed`/`Error` correctly increments
  `dora_releases_total{outcome="failed"}` and sets `dora-last-failure-time`, and that a
  subsequent real success correctly computes and clears MTTR.
- End-to-end: a real push through build->test->deploy->release, PR merged for real,
  confirming the whole chain from `mark-release-pending`'s annotation stamp through to a
  populated `/metrics` endpoint and populated dashboard panels, with no manual
  intervention.
- RBAC check: `kubectl auth can-i --list
  --as=system:serviceaccount:platform-system:dora-exporter -n argocd` shows exactly
  `get`/`list`/`watch`/`patch` on `applications.argoproj.io`, and confirm
  `pipeline-runner` still cannot touch any Application other than its own
  `<app-name>-staging`.
- The above all cover the same-cluster path (Phase F's original scope). For the
  cluster-mapped path (`update-dora-metrics.yaml`'s Task, fed by
  `argocd-outcome-relay` via the broker/Trigger), see `multi-cluster.md`'s
  "Live-verified end to end, 2026-08-17" section - a real `checkout-api`/`kind-prod`
  release confirmed both `dora_deployments_total` and
  `dora_releases_total{outcome="succeeded"|"failed"}` incrementing via that path too
  (predates the 2026-08-22 MTTR fix above; not yet re-verified live against the new
  `update-dora-metrics` Task/ConfigMap path - see multi-cluster.md's own deploy-steps
  note).
