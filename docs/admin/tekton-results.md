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

## Retention and its interaction with the pruner

`watcher.completed_run_grace_period: "1h"` - the Results watcher archives a completed
run and deletes its PipelineRun/TaskRun CRs from the cluster about an hour after
completion. This makes Results the primary cleanup path (freeing etcd/pod-count
pressure - a real, recurring capacity issue on this cluster, see
[installation.md](installation.md)), well ahead of `pipelinerun-pruner-cronjob`'s 24h
`RETENTION_HOURS`. In practice the pruner should rarely find anything left to do -
it's kept in place as a safety net for whatever Results doesn't catch (e.g. Results
itself unhealthy for a stretch), not removed.

## Querying archived runs

No `HTTPRoute` exists for the Results API - see below. Use the `tkn` CLI or a
port-forward:

```
kubectl port-forward -n tekton-pipelines svc/tekton-results-api-service 8080:8080
tkn results list --insecure --addr localhost:8080 default
```

(`--insecure` skips TLS verification against the operator's self-signed cert for a
local port-forward - not a statement that the connection itself is unencrypted.)

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
