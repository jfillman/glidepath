# ADR-0013: Shared catalog distributed as a git-tag-pinned Helm chart, not a bundle resolver

## Context

Every onboarded Application's `.tekton/*.yaml` references the shared Tekton catalog
(Tasks/Pipelines) via the cluster resolver, generated once at onboarding and never
hand-edited (ADR-0001). Many tenants share one catalog, so a catalog change has to be
rollout-able without breaking every tenant simultaneously, and a tenant needs a way to
test a catalog change before it reaches every Application.

## Decision

The catalog is a real Helm release, and each consuming chart/`ApplicationSet` pins a
specific git tag (`targetRevision`) rather than resolving "latest" off a moving branch.
Upgrading a tenant onto a newer catalog version is "repoint that pin," a reviewed,
explicit change - not an implicit effect of the catalog repo simply moving forward.
Testing a change ahead of a real rollout means pointing a canary tenant/
`ApplicationSet` at a feature branch (or a `platform-catalog-canary` install) for a
genuine live staging target, not just `helm template`/`lint`.

A Tekton bundle resolver (the catalog packaged and versioned as an OCI artifact,
referenced by digest) was identified as the more structurally correct target state -
immutable-by-digest, no reliance on git-tag discipline - but is explicitly **not built
yet** ("Option B"), not a rejected alternative.

## Consequences

- A tenant can deliberately stay pinned to an older catalog tag (staged rollout) - or
  drift accidentally, since nothing currently forces a re-pin. Real operational cost:
  there is no dashboard or alert today for "this tenant is N catalog versions behind."
- CI can verify a catalog change renders and lints correctly, but cannot fully prove a
  live tenant's actual behavior against it without a real canary rollout - "what CI
  actually checks (and what it can't)" is a known, stated gap, not silently assumed
  covered.
- Moving to the bundle-resolver model later is a materially different versioning and
  rollout story (immutable digest references instead of mutable git tags/branches) -
  worth revisiting if git-tag drift across tenants ever becomes a real, recurring
  problem rather than a theoretical one.
