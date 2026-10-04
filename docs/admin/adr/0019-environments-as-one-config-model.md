# ADR-0019: Environments are one config model, defined once in `cicd.yaml`

**Status: proposed (2026-10-04); plan approved by the owner.** Most open questions are
answered (see [requirements §6](../envs-overhaul-requirements.md#6-decisions-and-what-is-still-open)).
Q2 (who opens the gitops PR for a Flight env) and Q3 (a Ground env on another cluster) are
proposed and awaiting confirmation, so the status stays Proposed until they are.

## Context

An environment is not an object today. It is whatever `deploy.lowerEnvironments`,
`deploy.upperEnvironments`, `deploy.promotionOrder`, a pipeline step's `env`, the
existence of `platform/envs/<env>.yaml`, the existence of a gitops directory, and (for
cloud targets) deploy-run history happen to agree on. Nothing checks that they do.

The consequences were found on real apps: a declared env rendered as a ghost card beside
the working one; a Lambda function received a Kubernetes namespace and an Application it
cannot use; a cloud app can deploy to only one resource however many envs it names; a
cloud env cannot be gated because Flight means a gitops release PR and functions have no
gitops repo; and Ground and Flight environments are edited in two different tabs with a
form for one and raw YAML for the other. Details and evidence:
[envs-overhaul-requirements.md](../envs-overhaul-requirements.md).

## Decision (proposed)

1. `deploy.environments[]` in `cicd.yaml` is the single definition of the environments.
   Each entry has a `name`, a `tier` (`ground` or `flight`), and where relevant `cluster`,
   `target`, target config and `approval`. List order is promotion order.
2. Pipeline steps reference environments by name; an undefined name fails validation.
3. `deploy.target` and its config block remain the app default. An environment may
   override the target and the fields of its config (function name, region, ECS service,
   Container App), so one app can deploy different environments to different resources.
4. Cluster artifacts (the env file, namespace, RBAC, ApplicationSet entry, gitops
   directory) are derived from the definition and exist only for `k8s-rollout`
   environments. A cloud environment creates none.
5. Both shapes are read during a window. `deploy.environments` wins when present; the
   old fields are otherwise mapped in memory. Glidepath opens a migration PR per app. The
   old shape is removed last, after every app has been checked.
6. Tower manages environments in one flow for both tiers (a dedicated Environments tab
   is the recommended shape), with changes expressed as pull requests and a preview of
   every file touched, including for deletion.
7. Decided with the owner: a cloud Flight environment is approved by a PR on the source
   repo changing a per-env release pin (no gitops repo); cloud credentials are one set per
   app for now; an environment's target cannot change (delete and re-add); the product keeps
   the words Ground and Flight; App Configuration stays a separate tab; the Environments
   tab is a table with in-place expansion and a pending-changes panel.
8. This builds on, and does not change, ADR-0018 (the `glidepath/` folder and the chart
   contract). The folder move and `deploy.chart` are phases of the same work.

## Consequences

- One place to add, reorder or remove an environment, and one check that it is
  consistent. The ghost-env class of bug has no source.
- Cloud apps get real multi-environment support, and a defined path to approval gates
  (open question 1).
- A schema change means a toolbox rebuild and bump; the in-memory mapping must be checked
  against every real `cicd.yaml` before any app depends on it.
- A longer compatibility window than a one-shot migration, deliberately: a bad migration
  would affect every app at once.
- Adding a Flight environment still touches two repos (source `cicd.yaml`, gitops
  directory) unless open question 2 is resolved otherwise.
