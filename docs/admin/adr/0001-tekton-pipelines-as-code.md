# ADR-0001: Tekton + Pipelines-as-Code, vanilla Kubernetes

## Context

The platform needed a CI/CD engine running on plain Kubernetes (no cloud- or
distribution-specific lock-in), with a platform-as-a-service model: application
developers configure pipelines through one simple file and never write pipeline
engine YAML directly. Git-triggered pipelines need webhook signature validation, PR
status checks, and retest-style comments - all things that are easy to get wrong if
built by hand.

## Decision

Use Tekton Pipelines/Triggers for the execution engine, and Pipelines-as-Code (PaC)
for everything git-triggered (push, PR, tag, release, PR-comment ChatOps). PaC's own
GitHub App handles webhook signature validation and PR status checks natively. Each
app repo needs a small, platform-generated `.tekton/*.yaml` file - pure boilerplate,
generated once at onboarding, referencing the shared Tekton catalog via the cluster
resolver. It is never hand-written or edited by a developer.

Developer-facing configuration is a single file (`cicd.yaml`) read fresh from the
triggering commit and validated against a JSON Schema with fail-fast, readable errors.
No separate sync-to-ConfigMap step, no second source of truth. This drives a fixed
superset Pipeline DAG with `when:`-guarded stages (toggle/parameterize, not an
arbitrary graph compiler) - a full config-to-generated-Pipeline compiler is a
materially heavier pattern, out of scope until there's real evidence it's needed.

## Consequences

- Webhook auth, PR status, and retest UX come from PaC's GitHub App integration, not
  custom code - no key material or rotation to manage for that path.
- Inter-stage chaining (build → test → deploy → release) is *not* a git event, so PaC
  doesn't cover it - see ADR-0002 for how that's handled instead.
- The fixed-DAG-with-toggles model is simpler to reason about and debug than an
  arbitrary graph, at the cost of not supporting pipeline shapes outside the platform's
  own stage catalog without a platform change - four stages (build/test/deploy/release)
  at the time this ADR was written, joined by a fifth (`gitops-image-bump`, for
  hand-deployed singletons whose manifests don't live in a per-app gitops repo) on
  2026-08-28. Named `flows` (`cicd.yaml`'s `flows:` field) let a tenant compose an
  ordered sequence/subset of this catalog per trigger - still no branching or arbitrary
  graph shape, so this remains the toggle/parameterize model, not the graph compiler
  ruled out above.
