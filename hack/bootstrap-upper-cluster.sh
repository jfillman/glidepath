#!/usr/bin/env bash
# hack/bootstrap-upper-cluster.sh
#
# Bootstraps an "upper environment" cluster (staging/prod-tier, targeted via cicd.yaml's
# cluster: field) with a pinned ArgoCD install. The dev cluster (which runs Tekton/PaC/
# the broker, an almost entirely disjoint set of components) installs declaratively
# instead - see docs/admin/installation.md.
#
# No ArgoCD Notifications controller - the release-outcome feedback loop is ArgoCD sync
# hooks instead (PostSync/SyncFail Jobs, see catalog/lib/argocd-outcome-hook.sh):
# Notifications fired on any completed sync, including pure selfHeal drift with no
# release involved. See docs/admin/multi-cluster.md.
#
# Targets an EXISTING cluster context - never runs `kind create cluster`. Note:
# "kind-prod" (the first upper cluster) is named "prod" but currently
# hosts the "staging" environment per cicd.yaml's env->cluster mapping - see
# docs/admin/multi-cluster.md's terminology note. Idempotent, safe to re-run.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

CONTEXT="${UPPER_CLUSTER_CONTEXT:-kind-prod}"
ARGOCD_CHART_VERSION="10.3.2" # app version v3.5.0 - pinned deliberately, not "latest" -
                               # an untracked "latest" install is how this platform lost
                               # any record of which version was actually running before

log() { echo -e "\n\033[1;36m==> $*\033[0m"; }
warn() { echo -e "\033[1;33mwarning: $*\033[0m" >&2; }

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' is required but not installed." >&2; exit 1; }
}

require kubectl
require helm

log "0/3 - target the existing ${CONTEXT} cluster (not creating one)"
if ! kubectl config get-contexts -o name | grep -qx "${CONTEXT}"; then
  echo "error: context '${CONTEXT}' not found in your kubeconfig. This script assumes" >&2
  echo "the upper-env cluster already exists - it does not create one. If it's a stopped" >&2
  echo "podman-backed kind cluster, 'podman start <name>-control-plane' first." >&2
  exit 1
fi

log "CNI check (informational)"
if ! kubectl --context "${CONTEXT}" get pods -n kube-system -l k8s-app=calico-node --no-headers 2>/dev/null | grep -q .; then
  warn "${CONTEXT} runs kindnet, not Calico - NetworkPolicy is a silent no-op here."
  warn "See docs/admin/multi-cluster.md."
fi

log "Cross-cluster reachability (informational, verified once live)"
echo "  podman's kind provider puts every kind cluster's node container on one shared"
echo "  bridge network by default - the dev cluster and kind-prod's nodes reach each"
echo "  other directly by container IP, no 'podman network connect' needed. A NodePort Service"
echo "  on either cluster is reachable from the other's pods on the same basis. See"
echo "  docs/admin/multi-cluster.md."

log "1/2 - ArgoCD (pinned chart ${ARGOCD_CHART_VERSION})"
helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
kubectl --context "${CONTEXT}" create namespace argocd --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -
helm upgrade --install argocd argo/argo-cd \
  --kube-context "${CONTEXT}" \
  --namespace argocd \
  --version "${ARGOCD_CHART_VERSION}" \
  --set dex.enabled=false \
  --wait --timeout 5m

log "app-of-apps root Application"
echo "  Not yet applied by this script - per-tenant gitops repo's clusters/${CONTEXT}/"
echo "  applications/ path needs a one-time root Application pointed at it. See"
echo "  docs/admin/multi-cluster.md's Phase D section; done by hand for the first tenant."

log "Outcome-relay token (2026-08-19: Infisical-backed, once per cluster, not per app)"
echo "  ${CONTEXT}'s own platform-cicd-kind-prod-style Infisical project (see"
echo "  gitops-cluster-kind-prod/10-crds-operators/external-secrets/ for the real"
echo "  example) needs a platform-outcome-relay-token key holding the SAME value that"
echo "  backs cluster-registry.relaySecretName on the dev cluster - plant it once, via"
echo "  the Infisical UI/API, never through this script or pasted through chat."
echo "  airframe-application's own release-tracking/relay-token-external-secret.yaml syncs"
echo "  it into every onboarded app's namespace automatically from there - see"
echo "  docs/admin/multi-cluster.md and docs/admin/secrets-management.md."

log "2/2 - done"
echo "ArgoCD UI: kubectl --context ${CONTEXT} -n argocd port-forward svc/argocd-server 8080:443"
echo "Initial admin password: kubectl --context ${CONTEXT} -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
