# Glidepath: admin guide

Documentation for people **running** this platform - installing it, operating it,
and understanding why it's built the way it is. If you're an application developer
using an already-running platform, see [../user/](../user/) instead.

## Start here

| Doc | Read this when... |
|---|---|
| [architecture.md](architecture.md) | You want the current-state system overview. |
| [security.md](security.md) | You want the security model end to end - threat model, RBAC, supply chain, known gaps. |
| [installation.md](installation.md) | You're installing this platform on a cluster. |
| [onboarding-mechanics.md](onboarding-mechanics.md) | You're registering a new app onto an already-running platform. |
| [adr/](adr/) | You want to know why a specific decision was made this way. |

## Reference

Grouped by concern - each is a standalone deep-dive, cross-linked from
architecture.md wherever relevant.

**Chaining & multi-cluster**
- [chaining.md](chaining.md) - the CDEvents broker and TokenReview auth mechanism
- [multi-cluster.md](multi-cluster.md) - staging/prod topology, ArgoCD-per-cluster
- [concepts.md](concepts.md) - terminology (Application, flow, stage)

**Governance & supply chain**
- [governance-stubs.md](governance-stubs.md) - the stub→real gate mechanism
- [image-signing.md](image-signing.md) - Fulcio/cosign keyless signing
- [provenance-policy.md](provenance-policy.md) - SLSA attestation policy
- [commit-signing.md](commit-signing.md) - gitsign commit verification
- [rootless-builds.md](rootless-builds.md) - kaniko under Pod Security `restricted`

**Release & operations**
- [release.md](release.md) - the GitOps promotion flow, step by step
- [release-guardrails.md](release-guardrails.md) - adding/removing a release-PR gate
- [catalog-versioning.md](catalog-versioning.md) - promoting shared catalog changes
- [pipelinerun-pruner.md](pipelinerun-pruner.md) - PipelineRun history cleanup
- [stalled-pipeline-detector.md](stalled-pipeline-detector.md) - stuck-flow alerting

**Observability**
- [tracing.md](tracing.md) - OpenTelemetry span/trace stitching across stages
- [dora-metrics.md](dora-metrics.md) - deployment frequency/lead time/CFR/MTTR
- [OTEL_AND_CDEVENTS_FLOW.md](OTEL_AND_CDEVENTS_FLOW.md),
  [OTEL_CDEVENTS_COMPLETE_EXAMPLE.md](OTEL_CDEVENTS_COMPLETE_EXAMPLE.md),
  [OTEL_CDEVENTS_DEBUGGING_GUIDE.md](OTEL_CDEVENTS_DEBUGGING_GUIDE.md) - deep debugging
  reference for the tracing/CDEvents plumbing

**Notifications & secrets**
- [notifications.md](notifications.md), [design-language.md](design-language.md) -
  Slack/GitHub notification content and shared visual language
- [secrets-management.md](secrets-management.md), [app-secrets.md](app-secrets.md) -
  the ClusterSecretStore/ExternalSecrets model
- [ephemeral-environments.md](ephemeral-environments.md) - per-branch/PR preview envs
- [naming-conventions.md](naming-conventions.md) - namespace/resource naming rules

See [../archive/](../archive/) for the original, unabridged design history these docs
and the [ADRs](adr/) were distilled from.
