#!/usr/bin/env bash
# The "this environment runs no workload" check open-release-pr.yaml (Flight) and deploy-manifests.yaml
# (Ground) run before releasing an image (2026-10-10): the same yq expression in both, layering the
# human values files the way Argo CD/Helm does. Needs helm and yq.
# Usage: charts/glidepath-catalog/tests/workload_check_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
rendered="$(helm template t charts/glidepath-catalog)"
exprs="$(grep -o "yq eval-all '[^']*'" <<<"${rendered}" | sort -u)"
[[ "$(wc -l <<<"${exprs}" | tr -d ' ')" == 1 ]] || { echo "FAIL: the two tasks must use one expression, got: ${exprs}"; fail=1; }
q="$(sed -E "s/^yq eval-all '(.*)'$/\1/" <<<"$(head -1 <<<"${exprs}")")"
for t in open-release-pr deploy-manifests; do
  grep -A3 "^  name: ${t}$" <<<"${rendered}" >/dev/null || { echo "FAIL: ${t} not rendered"; fail=1; }
done
d="$(mktemp -d)"
case_() { # name base values expected
  printf '%s' "$2" > "$d/base.yaml"; printf '%s' "$3" > "$d/values.yaml"
  got="$(yq eval-all "$q" "$d/base.yaml" "$d/values.yaml")"
  [[ "${got}" == "$4" ]] || { echo "FAIL: $1: got ${got}, want $4"; fail=1; }
}
case_ "no rollout anywhere is a service"            $'appName: x\n'                   $'envName: dev\n'                        false
case_ "enabled: false in the env file"               $'appName: x\n'                   $'rollout:\n  enabled: false\n'          true
case_ "older rollout: null in the env file"          $'appName: x\n'                   $'rollout: null\n'                       true
case_ "enabled: false in base, partial env rollout"  $'rollout:\n  enabled: false\n'  $'rollout:\n  replicas: 3\n'            true
case_ "env turns it back on over base"               $'rollout:\n  enabled: false\n'  $'rollout:\n  enabled: true\n'          false
case_ "null in base, a rollout map in the env file"  $'rollout: null\n'                $'rollout:\n  replicas: 3\n'            false
[ $fail -eq 0 ] && echo ok || exit 1
