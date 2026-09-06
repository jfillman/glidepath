# ADR-0015: Provenance policy validates the attestation as input, additive to commit signing

## Context

Signing (ADR-0014) proves *who* produced an image and its SLSA provenance attestation.
It proves nothing about *what the build pipeline actually did* - whether the required
tasks (SAST, image scan, SBOM) genuinely ran as part of producing this specific image,
rather than the attestation simply existing. A release gate needs to check that, not
just that a valid signature is present.

## Decision

Conforma (`ec`) validates using **`ec validate input`, not `ec validate image`** - the
policy runs against the SLSA provenance attestation's own structured content (which
tasks actually ran, per the attestation's material list), not a bare `cosign verify`
against the image alone. This is **additive to gitsign's commit-signature check, not a
replacement for it** - the two verify different things (who committed vs. what the
build pipeline did) and both are required, not alternatives to pick between.

## Consequences

- Policy correctness is bounded by what Tekton Chains actually attests - a required
  task that never emits a checkable result in the provenance material list cannot be
  verified by this mechanism regardless of what the policy author intends it to check.
  Extending required-task coverage means first confirming Chains actually attests that
  task's execution, not just adding a policy rule and assuming it's enforced.
- An air-gapped, static local TUF root for policy/trust-material fetching was
  considered and not built - policy validation still depends on the live public TUF
  CDN being reachable, a real (if low-probability) availability dependency on every
  release.
- This validates the pipeline that *produced* the artifact, not the artifact's runtime
  behavior - deliberately scoped as a release-time supply-chain gate, not a substitute
  for runtime admission control.
