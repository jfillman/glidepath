# ADR-0023: Per-app chart through `deploy.chart`

*Status: Accepted (2026-10-06). Slice 1 built and live (2026-10-07: `cluster-defaults.yaml` per cluster; the values gate follows it). Slice 2 built (2026-10-07: schema and validator; nothing renders the field until slices 3 and 4). Requirement R6 of
[envs-overhaul-requirements.md](../envs-overhaul-requirements.md); phase 8.*

## Context

[ADR-0018](0018-glidepath-owns-envs-folder-and-chart-contract.md) made the chart that
renders an app's environments per-app configuration, with
[chart-contract.md](../chart-contract.md) as the interface. Today nothing can choose it:
every consumer hardcodes `airframe/charts/airframe-application` at a tag.

| Renders | Where the pin lives | What feeds that generator |
|---|---|---|
| Ground environments | lower-envs ApplicationSet in `gitops-cluster-dev`'s tenant chart (and apron's copy) | the app repo's env files |
| Flight environments | tenant-onboarding ApplicationSet in each `gitops-cluster-<name>` (twice per file: template and templatePatch) | `tenants/<app>/<env>/identity.yaml` in the cluster's tenants repo |
| PR previews | `glidepath-app` `idpServiceCatalog.chartVersion` | the app's `cicd.yaml` (a values file of `glidepath-app`) |
| The `values` gate | `valuesValidatorImage` (the validator carries its own chart copy) | none |

The pins already drift: on 2026-10-06 the `values` gate validated against
airframe-application v0.3.97 while every environment rendered v0.3.118. None of the
ApplicationSets reads the app's `cicd.yaml`, so a `deploy.chart` field has nowhere to be
read.

## Decision

### The field

```yaml
deploy:
  chart:                                 # app-wide; unset = the cluster default
    repoURL: https://github.com/<owner>/<repo>
    path: charts/<chart>                 # git source; or `chart:` for a Helm/OCI registry
    targetRevision: v1.2.3
  environments:
    - name: dev
      tier: ground
      chart: { targetRevision: v1.2.4 }  # per environment, merged over deploy.chart
```

Precedence, highest first: the environment's `chart`, then `deploy.chart`, then the
cluster default. A per-environment value sets only the fields it changes (the same rule as
the per-environment cloud overrides). Only `deploy.target: k8s-rollout` has a chart.

### Ground: the lower-envs ApplicationSet moves into `glidepath-app` (option B)

`glidepath-app` is already rendered per app with the app's `cicd.yaml` as a Helm values
file; that is how the PR-preview ApplicationSet works. The lower-envs ApplicationSet moves
there, and its chart source becomes plain Helm templating: the environment's `chart`
merged over `deploy.chart` merged over the cluster default, resolved per environment when
the chart renders. No ArgoCD generator reads `cicd.yaml`.

This also finishes ADR-0018's ownership move: the environments machinery is Glidepath's,
not each cluster repo's.

### Flight: the chart is recorded in `identity.yaml` (option C)

The tenant-onboarding ApplicationSet already reads `tenants/<app>/<env>/identity.yaml`. It
gains an optional `chart` block there, and the template uses it with the cluster default
as fallback (`default`), so a missing or partial block renders today's chart.
`ApplicationEnvironment` writes it when the environment is created; when `deploy.chart`
(or that environment's `chart`) changes in `cicd.yaml`, Glidepath opens a PR on the
tenants repo updating it, the same two-PR pattern as adding a Flight environment
([ADR-0019](0019-environments-as-one-config-model.md)). A chart change to a Flight
environment is therefore approval-gated like a release.

### One default per cluster, and the gate follows the chart

Each cluster's default chart reference lives in one place, read by both ApplicationSets
and by `glidepath-app`. The `values` gate validates against the chart that environment
will render: the default validator image is bumped in the same change as the default
chart, and an app with its own chart gets a plain `helm template` with that chart (Airframe's
strict keys and component checks do not apply to a chart they do not describe). Tower shows
the raw YAML editor instead of the values form for such an app; the form is generated from
Airframe's schema.

### Deploy RBAC moves with the environment

`glidepath-app`'s deploy RBAC (the pipeline runner's Role and RoleBinding in
`app-<app>-<env>`) is rendered by the `<app>-cicd` Application, but the namespace it targets
is created by that environment's own Application, which first syncs minutes later on a new
app. Until then every `<app>-cicd` sync fails with "error getting namespace" and ArgoCD
retries (seen on flight-attendant, 2026-10-06; it recovered on the second retry). In slice 3
the RBAC is rendered by the environment's Application instead, in the same sync as its
namespace, so the order cannot race. No retry tuning is added in the meantime.

## Alternatives considered

**A. A matrix generator that also reads the app's `cicd.yaml`.** The smallest change, but a
`cicd.yaml` that fails to parse or is removed makes the generator return zero elements, and
ArgoCD then deletes every environment's Application, with the resources finalizer deleting
the workloads. Rejected for that failure mode.

**Writing the Flight chart into the gitops repo** (a `chart.yaml` beside `values.yaml`,
read by a second generator) has the same zero-element failure when the file is missing.

## Procedures

### Upgrading the cluster default (all apps that do not pin)

1. Cut the Airframe tag; CI publishes the chart and a validator image of the same tag.
2. Offline: render every live env file (Ground and Flight) on the old and the new chart
   (`helm template`, YAML-parse, diff), and review the per-app diffs. Treat "identical" as
   proven only when each render produced objects.
3. Canary (below) on one Ground environment, then one Flight environment.
4. One PR per cluster bumps the default chart and the validator image together.
5. Watch: every Application Synced, every Rollout Healthy with new pods. A green sync is
   not proof the workload came up.
6. Rollback is reverting that PR.

Apps that pin their own chart do not move. Tower lists which apps pin what, so a pin cannot
go stale unnoticed.

### Canary

A canary is the override, used by the platform team: one Ground environment gets a
per-environment `chart` (a `cicd.yaml` PR), then one Flight environment (an `identity.yaml`
PR on the tenants repo, approval-gated). After both are healthy, the default moves (step 4)
and the canary overrides are removed. Reverting a canary is reverting its PR.

A chart canary is independent of an Argo Rollouts canary of the app's image; a chart
change reaches pods through the Rollout's own strategy like any other spec change.

## Consequences

- Moving the lower-envs ApplicationSet must not delete running environments: the old
  ApplicationSet's Applications are adopted by the new one. Same care as known-gap #29:
  `preserveResourcesOnDeletion` first as its own change, verified, then the swap, on one
  app first.
- `identity.yaml` gains a field written by two paths (the XR at creation, Glidepath's sync
  PR afterwards); the sync PR is the source of truth after creation.
- `cicd.schema.json` gains `deploy.chart` and `environments[].chart`: a toolbox rebuild and
  a `toolboxImage` bump in three values files.
- A custom chart must pass chart-contract.md's conformance checklist; Glidepath does not
  check that beyond the render in the `values` gate.

## Build order

| Slice | What | Verifiable by |
|---|---|---|
| 1 | One default chart reference per cluster; validator image bumped with it (fixes the v0.3.97 drift) | All consumers render the same tag; the gate validates against it |
| 2 | Schema: `deploy.chart`, `environments[].chart` | Toolbox rebuilt and bumped; validator accepts and rejects the right shapes |
| 3 | Lower-envs ApplicationSet in `glidepath-app`, adopting existing Applications; the deploy RBAC (`templates/env/deploy-rbac.yaml`) moves into each Ground environment's Application, beside the namespace it targets | Every live Ground Application byte-identical before and after (dry-run, then live); a fresh onboarding syncs `<app>-cicd` first time, with no "error getting namespace" retries |
| 4 | `identity.yaml` `chart` + template default; Glidepath sync PR | A Flight canary on one app renders the pinned chart; others unchanged |
| 5 | `values` gate and Tower for a non-Airframe chart | A conformant scratch chart deploys to a Ground environment and passes the gate |
