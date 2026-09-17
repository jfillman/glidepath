# ADR-0016: Tekton Results for long-term pipeline data retention

## Context

Nothing in this platform durably archived pipeline run history. Grafana/DORA-exporter/
CDEvents track *metrics and events* derived from runs (duration, pass/fail, deployment
frequency), not the actual PipelineRun/TaskRun objects - full spec, status, and step
logs. Those lived in etcd until [pipelinerun-pruner](../pipelinerun-pruner.md) deleted
everything past the newest-per-pipeline after 24h. Once pruned, a run's history was
gone - no way to look back at what a build actually did beyond that window.

`tektoncd/operator`'s `TektonConfig` bundles Tekton Results (an archival/results-API
component) under `profile: all`, but it had always been explicitly disabled
(`result.disabled: true`) since this need didn't previously exist.

## Decision

Enable Tekton Results (`result.disabled: false`) with:

- **Internal (operator-managed) Postgres**, not an external database. No
  general-purpose Postgres provisioning path exists in this stack - Crossplane here
  only manages GitHub repos - and the operator handles the internal DB/PVC
  automatically. Considered and rejected: standing up a dedicated Postgres just for
  this is more moving parts than the problem justifies today.
- **S3 storage reused from the existing observability MinIO instance** (already
  backing Loki/Tempo/Thanos), via a new `tekton-results` bucket, rather than a
  dedicated MinIO. Matches how every other observability component already shares
  that one instance.
- **`watcher.completed_run_grace_period: "1h"`**, making Results the primary
  CR-cleanup path (archive, then delete the CR) well ahead of
  `pipelinerun-pruner-cronjob`'s 24h retention - the pruner becomes a no-op safety net
  for anything Results doesn't catch, rather than racing it for the same objects.
- **No external exposure (no `HTTPRoute`) for now.** Live-checked: the Results API
  server is TLS-only on its single port (gRPC and REST are content-negotiated on the
  same socket; upstream install docs require generating a TLS cert as a hard
  prerequisite). The cluster's Gateway has only a plain HTTP/80 listener - no TLS
  listener exists - so a plain HTTPRoute (like `tekton-dashboard-ui`'s, which backends
  to a plain-HTTP port) cannot front a TLS-only backend; Traefik would hand the pod
  cleartext on a TLS socket and every request would fail the handshake. Access is via
  `tkn results` CLI or `kubectl port-forward` until the Gateway has a real
  TLS/passthrough listener.

## Consequences

- A real second storage/compute footprint lands on the cluster (Postgres + API +
  watcher pods), on a platform that has repeatedly hit node-capacity ceilings running
  this component set on one node - worth watching if capacity pressure recurs.
- Exposing the Results API externally (browsing archived runs/logs outside the
  cluster, not just via `tkn`) is blocked on a real, separate piece of infra work: a
  Gateway TLS or TLS-passthrough listener. Not scoped here.
- `pipelinerun-pruner-cronjob` is left in place, not removed - it now acts as a
  backstop for any run Results' watcher doesn't reach (e.g. Results itself unhealthy),
  rather than the only cleanup mechanism.
