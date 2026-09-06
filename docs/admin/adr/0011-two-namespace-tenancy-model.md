# ADR-0011: Every Application is (at least) two peer namespaces, not one

## Context

Onboarding an Application needs two structurally different things: somewhere its CI
pipelines actually execute (with RBAC for `pipeline-runner`, PaC's `Repository` CR, the
chaining `Trigger` CRs, kaniko's registry-push credentials), and somewhere the deployed
application itself runs and serves traffic. Putting both in one namespace would mean
pipeline execution RBAC and running-workload RBAC share the same blast radius, and
every additional upper environment (`staging`, `prod`) would have to fight the same
namespace for Deployment/Service names.

## Decision

Every Application gets namespaces following one flat `<type>-<app-name>-<env>` pattern,
with `-cicd` and each real environment name treated as **peer** values of `<env>`, not
a base-plus-suffix pair:

- `<type>-<app-name>-cicd` - the Application's CI control plane. `pipeline-runner` and
  its RBAC live here; PaC creates `build`/`test` PipelineRuns here; kaniko's push
  credentials sit here. Pipelines *execute* here, nothing else.
- `<type>-<app-name>-<env>` (one per real environment: `dev`, and later `staging`/
  `prod` per `cicd.yaml`'s `deploy.upperEnvironments`) - where the long-lived
  `Deployment`/`Service` actually runs, each environment fully isolated from every
  other.

## Consequences

- `pipeline-runner`'s `Role` only grants rights inside the Application's own `-cicd`
  namespace - a namespace-scoped `Role` never extends into a different namespace by
  construction. Deploying into an env namespace needs its own, separate grant, applied
  once per environment (`deploy-rbac.yaml`) - a real gap this design surfaced (caught by
  reasoning through what actually calls what, not by running it and hoping), not free.
- Each environment's Deployment gets a fully isolated namespace rather than every
  environment's objects colliding in one shared namespace - `dev`/`staging`/`prod` can
  never step on each other's Service/Deployment names.
- Adding a new upper environment is "provision a new namespace + its own RBAC grant,"
  not a toggle inside an existing shared namespace - more objects to manage, but no
  environment can accidentally inherit another environment's access.
