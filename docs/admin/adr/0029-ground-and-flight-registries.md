# ADR-0029: Ground and Flight registries; Flight images are promoted by digest before the release merges

*Status: Accepted (2026-10-27). Supersedes decision 3 of [ADR-0025](0025-retire-image-promotion-stub-check.md).
Phase 0 (spike) done; phase 1 (configuration and digest pinning) live on hangar-dev 2026-10-27; phase 2 (the
promoter: Pipeline, StepAction, broker `/promoter-token`) built, started by hand. Detail:
[registries.md](../registries.md).*

## Context

Every image lives in one registry (`ghcr.io/<owner>`): builds push there and every cluster pulls from there, so
"released" and "built" are the same place and an upper cluster can run anything a build ever pushed. ADR-0025
retired the `image-promotion` stub because, with one registry, it had nothing to do. A second registry that only a
promoter can write gives the check a real job: an upper cluster can only run an image that went through the gates
and was copied.

## Decisions

1. **Two registries.** Ground: written by builds, read by lower clusters, Testkube and the gates. Flight: written
   only by the promoter, read by upper clusters with a read-only credential. In the lab, Flight is
   `ghcr.io/riley-roooo`, a separate GitHub account. Configuration is admin-owned (catalog `registries`,
   `promotion`), never `cicd.yaml`.
2. **Release files pin a digest and record the source.** `release.image.digest` (what runs; airframe renders
   `repository:tag@digest`) and `release.image.source` (the Ground repository, where the evidence was verified). A
   Flight environment's release file pins the Flight repository from the moment the release PR is opened.
3. **Promotion happens before the merge**, when every required gate is green on the release PR's head commit (an
   event fan-in in the relay's reducer, not a polling pod). Merging before promotion fails closed.
4. **The copy keeps every digest.** Index, platform manifests, the legacy `.sig`/`.att` tags and the OCI 1.1
   referrers are copied by digest; nothing is re-signed, so Rekor entries and their integrated times stay valid.
5. **The copy runs in GitHub Actions.** GHCR does not accept GitHub App installation tokens, even with
   `packages: write` and the package linked to the installed repository (phase 0: push and read both refused). The
   promote pipeline uses the `hangar-flight-promoter` App only to start the `promote` workflow in
   `riley-roooo/flight-promoter`; that run's `GITHUB_TOKEN` is the only credential that writes Flight. Ground is
   private and GitHub will not grant another account's repository access to a package, so the workflow reads Ground
   with a `read:packages` token stored as an Actions secret (persisted, read-only).
6. **Upper-cluster admission verifies the promotion attestation**, not the Chains provenance. The workflow signs
   `https://hangar.io/attestations/promotion/v1` keyless on public Sigstore with its own workflow identity; cosign 3
   embeds the Rekor inclusion proof, so Kyverno verifies it offline. Chains' attestations carry no inclusion proof
   and Fulcio and Rekor run only on hangar-dev, so an upper cluster cannot verify those. The gates verify the
   provenance before the promotion that this attestation records. The "Flight images only" rule is a separate plain
   `ValidatingPolicy`: an `ImageValidatingPolicy` fetches every image it matches with its own credential.

## Consequences

- The promotion attestation is in the public Rekor log: the Flight repository, digest, source and run URL are
  public; the image is not.
- Two attestation storage schemes coexist per digest; the copy covers both.
- airframe-application before v0.3.166 rejects `digest`/`source`, so `promotion.pinDigest` stays off until every
  cluster's chart pin is at v0.3.166 or later. Tower (tower#143) and Backstage (backstage#198) must parse
  `repo:tag@digest` before then too.

## Phase 0 evidence (2026-10-27)

boarding-api `2.0.5-832e475` copied to Flight by the workflow: every digest equal, both platforms, the same five
attestations discoverable through the Flight reference; the promotion attestation verified offline against the
workflow identity, a wrong identity rejected; Kyverno 1.19 on hangar-prod (Audit+Warn, scratch namespace) admitted
the promoted image and warned on the Ground copy and on an unpromoted Flight image.
