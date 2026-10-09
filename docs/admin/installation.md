# Installation

platform-cicd installs onto any Kubernetes cluster declaratively, via ArgoCD - the
same way every other cluster-config piece gets installed: a couple of ArgoCD
`Application` manifests in that cluster's own `gitops-cluster-<name>` repo. See
`gitops-cluster-dev/50-platform-cicd/` for a working reference - it wires up
`tektoncd/operator` (Tekton Pipelines/Triggers/Chains/Dashboard/PaC in one namespace),
`glidepath-catalog`, and `glidepath-control-plane`.

Steps for a brand-new cluster:

1. Get ArgoCD running on the target cluster (out of scope here - see that cluster's
   own bootstrap).
2. Generate this cluster's own values, writing straight into its `gitops-cluster-<name>`
   repo checkout (not into `platform-cicd` - see that script's own header for why):

   ```
   ./hack/generate-cluster-values.sh <kube-context> <cluster-name> \
     ../gitops-cluster-<name>/50-platform-cicd/glidepath-control-plane
   ```

   This reads the cluster's own API server root CA live and generates a fresh,
   independent Fulcio signing root for it - it never copies another cluster's trust
   material. Commit and push the resulting file.
3. Add the two Application manifests (control-plane, catalog) to that repo, each
   multi-source: one source is this platform's chart at `charts/glidepath-catalog`
   / `charts/glidepath-control-plane`, the other a `directory` source (`exclude:
   "*"`) pointed at the cluster-config repo itself, `$ref`'d for `valueFiles` - see
   `gitops-cluster-dev/50-platform-cicd/glidepath-control-plane/application.yaml`
   for the exact shape, and [architecture-decisions](adr/) ADR-0006 for why cluster
   state never lives inside `platform-cicd` itself.
4. Push. ArgoCD takes it from there.

This keeps `platform-cicd` installable standalone on any cluster - it carries zero
cluster-specific secrets or identity in its own repo. For local/`kind` development,
`hack/kind-config.yaml` creates a raw, Calico-enabled cluster to point ArgoCD at -
there's no separate imperative install path beyond that; get ArgoCD running, then
follow the steps above.

Grafana serves the platform's own dashboards (rendered as ConfigMaps by the
control-plane chart, not a separate apply step). The Tekton Dashboard is the
complementary low-level view - installed **read-only** deliberately, matching this
platform's PaaS/RBAC posture.

## Operational notes

- **NetworkPolicy needs a real CNI.** `default-deny`/`allow-from-same-namespace`
  manifests are silent no-ops on a CNI that doesn't enforce `NetworkPolicy` (e.g.
  kindnet). TokenReview authentication on the broker - not NetworkPolicy - is the
  actual trust boundary (see [chaining.md](chaining.md)); NetworkPolicy is
  defense-in-depth on top of it. Pin a Calico/Cilium-class CNI for anything beyond a
  shared dev cluster.
- **The GitHub App needs a public endpoint.** Pipelines-as-Code's webhook delivery
  can't reach a local/private cluster directly - front the PaC controller with a
  tunnel (`cloudflared`, `ngrok`) for local dev, or real ingress/DNS for anything else.
- **`token-review-interceptor`/`glidepath-relay` images are `IfNotPresent` +
  `:latest`.** A source change under `glidepath/broker/cmd/` does nothing to a running
  cluster until you rebuild, push, and `kubectl rollout restart` the affected
  Deployment - there's no CI wired to a private cluster to do this automatically.
- **Pod Security Standards `restricted` + kaniko**: validate this combination against
  your actual target CNI before onboarding real apps - see
  [rootless-builds.md](rootless-builds.md).
- **Tekton's Affinity Assistant is off (`coschedule: disabled`).** Set in the cluster repo's
  `50-platform-cicd/tekton-operator/tektonconfig.yaml` (2026-10-09). The default (`workspaces`)
  starts a StatefulSet and placeholder pod before every build and test run, 10-25 s each, only to
  keep TaskRuns that share a PVC on one node. A single-node cluster, or one whose StorageClass pins
  a PV to a node (local-path), gets that anyway. On a multi-node cluster with network-attached
  `ReadWriteOnce` storage use `coschedule: pipelineruns`: the per-app `build-cache` PVC and the
  run's `source` PVC are both mounted by `build-source` and `unit-test`, and must land together.
- **One Trivy server holds the vulnerability DB.** `glidepath-control-plane` runs `trivy-server` in
  `platform-system` (`trivyServer` values; its tag must equal `TRIVY_VERSION` in
  `catalog/toolbox/Dockerfile`), and `image-scan` scans against it (`trivyServerUrl` in the catalog
  values). Before 2026-10-09 every scan pod downloaded the ~120 MiB DB itself. If the server does not
  answer `/healthz` or fails mid-scan, the scan runs standalone and downloads the DB as before, so an
  outage costs time, not a false failure. `generate-sbom` does not use it: a CycloneDX-only run has
  no vulnerability scanner and never needed the DB.
- **Ground deploys ask Argo CD to refresh.** `deploy-manifests` annotates the environment's
  Application in `argocd-apps` with `argocd.argoproj.io/refresh=hard` right after its push, so the
  new release file is picked up in seconds instead of on the next 120 s (+60 s jitter) poll. The
  grant is per app (`glidepath-app` `templates/env/argocd-refresh-rbac.yaml`, get+patch on its own
  Ground Applications), and the control plane's ValidatingAdmissionPolicy
  `pipeline-runner-application-refresh-only` denies any change from a `pipeline-runner`
  ServiceAccount other than that annotation and the `hangar.io/dora-*` ones, so the grant can never
  change an Application's project, destination or sync operation. It needs Kubernetes 1.30+
  (ValidatingAdmissionPolicy GA). Without the grant the deploy still works, at poll speed.

## Upper environments (staging/prod)

A separate bootstrap, covered in [multi-cluster.md](multi-cluster.md) -
`hack/bootstrap-upper-cluster.sh` installs just ArgoCD on a target cluster; this
platform's own Tekton/broker never runs there.
