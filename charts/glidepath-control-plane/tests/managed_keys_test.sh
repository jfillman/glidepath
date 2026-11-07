#!/usr/bin/env bash
# sigstore.managedKeys (C-2a, docs/admin/signing-pki-cutover.md): off, both bootstrap hooks render as before and nothing
# new but the trusted-root read grant appears; on, the hooks stop minting and ExternalSecrets build the Secrets.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
CHART=.
VALUES=tests/values.example.yaml
FAIL=0
ok()  { echo "  ok   $*"; }
bad() { echo "  FAIL $*"; FAIL=1; }
render() { helm template t "${CHART}" -f "${VALUES}" "$@" 2>&1; }
has()  { grep -qE -- "$2" <<<"$1" && ok "$3" || bad "$3 (/$2/ not found)"; }
lacks(){ grep -qE -- "$2" <<<"$1" && bad "$3 (/$2/ found)" || ok "$3"; }

OFF="$(render)"
has   "${OFF}" 'openssl rand -base64 32'                      "off: the fulcio hook still mints the root"
has   "${OFF}" 'openssl genpkey -algorithm EC'                "off: the rekor hook still mints the key"
lacks "${OFF}" 'kind: ExternalSecret'"$(printf '\n')"'metadata:'"$(printf '\n')"'  name: (fulcio|rekor)' "off: no managed ExternalSecret"
lacks "${OFF}" 'name: hangar-trusted-root$'                   "off: no trusted-root ConfigMap without a value"

PEM='-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n'
ON="$(render --set sigstore.managedKeys.enabled=true --set-string "sigstore.managedKeys.rootCaPem=${PEM}" \
  --set-string 'sigstore.managedKeys.trustedRootJson={"mediaType":"x"}')"
lacks "${ON}" 'openssl rand -base64 32'                       "on: the fulcio hook mints nothing"
lacks "${ON}" 'openssl genpkey'                               "on: the rekor hook is gone"
has   "${ON}" 'key: hangar-fulcio-example-dev-chain'          "on: the chain key is derived from the cluster name"
has   "${ON}" 'key: hangar-fulcio-example-dev-key$'           "on: the intermediate key"
has   "${ON}" 'key: hangar-fulcio-example-dev-key-password'   "on: the passphrase"
has   "${ON}" 'key: hangar-rekor-example-dev-signer'          "on: Rekor's signer"
has   "${ON}" 'fileca-key-passwd: \{\{ \.password'            "on: server.yaml is templated by ESO, the passphrase never rendered"
has   "${ON}" 'name: fulcio-root-ca'                          "on: fulcio-root-ca published by the chart"
has   "${ON}" 'name: hangar-trusted-root'                     "on: the trusted root is published"
has   "${ON}" 'resourceNames: \["fulcio-root-ca", "hangar-trusted-root"\]' "every ServiceAccount may read both"
NOPEM="$(render --set sigstore.managedKeys.enabled=true)"
has   "${NOPEM}" 'rootCaPem is required'                      "on without rootCaPem is refused"
OVR="$(render --set sigstore.managedKeys.enabled=true --set-string "sigstore.managedKeys.rootCaPem=${PEM}" --set sigstore.managedKeys.fulcioChainKey=custom-chain)"
has   "${OVR}" 'key: custom-chain'                            "an explicit key name overrides the derived one"
exit "${FAIL}"
