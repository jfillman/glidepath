# ADR-0009: External Secrets Operator + self-hosted Infisical as the secrets backend

## Context

Every chart in this platform needs real secret material (registry credentials, GitHub
App keys, per-app webhook tokens) without committing it to git or hand-applying raw
`Secret` objects per cluster. ESO was installed early as bootstrap-time infrastructure
but had no configured backend for a long time - the pragmatic bridge that filled the
gap (ESO's own `kubernetes` provider, mirroring real `Secret`s out of one hand-managed
namespace) was always documented as a stopgap, not the destination.

## Decision

External Secrets Operator is platform infrastructure, not a bootstrap-only install -
every chart consumes secret material via a real `ExternalSecret`, never a hand-applied
raw `Secret`. The backend is self-hosted Infisical (`idp-service-catalog`'s instance,
kind-dev only), not ESO's `kubernetes` provider, HashiCorp Vault, or a cloud secrets
manager.

Two separate, deliberately non-identical paths:

- **Platform-wide material** (registry credentials, GitHub App creds, per-cluster relay
  tokens) lives in the control plane's own Infisical project (one per cluster it runs
  on), `authMethod: kubernetes` - zero persisted credential, ESO's controller SA token
  verified live against the cluster's own TokenReview API.
- **Application-owned secrets** (Slack webhooks, scan credentials) come from THAT
  application's own idp-managed `ClusterSecretStore`, referenced directly by name -
  never mirrored into a platform-cicd-owned project. An earlier version did mirror them
  per-app; live comparison showed the mirror was strictly *wider* than the original
  (missing idp's own `namespaceRegexes` scope), a real least-privilege regression, not
  just duplication - deleted in favor of referencing idp's object directly.

Real credential material is always planted by a human directly into Infisical (UI or
API) - never through this chart, never committed to this repo.

## Consequences

- An Application's CI secrets require that Application to have been onboarded through
  idp's `NodeJSApplication` XR first - a real, accepted coupling. An app that hasn't
  just gets a not-ready `ExternalSecret`, the same graceful-degrade shape every other
  "not configured yet" case in this platform tolerates.
- `registry-credentials` is disseminated cluster-wide via one `ClusterExternalSecret`
  keyed off a namespace label, not a per-app opt-in - every onboarded namespace gets it
  automatically, with no flag to forget.
- Deliberate exceptions exist where "sync from Infisical" doesn't fit: Fulcio's root
  signing key (fresh, per-cluster-generated, never persisted anywhere Infisical-adjacent
  - a leak there is catastrophic) and Rekor/Trillian's MySQL credentials (genuinely
  internal DB auth, no ability to forge a signature or cluster identity if leaked - a
  committed value is an acceptable trade-off there, unlike Fulcio's key).
- ESO's `refreshInterval` re-syncs a changed Infisical value on a timer - it does not
  rotate the underlying credential itself. Real rotation is still a human action.
