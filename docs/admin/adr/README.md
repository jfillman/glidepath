# Architecture Decision Records

One file per load-bearing decision: what it is, why it was made this way, and what it
costs. Written after the fact from the real design/implementation history - see
[../../archive/](../../archive/) for the original, unabridged design record these were
distilled from.

| ADR | Decision |
|---|---|
| [0001](0001-tekton-pipelines-as-code.md) | Tekton + Pipelines-as-Code, vanilla Kubernetes |
| [0002](0002-cdevents-broker-tokenreview.md) | CDEvents broker with TokenReview auth for inter-stage chaining |
| [0003](0003-governance-stubs.md) | Governance gates as explicit, structurally-loud extension points |
| [0004](0004-gitops-only-release.md) | GitOps-only release promotion |
| [0005](0005-multicluster-per-cluster-argocd.md) | Per-cluster ArgoCD instances, event-driven outcome reporting |
| [0006](0006-cluster-agnostic-bootstrap.md) | Cluster-agnostic bootstrap, no cluster state in the app repo |
| [0007](0007-testkube-shared-namespace.md) | Testkube CE in one shared namespace, not one per tenant |
| [0008](0008-kyverno-testkube-secret-policy.md) | Kyverno ValidatingPolicy closes the Testkube shared-secret gap |
| [0009](0009-eso-infisical-secrets-backend.md) | External Secrets Operator + self-hosted Infisical as the secrets backend |
| [0010](0010-kaniko-rootless-builds.md) | Kaniko for rootless image builds under PSS `restricted` |
| [0011](0011-two-namespace-tenancy-model.md) | Every Application is (at least) two peer namespaces, not one |
| [0012](0012-ephemeral-environments-airframe-application.md) | Ephemeral (PR-preview) environments deploy through `airframe-application`, TTL-swept |
| [0013](0013-catalog-git-tag-pinned-distribution.md) | Shared catalog distributed as a git-tag-pinned Helm chart, not a bundle resolver |
| [0014](0014-keyless-signing-two-trust-roots.md) | Keyless signing uses two separate trust roots - public Sigstore for humans, self-hosted Fulcio for workloads |
| [0015](0015-provenance-policy-validates-attestation-input.md) | Provenance policy validates the attestation as input, additive to commit signing |

New decisions get a new numbered file here, not a paragraph buried in an unrelated doc.

**Note on 0014/0015**: distilled from `image-signing.md`/`commit-signing.md`/
`provenance-policy.md` as they stood 2026-09-06. A separate, concurrent session is
implementing sigstore end-to-end in GitOps fashion at the time these were written -
re-check both against the live mechanism before treating them as current if that work
has since landed.
