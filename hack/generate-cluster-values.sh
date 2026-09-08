#!/usr/bin/env bash
# hack/generate-cluster-values.sh <context> <cluster-name> <output-dir>
#
# MANUAL OVERRIDE PATH ONLY as of 2026-09-05 (ADR-0006's "Update",
# docs/admin/adr/0006-cluster-agnostic-bootstrap.md) - a fresh cluster no longer needs
# this run by hand. charts/glidepath-control-plane/templates/hooks/
# fulcio-bootstrap-job.yaml now ports this exact recipe into an ArgoCD pre-install hook
# Job that generates the same root CA live, inside the cluster, on first sync -
# idempotent, same refuse-to-clobber-an-existing-root guard as this script. Use this
# script directly only to deliberately ROTATE an existing cluster's root
# (FORCE=1) - the bootstrap Job never does that on its own.
#
# Cluster-agnostic values generation: replaces hand-typing a per-cluster values file
# (openssl run by hand, kube-root-ca.crt copy-pasted, YAML assembled manually) with a
# single script run. Produces <output-dir>/values-<context>.yaml, meant to land next
# to the ArgoCD Application that consumes it via a multi-source $ref - e.g.:
#   ./hack/generate-cluster-values.sh kind-dev kind-dev \
#     ../gitops-cluster-dev/50-platform-cicd/glidepath-control-plane
# platform-cicd carries no cluster-specific state this way, staying installable
# standalone on any cluster. Note this OVERWRITES values-<context>.yaml's clusterName/
# fulcio fields specifically (see step 4 below) - preserve any other keys already in
# that file by hand, same caution as always.
#
# Only genuinely irreducible per-cluster material goes through this script: the
# cluster's own API server root CA (read live, can't be derived) and a freshly,
# independently generated Fulcio signing root (deliberately never copied between
# clusters - each cluster gets its own trust root, docs/admin/image-signing.md's
# documented procedure, run here instead of by hand). clusterName/tenantsRepoUrl/
# tenantOnboardingApplicationSetName are NOT emitted here - charts/platform-cicd-
# control-plane/values.yaml derives those from clusterName by convention (see that
# file's own comments).
#
# The private key + passphrase this script generates are used ONLY to create the
# fulcio-secret/fulcio-server-config Secrets directly on the target cluster (kubectl
# create secret) - never written to the emitted values file, never persisted to disk
# beyond a temp directory this script deletes on exit, matching every prior per-cluster
# Fulcio bootstrap in this platform's history.
#
# Idempotent for the values file (safe to re-run - overwrites values-<context>.yaml
# with the SAME cluster-CA content, byte-for-byte, since that's read live each time).
# NOT idempotent for the Fulcio Secrets - re-running this against a cluster that already
# has fulcio-secret/fulcio-server-config would mint a SECOND, different root CA and
# overwrite them, invalidating any image signed under the old root. Refuses to do that
# unless FORCE=1 is set.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

CONTEXT="${1:?usage: hack/generate-cluster-values.sh <context> <cluster-name> <output-dir>}"
CLUSTER_NAME="${2:?usage: hack/generate-cluster-values.sh <context> <cluster-name> <output-dir>}"
OUTPUT_DIR="${3:?usage: hack/generate-cluster-values.sh <context> <cluster-name> <output-dir>}"
FORCE="${FORCE:-0}"

log() { echo -e "\n\033[1;36m==> $*\033[0m"; }
warn() { echo -e "\033[1;33mwarning: $*\033[0m" >&2; }

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' is required but not installed." >&2; exit 1; }
}
require kubectl

# macOS ships LibreSSL as /usr/bin/openssl, which rejects `-newkey ed25519` outright
# ("Unknown algorithm ed25519") - confirmed live. Prefer a real OpenSSL (Homebrew's,
# where present) over whatever's first on PATH, rather than requiring the caller to
# fix their PATH by hand.
OPENSSL=openssl
for candidate in /opt/homebrew/opt/openssl/bin/openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl/bin/openssl; do
  if [[ -x "${candidate}" ]]; then
    OPENSSL="${candidate}"
    break
  fi
done
require "${OPENSSL}"
if "${OPENSSL}" version | grep -q LibreSSL; then
  echo "error: only LibreSSL found (no real OpenSSL) - LibreSSL rejects '-newkey ed25519'." >&2
  echo "Install OpenSSL 3.x, e.g. 'brew install openssl'." >&2
  exit 1
fi

if ! kubectl config get-contexts -o name | grep -qx "${CONTEXT}"; then
  echo "error: context '${CONTEXT}' not found in your kubeconfig." >&2
  exit 1
fi

if [[ "${FORCE}" != "1" ]] && kubectl --context "${CONTEXT}" get secret fulcio-secret -n fulcio-system >/dev/null 2>&1; then
  echo "error: fulcio-secret already exists on ${CONTEXT} - re-running would mint a" >&2
  echo "second, different root CA and invalidate every image already signed under the" >&2
  echo "current one. Set FORCE=1 to do this anyway (only if you mean to rotate the root)." >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

log "1/4 - reading ${CONTEXT}'s own API server root CA (kube-root-ca.crt)"
ISSUER_CA_CERT="$(kubectl --context "${CONTEXT}" get configmap kube-root-ca.crt -n default -o jsonpath='{.data.ca\.crt}')"
if [[ -z "${ISSUER_CA_CERT}" ]]; then
  echo "error: could not read kube-root-ca.crt/ca.crt from ${CONTEXT}'s default namespace." >&2
  exit 1
fi

log "2/4 - generating a fresh, independent Fulcio root CA for ${CLUSTER_NAME} (docs/image-signing.md's documented procedure)"
# ed25519, self-signed, matching Fulcio's own CI test recipe (sigstore/fulcio's
# verify-k8s.yml) - see docs/image-signing.md's own "Root CA" section for why this
# exact recipe, not a generic openssl example.
PASSPHRASE="$("${OPENSSL}" rand -base64 32)"
"${OPENSSL}" req -x509 -newkey ed25519 -sha256 \
  -keyout "${TMPDIR}/key.pem" -out "${TMPDIR}/cert.pem" \
  -subj "/CN=platform-cicd-${CLUSTER_NAME}-fulcio-root" -days 36500 \
  -passout "pass:${PASSPHRASE}"

log "3/4 - creating fulcio-secret + fulcio-server-config directly on ${CONTEXT} (private key never leaves this script)"
kubectl --context "${CONTEXT}" create namespace fulcio-system --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f - >/dev/null
kubectl --context "${CONTEXT}" create secret generic fulcio-secret -n fulcio-system \
  --from-file="${TMPDIR}/cert.pem" --from-file="${TMPDIR}/key.pem" \
  --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -
# Schema confirmed live against an existing working install (kind-dev's own
# fulcio-server-config) - not documented anywhere upstream in this repo beyond "matches
# Fulcio's own CI test recipe" (docs/image-signing.md), so pinned here explicitly now
# that it's been verified once: `ca: fileca` backend, cert/key paths matching where
# fulcio-deployment.yaml mounts fulcio-secret, ct-log-url disabled (this platform runs
# without a CT log, see docs/image-signing.md), ports matching the Deployment's own
# containerPort values.
cat > "${TMPDIR}/server.yaml" <<EOF
ca: fileca
fileca-cert: /etc/fulcio-secret/cert.pem
fileca-key: /etc/fulcio-secret/key.pem
fileca-key-passwd: "${PASSPHRASE}"
ct-log-url: ""
port: "5555"
grpc-port: "5554"
metrics-port: "2112"
EOF
kubectl --context "${CONTEXT}" create secret generic fulcio-server-config -n fulcio-system \
  --from-file="${TMPDIR}/server.yaml" \
  --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -

OUTPUT_FILE="${OUTPUT_DIR}/values-${CONTEXT}.yaml"
mkdir -p "${OUTPUT_DIR}"

log "4/4 - writing ${OUTPUT_FILE} (public material only - clusterName + both CA certs, no private key/passphrase)"
{
  echo "# values-${CONTEXT}.yaml"
  echo "#"
  echo "# Generated $(date -u +%Y-%m-%dT%H:%M:%SZ) by platform-cicd's hack/generate-cluster-values.sh ${CONTEXT} ${CLUSTER_NAME}."
  echo "# Consumed by this cluster's ArgoCD Application for glidepath-control-plane via a"
  echo "# multi-source \$ref - see that Application's own header."
  echo "# Re-run the generator (not this file by hand) if the cluster's own API server CA ever"
  echo "# rotates; NEVER re-run it to regenerate the Fulcio root without FORCE=1 - see that"
  echo "# script's own header for why."
  echo ""
  echo "clusterName: ${CLUSTER_NAME}"
  echo ""
  echo "fulcio:"
  echo "  issuerCACert: |"
  echo "${ISSUER_CA_CERT}" | sed 's/^/    /'
  echo "  rootCert: |"
  sed 's/^/    /' "${TMPDIR}/cert.pem"
} > "${OUTPUT_FILE}"

echo
echo "Wrote ${OUTPUT_FILE}. Review it, then commit it - it carries no secret material."
echo "Next: commit and push ${OUTPUT_FILE} in its own repo - the cluster's ArgoCD"
echo "Application will pick it up on next sync."
