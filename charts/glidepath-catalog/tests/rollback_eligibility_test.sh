#!/usr/bin/env bash
# Runs extract-promoted-image.yaml's rollback-eligibility step (ADR-0021 phase 4) against a stub
# kubectl and checks the answer. Needs helm, python3 (pyyaml) and jq.
# Usage: charts/glidepath-catalog/tests/rollback_eligibility_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
script="$(mktemp)"
helm template t charts/glidepath-catalog --show-only templates/tasks/extract-promoted-image.yaml \
  | python3 -c "
import sys,yaml
t=yaml.safe_load(sys.stdin.read())
print([s for s in t['spec']['steps'] if s['name']=='rollback-eligibility'][0]['script'])" > "$script"
# The step writes its results to $(step.results.<name>.path); point them at files.
out_dir="$(mktemp -d)"
sed -i.bak -e "s#\$(step.results.eligible.path)#${out_dir}/eligible#" -e "s#\$(step.results.note.path)#${out_dir}/note#" "$script"

stubdir="$(mktemp -d)"
export ARGSLOG="${stubdir}/args"
cat > "${stubdir}/kubectl" <<'STUB'
#!/usr/bin/env bash
[[ "${NOLIST:-}" == 1 ]] && { echo "forbidden" >&2; exit 1; }
echo "$*" > "${ARGSLOG}"
printf '%s' "${RECORDS}"
STUB
chmod +x "${stubdir}/kubectl"

rec() { # releaseId image healthyAt
  jq -n --arg r "$1" --arg i "$2" --arg h "$3" '{metadata:{name:("release-tracking-"+$r)},data:({releaseId:$r,image:$i,healthyAt:$h}|with_entries(select(.value!="")))}'
}
run() { # image manifest records [nolist]
  PATH="${stubdir}:${PATH}" IMAGE_REF="$1" MANIFEST="$2" RECORDS="{\"items\":[$3]}" NOLIST="${4:-0}" \
    REPO_URL="https://github.com/o/gitops-sky-marshall.git" NAMESPACE=app-sky-marshall-cicd bash "$script" >/dev/null 2>&1 \
    || { echo "FAIL: step exited non-zero"; fail=1; }
  printf '%s|%s' "$(cat "${out_dir}/eligible")" "$(cat "${out_dir}/note")"
}
img() { echo "ghcr.io/o/sky-marshall:0.1.$1-abc"; }
seven="$(for i in 1 2 3 4 5 6 7; do printf '%s,' "$(rec "r$i" "$(img $i)" "2026-10-0${i}T00:00:00Z")"; done)"
seven="${seven%,}"

got="$(run "$(img 3)" kind-prod/staging/release.yaml "$seven")"
[[ "$got" == true\|*r3* ]] || { echo "FAIL: an image among the last five healthy releases is eligible: $got"; fail=1; }
grep -q "hangar.io/app=sky-marshall,hangar.io/env=staging,hangar.io/cluster=kind-prod" "${ARGSLOG}" \
  || { echo "FAIL: records must be selected by app, env and cluster: $(cat "${ARGSLOG}")"; fail=1; }
got="$(run "$(img 2)" kind-prod/staging/release.yaml "$seven")"
[[ "$got" == false\|* ]] || { echo "FAIL: the sixth-newest healthy release is outside the window: $got"; fail=1; }
got="$(run "$(img 9)" kind-prod/staging/release.yaml "$(rec bad "$(img 9)" ""),$seven")"
[[ "$got" == false\|* ]] || { echo "FAIL: a release that never reached healthy is not a target: $got"; fail=1; }
got="$(run "$(img 7)" glidepath/releases/prod.yaml "$seven")"
[[ "$got" == false\|*"not a Kubernetes"* ]] || { echo "FAIL: a cloud release pin is never advisory: $got"; fail=1; }
got="$(run "$(img 7)" kind-prod/staging/release.yaml "$seven" 1)"
[[ "$got" == false\|*"could not read"* ]] || { echo "FAIL: unreadable records mean not eligible: $got"; fail=1; }
got="$(run "$(img 7)" kind-prod/staging/values.yaml "$seven")"
[[ "$got" == true\|* ]] || { echo "FAIL: values.yaml (pre-split) environments are covered too: $got"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
