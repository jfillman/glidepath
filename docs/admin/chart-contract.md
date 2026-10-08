# Glidepath ↔ app chart contract (v1)

Glidepath deploys an app by rendering a Helm chart with values files that live in the
app's own repo. This page is the whole interface between the two. A chart that satisfies
it works with Glidepath; Glidepath asks nothing else of the chart. Decision record:
[ADR-0018](adr/0018-glidepath-owns-envs-folder-and-chart-contract.md).

The reference implementation is Airframe's `charts/airframe-application`.

**Status.** Today both ApplicationSets hardcode the reference chart at a pinned tag. The
`deploy.chart` field in [Chart reference](#chart-reference) does not exist yet, so only
the reference chart can be used until it lands. Everything else here describes current
behavior, except where marked *(proposed)*.

## Applies to `deploy.target: k8s-rollout` only

Apps with `aws-ecs`, `aws-lambda`, `azure-container-apps` (or a function target) have no
chart and no environments folder, and the lower-envs ApplicationSet must not generate
anything for them. *(proposed: gate the generator on `deploy.target`; today it keys only
on files existing.)*

## How the environments are declared

An app declares its environments with `deploy.environments` (a list of `{name, tier, cluster?}`,
`tier` being `ground` or `flight`; unset means one Ground environment, `dev`). The older
`lowerEnvironments` / `upperEnvironments` / `promotionOrder` were removed on 2026-10-07 and are
refused. The chart reads the list through one helper (`glidepath-app.envEntries`). See
[ADR-0019](adr/0019-environments-as-one-config-model.md) and the `deploy.environments` section of
the [cicd.yaml reference](../user/cicd-yaml-reference.md).

## Folder layout in the app repo

```
glidepath/
  base.yaml                   optional; values shared by every lower env
  envs/<env>.yaml             one file = one lower environment (human-owned)
  envs/<env>.release.yaml     what was released (Glidepath-owned; see below)
  pr-env.yaml                 optional; values for pull-request preview environments
```

- The file name under `envs/` is a path, not an identity. The environment's name is the
  `envName:` key inside the file.
- The generator matches `glidepath/envs/*.yaml` and excludes `*.release.yaml`.
- `pr-env.yaml` sits one level up on purpose: a file under `envs/` would also spawn a
  static `pr` lower environment.
- The deploy stage writes the image to `<folder>/envs/<env>.release.yaml` (the release-file
  split; the default since 2026-10-06). On an app's first deploy after that it also removes
  `release.image`, `rollout.image` and a bootstrap `rollout: null` from the env file, in the
  same commit. `deploy.releaseFile` may only name that same path (`{env}` placeholder); any
  other path is refused, because the lower-envs ApplicationSet would never read it.

### Folder: `glidepath/` (formerly `platform/`)

The environments folder is `glidepath/`. Every app repo moved off `platform/` by
2026-10-07 and the dual-path shims were removed then: the ApplicationSets, the catalog
Tasks, Backstage and Tower read and write only `glidepath/`. A repo still carrying a
`platform/` folder deploys nothing from it.

The PaC config-only-push exemption skips builds for pushes that only touch `cicd.yaml`,
`glidepath/` or `.tekton/` (the generated boilerplate, so merging an onboarding-resync PR
does not build). An app gets the current exemption when its `.tekton/` is next re-synced
(any `cicd.yaml` change).

## Values merge order

Lowest to highest precedence: `base.yaml`, `envs/<env>.yaml`, the release file. Glidepath
then injects its own values on top, as `helm.valuesObject`, so a values file cannot
override them. Missing `base.yaml` and release files are tolerated.

## What Glidepath injects (the chart must accept these)

| Key | Value |
|---|---|
| `appName` | the app's name |
| `cluster` | the cluster name the environment lives on |
| `envName` | the environment name (`pr-<number>` for previews) |
| `namespace.labels` | labels the chart must put on the Namespace it renders, including `hangar.io/managed-secrets: "true"` (and `hangar.io/ephemeral-env: "true"` for previews) |
| `release.image.repository`, `release.image.tag` | previews only: stamped per pull request |

The chart must render its own Namespace object (not rely on `CreateNamespace=true`),
tracked so that pruning works, and label it as above. Otherwise the labels are lost and
registry credentials never reach the namespace.

The labeled namespace receives a `kubernetes.io/dockerconfigjson` Secret named
`registry-credentials` (Glidepath's ClusterExternalSecret). The workload must pull with it,
through the pod's `imagePullSecrets` or its ServiceAccount's; app images are private, so a
chart that leaves it out deploys a pod stuck in `ImagePullBackOff` (found by the
conformance-sample canary, 2026-10-07).

## The release keys (the only values Glidepath writes)

Glidepath's deploy stage writes exactly these into the release file and nothing else:

```yaml
release:
  image:
    repository: ghcr.io/<owner>/<app>
    tag: <tag>
```

`releaseTracking.*` is written by the upper-environment release flow into the gitops
repo's own release file, not into `glidepath/envs/`. A release file containing anything
else is an error. Human files must not contain `release` or `releaseTracking`.

The chart must treat `release.image` as the image to run. A deprecated fallback to
`rollout.image` is allowed for a transition window.

## What the deploy stage waits on

After Glidepath commits the release file it waits for health. It assumes:

1. An ArgoCD Application named `<appName>-<envName>`, in the namespace
   `app-<appName>-<envName>`.
2. An Argo Rollouts `Rollout` named `<appName>` in that namespace. Glidepath polls
   `.status.phase`: `Healthy` passes, `Degraded` fails, anything else keeps waiting until
   a timeout.

A chart that uses a plain `Deployment` does not satisfy this today. *(proposed:
`deploy.healthCheck` choosing the resource kind and name, so a chart can use a Deployment
or a StatefulSet.)*

## What Tower reads *(reference chart convention, not required by Glidepath)*

Tower derives the Topology, Deployments and Overview tabs from the same Rollout, the
namespace name and the image's provenance. A chart that differs still deploys; those tabs
show less.

## Chart reference *(proposed)*

```yaml
deploy:
  chart:
    repoURL: https://github.com/<owner>/<repo>   # git repo or OCI registry
    path: charts/my-chart                        # or `chart:` + `version:` for a registry
    targetRevision: v1.2.3
```

Unset means the reference chart at the version Glidepath pins today, so existing apps do
not change. Both ApplicationSets read this field from the app's own `cicd.yaml`.

## Conformance checklist

A chart conforms if all of the following hold:

- [ ] It accepts `appName`, `cluster`, `envName`, `namespace.labels`, `release.image`.
- [ ] It renders a labeled Namespace and a Rollout named `<appName>`.
- [ ] The Rollout's pods pull with the `registry-credentials` Secret.
- [ ] `helm template` with only `base.yaml` plus one env file plus a release file succeeds.
- [ ] Rendering with the release file removed still succeeds (bootstrap, before the first build).
- [ ] An unknown key in a values file fails loudly or is documented as ignored.
