# ADR-0012: Ephemeral (PR-preview) environments deploy through `airframe-application`, TTL-swept

## Context

Per-PR preview environments (deploy the app for the lifetime of an open PR, so a
reviewer can see it running) were originally built as a bespoke, Kustomize-only
delivery path - separate rendering logic from every other tier this platform deploys
to, and a separate cleanup story to maintain alongside it.

## Decision

Ephemeral environments deploy through `airframe-application` - the same Helm chart every
other tier (dev/staging/prod) already uses - driven by an ArgoCD `ApplicationSet` PR
generator, not a bespoke Kustomize-only path. Image tagging for PR builds is sha-only
(no branch-name tag reuse across force-pushes, which would otherwise let a stale image
silently keep serving under a reused tag).

Namespace cleanup is a **TTL sweep** (teardown some fixed duration after the last
successful deploy), not cascade-delete triggered by the PR closing. Considered and
accepted as a real trade-off, not an oversight: reverting to a tracked `Namespace`
object precisely correlated to PR-open/PR-close state was evaluated and rejected in
favor of the simpler, already-proven TTL mechanism the platform uses elsewhere.

## Consequences

- The ephemeral tier behaves like every other tier instead of a parallel, bespoke
  system - one rendering path to maintain, not two.
- A preview environment can outlive its PR by design (up to the TTL window) - accepted
  cost of not wiring PR-close as a hard trigger, rather than a bug to fix.
- Credential/generator plumbing for the `ApplicationSet`'s PR source is shared with the
  rest of this platform's PR-based mechanisms, not a separate integration to keep in
  sync.
