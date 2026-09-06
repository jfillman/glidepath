# ADR-0014: Keyless signing uses two separate trust roots - public Sigstore for humans, self-hosted Fulcio for workloads

## Context

Both git commits and build images need cryptographic signing/attestation without this
platform managing long-lived private key material for anyone. The identity being
proven is different in each case, though: a human developer authenticating via GitHub/
Google OIDC, versus an in-cluster Tekton workload authenticating as a Kubernetes
ServiceAccount. One sigstore trust root doesn't naturally fit both.

## Decision

**Commit signing** (`gitsign`, see `docs/commit-signing.md`) uses the **public**
Sigstore instance (`fulcio.sigstore.dev`/public Rekor/CTLog) - a human developer's
GitHub/Google OIDC identity is exactly what the public instance already trusts, so
running private infrastructure for it would add operational cost for no real gain.

**Image and provenance signing** (Tekton Chains, see `docs/image-signing.md`) uses a
**self-hosted** Fulcio instance instead, in-cluster (`fulcio-system`), for the opposite
reason: the public Fulcio only trusts a fixed, curated set of known CI issuers and has
no way to trust an arbitrary Kubernetes cluster's own private OIDC issuer. Fulcio's
built-in `type: kubernetes` OIDC issuer mode is used directly against this cluster's
own ServiceAccount tokens - explicitly **not** SPIFFE/SPIRE, which would be separate,
unneeded infrastructure for a problem Fulcio's own Kubernetes issuer mode already
solves natively, confirmed against Fulcio's own upstream CI test rather than assumed.

Self-hosted Rekor + Trillian was added later (2026-09-05, after four earlier attempts
were destabilized by unrelated podman-emulation artifacts on the older stack - see
`docs/provenance-policy.md`) so that image signatures get a real transparency-log
timestamp to verify against, the same role the public Rekor plays for commit signing.

## Consequences

- Two structurally different sigstore trust roots are live on this platform at once -
  verification tooling must point at the correct root/issuer for the artifact type it's
  checking; the two are never interchangeable, and mixing them up fails closed (a good
  failure mode, but a real footgun to document clearly for anyone extending
  verification).
- Self-hosted Fulcio's root CA is a one-time, manually-bootstrapped secret (later moved
  from a by-hand script to an ArgoCD pre-install hook Job - ADR-0006's update),
  deliberately never auto-rotated - rotating it invalidates every image ever signed
  under the old root, so it's a rare, explicit human decision, not routine maintenance.
- Tekton Chains ships its own cluster-wide `ClusterRole` (read on `secrets`/
  `configmaps`/`serviceaccounts` across every namespace) to resolve any Application's
  registry credentials - a known, accepted deviation from this platform's otherwise
  strict least-privilege posture, not narrowed here since doing so risks breaking
  Chains' own core reconciliation loop.
- CTLog, TSA, and TUF remain deliberately deferred - self-hosted Rekor's own tlog
  timestamp already closes the specific cert-expiry gap those would have addressed, and
  no other current gap justifies the added infrastructure.
