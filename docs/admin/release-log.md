# Release log

A queryable, human-readable history of releases - one record per confirmed outcome,
carrying what a scattered set of existing signals (a Tempo trace, a DORA counter, one
gitops-repo PR) each only show a slice of: which app/env, what image/commit, who
approved, what each governance gate concluded, whether the merge bypassed a failing
check, and how it resolved.

## Why this doesn't add a new datastore

Release data already exists - Tempo traces (`docs/admin/tracing.md`), DORA counters
(`docs/admin/dora-metrics.md`), and each gitops-repo release PR's own checks/reviews -
but none of it is a single, durable, cross-app "show me every release" view. Rather than
stand up a new service or database for that, this reuses infrastructure that was already
running but idle: the OTel Collector deployed by
`gitops-cluster-dev/40-observability/otel-collector` already has a `logs:` pipeline
(`receivers: [otlp]` -> `otlphttp/loki` exporter into Loki's own OTLP endpoint) - it just
had no producer sending it anything. `release-log-emit.yaml` is that producer: one
stateless OTLP log record per confirmed release, sent via `otel_log_send`
(`catalog/lib/otel.sh`) to the exact same collector endpoint
(`OTEL_EXPORTER_OTLP_ENDPOINT`) every span already targets - same "mint the payload,
send one stateless call, never a daemon" philosophy `docs/admin/tracing.md` already
established for spans. otel-cli itself has no logs subcommand (traces only), so this
speaks the collector's OTLP/HTTP JSON receiver directly with a plain `curl` instead of
going through it.

**A DaemonSet log-shipper (Promtail/Alloy scraping pod stdout) was considered and
rejected** - found live, 2026-08-23, that despite Loki running, nothing was shipping pod
logs into it at all (`kubectl get daemonset -A` showed none; querying Loki directly
confirmed the only real data was its own `loki-canary` self-check). A DaemonSet would
have been a second, redundant logs pipeline sitting next to one already built and wired
to Loki, and would have broken from this platform's established push-not-scrape
instrumentation pattern. The OTLP-push fix above closes the same gap with zero new
infrastructure.

## What's in a record

Wired into `release-outcome-notify.yaml` as a fourth independent task
(`release-log-emit`, alongside `notify`/`span`/`update-dora-metrics` - no `runAfter`,
same as its siblings). On a confirmed outcome, it:

1. Parses owner/repo/PR number out of `pr-url`.
2. Gets a GitHub App installation token scoped to that one gitops repo (the same
   `/github-installation-token` broker call `detect-bypass-merge.yaml` and
   `open-release-pr.yaml` already use - this Task runs as the Application's own
   `pipeline-runner` SA, in the Application's own namespace, so the broker's "caller's
   namespace must own a Repository CR for this repo" check applies exactly as it does
   everywhere else. See `docs/admin/release.md`'s "How the GitHub App's private key
   stays out of Application namespaces" section).
3. Fetches the PR itself (merged-by, title), its reviews (approvers), and its head
   commit's check-runs - one gate lookup per entry in `.Values.releaseGuardrails`, same
   "checked by name" approach `detect-bypass-merge.yaml` already uses, off the same
   single source of truth (add/remove a gate there, this Task needs no change).
4. Computes `bypass`: true iff the PR merged while any gate's check wasn't `success` -
   identical definition to `detect-bypass-merge.yaml`'s own break-glass detection.
5. Emits one OTLP log record: a JSON object (app/env/cluster/status, git url/revision,
   chain-id, every timestamp, PR url/number/title, merged-by, approvers, one flat
   `gate_<name>` key per entry in `.Values.releaseGuardrails` - e.g. `gate_sast`,
   `gate_image_scan` - and bypass) as the log body, plus a small set of low-cardinality
   fields (`app_namespace`, `app_name`, `env`, `cluster`, `status`, `bypass`) as OTLP log
   attributes. Gate results are flat top-level keys, not a nested `gates` object -
   Grafana's "Extract fields" table transform (see "Rendering it in Grafana" below) turns
   each top-level JSON key into its own column directly; a nested object would just
   render as one column holding a stringified sub-object.

## Real gap hit live: `otel_log_send` didn't exist in the running toolbox image

`catalog/lib/otel.sh` is baked into `ghcr.io/jfillman/platform-cicd-toolbox` at build
time (`catalog/toolbox/Dockerfile`'s `COPY catalog/lib/otel.sh ...`), not mounted live
from git - adding `otel_log_send` to the source file alone doesn't reach a running
cluster. First real `release-log-emit` TaskRun failed with `otel_log_send: command not
found`: the image is tagged `:latest` with `imagePullPolicy: IfNotPresent`, so the
dev-cluster node's already-cached `:latest` kept being reused even after a fresh image
was pushed to the registry - the same class of staleness already flagged as a risk in
`docs/admin/multi-cluster.md`'s "Still not done" section for this exact image. Fixed by
rebuilding (`docker build -f catalog/toolbox/Dockerfile -t
ghcr.io/jfillman/platform-cicd-toolbox:latest .`, arm64 to match the kind-dev node),
pushing, then explicitly evicting the stale image from the node
(`crictl rmi ghcr.io/jfillman/platform-cicd-toolbox:latest` inside the node container -
`IfNotPresent` won't re-pull on its own). A second real TaskRun, fed
`gitops-checkout-api`'s actual PR #14, then succeeded and correctly reported real data
(`bypass=true`, `approvers=none`, `gate_provenance=missing` - that last one is real too:
PR #14 predates the `policy-check`->`provenance` gate rename, so its check-runs were
posted under the old name and today's lookup for `provenance` correctly finds nothing,
rather than silently matching the wrong check).

## Querying it

Loki's OTLP ingestion routes log-record *attributes* to **structured metadata**, not a
real indexed stream label - confirmed live, 2026-08-23: `/loki/api/v1/labels` never
listed any `platform_release_log_*` name no matter how many were sent, only
`k8s_namespace_name`/`k8s_pod_name`/`pod`/`service_name`/`stream` are real index labels
on this cluster (an earlier pass at this doc claimed the opposite, based on misreading
`query_range`'s response shape - its `"stream"` object merges real labels *and*
structured metadata together for display, which isn't proof either one is indexed; the
`/labels` endpoint is the authoritative source). This means there's no cardinality cost
either way, but the split still matters for how you query:

- **Structured metadata** (`app_namespace`/`app_name`/`env`/`cluster`/`status`/`bypass`)
  is filterable directly with LogQL's `| key="value"` (or `=~` for a substring/regex
  match), no parsing step needed - `{service_name="platform-cicd"} |
  platform_release_log_status="Succeeded"`.
- **Everything else** (PR detail, approvers, per-gate results, every timestamp) lives in
  the log body as one JSON object, reached via `| json` -
  `{service_name="platform-cicd"} | json | pr_title != ""`.

Live-verified combined query (2026-08-23, real cluster, real data - not a hypothetical):

```logql
{service_name="platform-cicd"}
  | platform_release_log_app_name=~".*checkout-api.*"
  | platform_release_log_env=~".*prod.*"
  | json
```

correctly returned the matching record with `pr_title`/`gate_sast`/etc. all present as
extracted fields - that's true of Loki's own raw HTTP API (`| json` auto-expands the
body inline), but **not** of what a Grafana panel actually receives.

## Rendering it in Grafana: real bugs found in the first cut of the dashboard

The first version of `release-log.json` shipped broken - wrong columns, and a
`labelTypes` column dumping raw JSON - caught only once actually rendered (headless
Chromium via Playwright, not just deploying and assuming), not by any of the LogQL
testing above. Two real, distinct bugs:

1. **A Grafana panel's datasource query and Loki's raw HTTP API return different
   shapes.** `/api/ds/query` against the exact same LogQL always returns a fixed
   six-field frame - `labels`, `Time`, `Line`, `tsNs`, `labelTypes`, `id` - regardless of
   `| json` in the query; the JSON body's keys are **not** auto-expanded into columns the
   way they are in Loki's own `/loki/api/v1/query_range` response (confirmed by querying
   `/api/ds/query` directly and inspecting the returned frame schema). `labelTypes` -
   Grafana's own internal per-field type classification (indexed/structured-metadata/
   parsed), never meant to be displayed - was rendering as a raw column because nothing
   excluded it. Fixed by adding a real `extractFields` transformation (`source: "Line",
   format: "json"`) as the first step, ahead of `organize`, so the body's keys actually
   become dataframe fields before anything tries to reference them by name.
2. **Renamed columns silently didn't rename.** A separate `{"id": "renameByName", ...}`
   transformation is not how this platform's other dashboards do it (checked against
   real precedent - `cicd-fleet-overview.json`, `cicd-performance.json`,
   `cicd-governance.json` - before fixing, not guessed): `renameByName` is a key *inside*
   `organize`'s own `options`, alongside `excludeByName`/`indexByName`, not a standalone
   transform. The standalone version didn't error, it just silently did nothing.

Also found via the same render: Grafana's Loki datasource is configured with a global
`derivedFields` entry (`charts/glidepath-control-plane` observability stack) that
injects its own `TraceID` field into every logs query - unrelated to this feature,
excluded explicitly rather than left to leak into the table.

**Verification method**: imported the dashboard JSON under a throwaway UID via
`/api/dashboards/db`, rendered it with a headless browser, and read the real DOM (column
headers, cell text) rather than trusting the query response shape or the transformation
option names from memory - the same live-verification standard as the rest of this
platform, just applied to a Grafana panel instead of a cluster resource.

## Known gap: cluster-mapped releases only

`release-log-emit` only runs for releases with a cluster-mapped upper env
(`release-outcome-notify`'s own trigger,
`charts/glidepath-app/templates/triggers/release-outcome-trigger.yaml`, is only
rendered when `glidepath-app.hasClusterMappedUpperEnv` is true - see
`docs/admin/multi-cluster.md`). A same-cluster release (today's `nodejs-demo-app`/
`cicd-flow-test-app` staging deploys) never reaches this Task - `dora-exporter`'s own
direct `Application` watch handles DORA metrics for that path instead, with no
Tekton Pipeline involved at all.

Deliberately not closed here rather than papered over. The natural place to add it would
be `dora-exporter` itself (`platform/dora-exporter`) - it already receives confirmed
terminal outcomes for *both* paths (same-cluster via its own watch, cluster-mapped via
`update-dora-metrics.yaml`'s call into `/argocd-outcome`), so it's the one place both
paths already converge. But `dora-exporter` is a cluster-wide singleton with no
Repository-CR-scoped identity for any one tenant's gitops repo - giving it the ability to
mint a GitHub installation token for *any* app's gitops repo (needed for the same
approvers/gate enrichment this doc's Task does) would widen a blast radius this platform
has deliberately kept narrow everywhere else (TokenReview-scoped brokering, per-app
impersonation - see `docs/admin/release.md`'s own token-broker section). Closing this
gap needs a real design decision (e.g. `mark-release-pending.yaml` also stamping a
`pr-url` annotation, and `dora-exporter` posting a `dev.cdevents.environment.deployed`
CDEvent back through the broker the same way `argocd-outcome-relay` already does "on
behalf of" any app - reusing that already-reviewed trust boundary rather than opening a
new one), not a quick patch alongside this feature.
