# Tekton Results

Archives completed PipelineRuns/TaskRuns (full spec, status, and step logs) to a
durable store, so run history survives past whatever
[pipelinerun-pruner](pipelinerun-pruner.md) or etcd itself would otherwise keep.
Grafana/[DORA-exporter](dora-metrics.md)/CDEvents track metrics and events derived from
a run - duration, pass/fail, deployment frequency - not the run object itself; before
this, once a run was pruned there was no way to look back at what it actually did. See
[ADR-0016](adr/0016-tekton-results-archival.md) for the full decision record.

Installed via `tektoncd/operator`'s `TektonConfig` CR (`result.disabled: false`) - see
`gitops-cluster-dev/50-platform-cicd/tekton-operator/tektonconfig.yaml` (and the
equivalent path in `apron` for new clusters) for the live manifest and its own inline
rationale comments.

## Architecture

- **Database**: the operator's own internal, auto-managed Postgres (`is_external_db:
  false`) - not an external database. No general-purpose Postgres provisioning path
  exists elsewhere in this stack (Crossplane here only manages GitHub repos), so
  standing up a dedicated one just for this wasn't justified.
- **Object storage**: the existing observability MinIO instance (the same one behind
  Loki/Tempo/Thanos), via a new `tekton-results` bucket
  (`40-observability/minio/create-buckets-job.yaml`) and a `tekton-results-s3-secret.yaml`
  Secret in `tekton-pipelines` (static creds, same pattern as the other observability
  buckets - not an ExternalSecret).
- **Only completed runs are archived** (`watcher.disable_storing_incomplete_runs: true`) - no
  in-progress-run noise.
- **Step logs are queryable and land in the MinIO bucket** - this needs three separate
  flags, confirmed the hard way during rollout: `result.logs_api: true` enables the API
  server's log-retrieval endpoints, `result.logs_type: S3` picks the storage backend, and
  `result.watcher.logs_api: true` - a **different** field on the watcher's own config,
  not inherited from `result.logs_api` despite the identical name - is what actually
  makes the watcher push step logs to the API at all. Without the third one, DB records
  for PipelineRun/TaskRun objects archive fine and the first two flags look correctly
  set, but zero log objects ever reach the bucket - the watcher's own startup log prints
  `logs disable in watcher` verbatim when it's missing.
- **A fourth flag was needed even with all three above correct**: every log upload still
  failed live with `operation error S3: UploadPart, compute input header checksum
  failed, unseekable stream is not supported without TLS and trailing checksum`. The API
  server's S3 client (`aws-sdk-go-v2`) defaults to a trailing-checksum multipart upload
  that requires TLS, and this MinIO instance is plain HTTP (same as Loki/Tempo/Thanos's
  already-working connections to it) - a real upstream SDK behavior change
  (config v1.27+/SDK v1.30+ default to `when_supported`), not a MinIO-specific bug.
  Fixed via the operator's documented deployment-override mechanism
  (`result.options.deployments.tekton-results-api`, `tektoncd/operator`'s own
  `TektonConfig.md`), setting `AWS_REQUEST_CHECKSUM_CALCULATION` /
  `AWS_RESPONSE_CHECKSUM_VALIDATION=when_required` on the API deployment's `api`
  container.

- **Step logs are archived with per-line timestamps** (`result.watcher.logs_timestamps: true`,
  the watcher's `-logs_timestamps` flag, 2026-10-12): the watcher reads each step's pod log
  with `timestamps=true`, so every archived line starts with the container runtime's RFC3339
  instant, exactly as a live `kubectl logs --timestamps` does. Tower parses that prefix out of
  archived logs so a finished run's log view has the same time column as a running one's
  (which glidepath-api reads live from the TaskRun's pod with the same option, `getLiveLog`;
  [api.md](api.md#live-and-archived-reads)). Runs archived before
  the flag took effect have no prefix and still render, just without times.

## Task log conventions

Everything a Task prints is read later, in Tower, next to the step it came from. Two rules
keep that readable (both found the hard way, 2026-10-12):

- **A result is written, then logged on its own line as `name=value`** - never
  `echo -n "$x" | tee "$(results.name.path)"`. Results must have no trailing newline (a
  `when:` gate compares the exact string; `$(steps.*.results.*)` env substitutions keep a
  newline literally), so `tee` printed them with no newline either and consecutive results ran
  together in the log (`./Containerfileghcr.io/…:0.1.7-5616e3d0.1.7truetrue2026-…`). Use
  `printf '%s' "$x" > "$(results.name.path)"` for the file and `echo "name=$x"` for the log;
  a result nobody needs to see in the log is written and not echoed.
- **JSON in a log line is one line**: `yq -o=json -I=0 …` (or `jq -c`), never bare
  `yq -o=json`, which pretty-prints and splits the message over several lines.

Timestamps are not something a Task prints itself: the runtime stamps every line (above), so
third-party steps (BuildKit, trivy, cosign) are covered the same way as the toolbox scripts.

## Retention and its interaction with the pruner

`watcher.completed_run_grace_period: "1h"` - the Results watcher archives a completed
run and deletes its PipelineRun/TaskRun CRs from the cluster about an hour after
completion. This makes Results the primary cleanup path (freeing etcd/pod-count
pressure - a real, recurring capacity issue on this cluster, see
[installation.md](installation.md)), well ahead of `pipelinerun-pruner-cronjob`'s 24h
`RETENTION_HOURS`. In practice the pruner should rarely find anything left to do -
it's kept in place as a safety net for whatever Results doesn't catch (e.g. Results
itself unhealthy for a stretch), not removed.

## When a run's logs are missing

A run can be archived with some of its TaskRuns' logs missing: the Log record exists but its
`status` reads `isStored: false`, `size: 0`, with no `errorOnStoreMsg`. glidepath-api then
answers "Tekton Results archived no logs for this task" (404, `GP-LOG-404`), and Tower shows that.
The blob was never written, so nothing is in the bucket to recover, and no collector ships pod logs to Loki on the dev cluster.

Two causes, both in the watcher, both seen on the dev cluster on 2026-10-25 (56 lost logs in 6 h,
about one TaskRun in ten during release-gate bursts):

- **The watcher's own timeout (53 of 56).** The watcher streams a finished TaskRun's logs on a
  background goroutine and cancels it after `watcher.dynamic_reconcile_timeout`, 30 s by default,
  logging `Leaving sendLogs thread only after timeout`. It creates the Log record first and never
  retries one that exists, so a slow stream is lost for good. Under a burst of gate pipelines plus
  etcd stalls, 30 s wasn't enough. The cluster repos now set `dynamic_reconcile_timeout: "5m0s"`, the
  same bound `update_log_timeout` already gives the API's upload.
- **An etcd timeout patching the log annotation** (`Error sending log ... etcdserver: request timed
  out`). Same no-retry path; it goes with the cluster's etcd stalls, not with Results.

To check a run, decode its Log records and read their `status`:

```shell
kubectl get --raw "/api/v1/namespaces/platform-system/services/tekton-results-relay:8080/proxy/apis/results.tekton.dev/v1alpha2/parents/<ns>/results/<result>/records?page_size=100" \
  | jq -r '.records[] | select(.data.type | endswith("Log")) | .data.value | @base64d | fromjson | "\(.spec.resource.name) \(.status)"'
```

## Querying archived runs

No `HTTPRoute` exists for the Results API (see below), and `tkn`'s built-in `results`
subcommand doesn't exist in this platform's installed CLI version (0.45.1) - use the
separate **`tkn-results`** plugin instead (not bundled with `tkn`; there are no prebuilt
release binaries either, install via Go):

```
go install github.com/tektoncd/results/tools/tkn-results@latest
```

### Access control

The API authorizes every request against Kubernetes RBAC (`AUTH_MODE=token`) for the
`results.tekton.dev` API group (`results`/`records`/`logs`/`summary`), scoped to the
run's own namespace - it's aggregated into the built-in `view`/`admin` ClusterRoles
(confirmed live: `kubectl get clusterrole view -o yaml` shows the aggregated rule), but
**no namespace in this cluster currently has anything bound to `view` or `admin`** - every
app namespace's existing RoleBindings are narrow, purpose-built ones (`pipeline-runner`,
token-refreshers, etc.), none of which include this permission. So today, querying
Results requires either a cluster-admin identity or a RoleBinding you create yourself.
For regular use, bind a dedicated read-only identity per namespace you want to query
(`view` is the least-privileged ClusterRole that has the aggregated rule):

```
kubectl create serviceaccount results-reader -n <namespace>
kubectl create rolebinding results-reader --clusterrole=view \
  --serviceaccount=<namespace>:results-reader -n <namespace>
```

Not created as a standing identity by this rollout - deliberately left as a decision for
whoever actually needs regular query access, rather than adding a new persistent
credential to the cluster as a side effect of enabling the feature.

### Commands

`tkn-results` auto-port-forwards to `tekton-results-api-service` and auto-mints a token
for `--sa`/`--sa-ns` using your own kubeconfig's authority (no manual token handling,
no `kubectl port-forward` needed) - `--insecure` skips verifying the operator's
self-signed cert, same non-statement about wire encryption as any local port-forward:

```
# list PipelineRun results in a namespace
tkn-results list --sa=results-reader --sa-ns=<namespace> --insecure <namespace>

# list records (PipelineRun/TaskRun objects + Log pointers) for one result
tkn-results records list --sa=results-reader --sa-ns=<namespace> --insecure \
  <namespace>/results/<result-id>

# fetch one step's actual log content - the "logs" path swaps in for "records"
# using that record's own name, not its later-listed id/uid field
tkn-results logs get --sa=results-reader --sa-ns=<namespace> --insecure -o textproto \
  <namespace>/results/<result-id>/logs/<log-record-name>
```

Live-verified end to end against real `app-backstage-cicd`/`app-checkout-api-cicd`
pipelines: `list` and `records list` both return real data immediately; `logs get`
against a record created **before** the checksum fix above returns `rpc error: code =
Internal desc = Error streaming log` (the DB record exists, but its S3 object was never
actually written - that upload failed at the time) - this is expected for anything
archived before the fix, not a sign it's still broken. Anything archived after the fix
streams real log content correctly.

## Why there's no external route yet

The Results API server is TLS-only on its single port - gRPC and REST are
content-negotiated on the same socket, and upstream install docs list generating a
TLS cert as a hard prerequisite, not an option. This cluster's Gateway has only a
plain HTTP/80 listener today (confirmed live - no TLS listener exists). A plain
`HTTPRoute`, like the one already in place for the read-only Tekton Dashboard
(`tekton-dashboard-ui`, which backends to a plain-HTTP port), cannot front a TLS-only
backend: Traefik would forward cleartext to a TLS socket and every request would fail
the handshake. Exposing this externally needs a real Gateway TLS or TLS-passthrough
listener - separate infrastructure work, not scoped here.
