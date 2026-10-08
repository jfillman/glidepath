# ADR-0024: Cluster taxonomy: zone, roles and environment tier

*Status: Accepted (2026-10-08). Built: the registry fields, the control plane's own zone and roles, the
`production` environment flag and its checks. Not built: Ground environments on other clusters
([known-gaps.md](../known-gaps.md) #39), and moving Airframe's cluster registry onto these fields (#40).*

## Context

Clusters had no declared purpose. "Dev" meant whichever cluster runs the control plane (Tekton, Pipelines-as-Code,
Tekton Results, the relay) and every Ground environment assumed it. "Upper" meant a cluster in the control plane's
`clusters:` registry, which holds only a name and a relay secret. Tier (`ground`/`flight`) belongs to an environment,
not a cluster: a Flight environment may run on the dev cluster, and kind-prod hosts staging, prod and Backstage.

Two questions had no answer in the platform: what makes a cluster upper, and what stops a production environment
from landing somewhere a pipeline can write to directly. The owner raised them while weighing Ground environments
on other clusters ([envs-overhaul-requirements.md](../envs-overhaul-requirements.md) Q3).

## Decision

Three separate properties.

| Property | Belongs to | Values | What it decides |
|---|---|---|---|
| **zone** | cluster | `lower`, `upper` | Which credentials may reach it and how changes arrive |
| **roles** | cluster | `control-plane`, `workloads`, `platform-services` | What the platform installs there |
| **tier** (+ `production`) | environment | `ground`/`flight`; `production: true` | How a change reaches the environment |

**An upper cluster** is one that nothing below it holds a write credential to, and that changes only through a
reviewed merge to a repository its own Argo CD watches ([ADR-0005](0005-multicluster-per-cluster-argocd.md)).
Credentials flow down only: Backstage on kind-prod may read kind-dev, never the reverse. **A lower cluster** may be
written to by pipelines directly; the control plane's cluster is always lower, since its pipelines deploy to it.

**Production** is a property of an environment, not a cluster: the one serving real users and real data. kind-prod is
an upper cluster that also hosts staging, which is fine.

Rules, enforced by `glidepath-app`'s `deploy.environments` checks:

- A production environment is a Flight environment.
- A production environment of a Kubernetes target runs on an upper cluster (its `cluster`, or the control plane's own
  cluster when unset, which is lower, so it must set one).
- A named `cluster` must be in the registry.
- A Ground environment sets no `cluster` (unchanged; multi-cluster Ground is a future feature, below).

A cloud target's production environment has no cluster (its runtime is the cloud account), so only the tier rule
applies to it.

### Where it lives

- `glidepath-control-plane` values: `clusterZone`/`clusterRoles` describe the control plane's own cluster (must be
  `lower` and include `control-plane`); each `clusters[]` entry gains `zone` (default `upper`, which every cluster
  registered before this was) and `roles` (default `[workloads]`; `control-plane` is not allowed there).
- The `cluster-registry` ConfigMap carries `zone` and `roles` per entry.
- The control plane forwards `clusterTaxonomy: {self, zones}` to every app's `glidepath-app` through the
  tenant-onboarding ApplicationSet, the same way it forwards `defaultChart`.
- `cicd.schema.json`: `deploy.environments[].production` (boolean).

## Consequences

- A production environment can no longer be declared on the control-plane cluster or on an unregistered or lower
  cluster; the app's `-cicd` sync fails with the reason, and `validate-cicd-config` rejects the schema violation.
- Existing apps are unaffected: no environment set `production`, and every app rendered identically with the
  taxonomy forwarded (12 live apps, 2026-10-08).
- Declaring a lower cluster in the registry is allowed but nothing deploys to it yet.
- There are now two cluster registries with overlapping meaning: Glidepath's (`clusters:`) and Airframe's
  (`gitops-cluster-dev/00-bootstrap/cluster-registry/*.yaml` ConfigMaps, `type: dev|upper`, read by the
  ApplicationEnvironment Composition). Airframe's `type: dev` is zone lower with the control-plane role; `type: upper`
  is zone upper. Consolidating them is known-gaps #40.

### Future: Ground environments on other clusters

Wanted (the owner likes several dev clusters run by one Glidepath), not built. With this taxonomy it means: a Ground
environment may name a **lower** cluster with the `workloads` role. What it needs, each currently tied to the control
plane's cluster:

1. The Ground ApplicationSet rendered by that cluster's own Argo CD (watching the app repos' `glidepath/envs/`), so the
   control plane holds no credential to it, or the control plane's Argo CD holding one (acceptable in a lower zone,
   but it breaks "every workload cluster runs its own Argo CD").
2. `deploy-manifests` waiting on Rollout health through the relay's facts ([ADR-0021](0021-rollout-facts-and-release-record.md)),
   as Flight already does, instead of watching the Rollout through same-cluster RBAC. Worth doing first, on the dev
   cluster alone: one health path for Ground and Flight.
3. Per-cluster secrets store (ESO + the app's Infisical project) and registry pull credentials, scaffolded by Airframe.
4. Tests: Testkube runs on the control plane's cluster and reaches services by in-cluster DNS.
5. Tower and Backstage: a cluster entry per workload cluster; Tower stops treating Ground as `kind-dev`.

Cheaper alternatives for some reasons to want it (architecture, GPU): node pools and taints on one dev cluster.
