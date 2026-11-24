#!/usr/bin/env bash
# Runs extract-promoted-image.yaml's rollback-eligibility step (ADR-0021 phase 4) against a stub
# kubectl and checks the answer. Needs helm, python3 (pyyaml) and jq.
# Usage: charts/glidepath-catalog/tests/rollback_eligibility_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
script="$(mktemp)"
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/extract-promoted-image.yaml \
  | python3 -c "
import sys,yaml
t=yaml.safe_load(sys.stdin.read())
print([s for s in t['spec']['steps'] if s['name']=='rollback-eligibility'][0]['script'])" > "$script"
# The step writes its results to $(step.results.<name>.path); point them at files.
out_dir="$(mktemp -d)"
sed -i.bak -e "s#\$(step.results.eligible.path)#${out_dir}/eligible#" -e "s#\$(step.results.note.path)#${out_dir}/note#" \
  -e "s#\$(step.results.change-kind.path)#${out_dir}/kind#" -e "s#\$(step.results.applicability.path)#${out_dir}/applicability#" \
  -e "s#\$(step.results.change-reason.path)#${out_dir}/reason#" "$script"

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
    REPO_URL="https://github.com/o/gitops-sky-marshall.git" NAMESPACE=app-sky-marshall-cicd \
    CLASSIFICATION_SOURCE="${SOURCE:-none}" CHANGE_KIND="${KIND:-}" GATE_APPLICABILITY="${APPLICABILITY:-run}" CHANGE_REASON="" \
    ROLLBACK_APPLICABILITY=advisory bash "$script" >/dev/null 2>&1 \
    || { echo "FAIL: step exited non-zero"; fail=1; }
  printf '%s|%s' "$(cat "${out_dir}/eligible")" "$(cat "${out_dir}/note")"
}
img() { echo "ghcr.io/o/sky-marshall:0.1.$1-abc"; }
seven="$(for i in 1 2 3 4 5 6 7; do printf '%s,' "$(rec "r$i" "$(img $i)" "2026-10-0${i}T00:00:00Z")"; done)"
seven="${seven%,}"

got="$(run "$(img 3)" example-prod/staging/release.yaml "$seven")"
[[ "$got" == true\|*r3* ]] || { echo "FAIL: an image among the last five healthy releases is eligible: $got"; fail=1; }
grep -q "hangar.io/app=sky-marshall,hangar.io/env=staging,hangar.io/cluster=example-prod" "${ARGSLOG}" \
  || { echo "FAIL: records must be selected by app, env and cluster: $(cat "${ARGSLOG}")"; fail=1; }
got="$(run "$(img 2)" example-prod/staging/release.yaml "$seven")"
[[ "$got" == false\|* ]] || { echo "FAIL: the sixth-newest healthy release is outside the window: $got"; fail=1; }
got="$(run "$(img 9)" example-prod/staging/release.yaml "$(rec bad "$(img 9)" ""),$seven")"
[[ "$got" == false\|* ]] || { echo "FAIL: a release that never reached healthy is not a target: $got"; fail=1; }
got="$(run "$(img 7)" glidepath/releases/prod.yaml "$seven")"
[[ "$got" == false\|*"not a Kubernetes"* ]] || { echo "FAIL: a cloud release pin is never advisory: $got"; fail=1; }
got="$(run "$(img 7)" example-prod/staging/release.yaml "$seven" 1)"
[[ "$got" == false\|*"could not read"* ]] || { echo "FAIL: unreadable records mean not eligible: $got"; fail=1; }
got="$(run "$(img 7)" example-prod/staging/values.yaml "$seven")"
[[ "$got" == true\|* ]] || { echo "FAIL: values.yaml (pre-split) environments are covered too: $got"; fail=1; }

# CG-3: with the classify step not asked (no gate) the rule is the whole answer, and the gate's kind is left as classified.
expect_kind() { [[ "$(cat "${out_dir}/kind")|$(cat "${out_dir}/applicability")" == "$2" ]] || { echo "FAIL: $1: $(cat "${out_dir}/kind")|$(cat "${out_dir}/applicability")"; fail=1; }; }
run "$(img 3)" example-prod/staging/release.yaml "$seven" > /dev/null
expect_kind "no gate named: run, as before" "|run"
# glidepath-api did not answer (fallback): an eligible rollback found here gives the gate its rollback value.
got="$(SOURCE=fallback KIND=image-release run "$(img 3)" example-prod/staging/release.yaml "$seven")"
[[ "$got" == true\|* ]] || { echo "FAIL: fallback still runs the rule: $got"; fail=1; }
expect_kind "fallback + eligible: rollback, the gate's rollback value" "rollback|advisory"
SOURCE=fallback KIND=image-release run "$(img 2)" example-prod/staging/release.yaml "$seven" > /dev/null
expect_kind "fallback + not eligible: as classified" "image-release|run"
# glidepath-api answered: its kind is the answer, no record is read.
: > "${ARGSLOG}"
got="$(SOURCE=api KIND=config APPLICABILITY=n/a run "$(img 3)" "" "$seven")"
[[ "$got" == false\|* && ! -s "${ARGSLOG}" ]] || { echo "FAIL: n/a reads nothing and is not eligible: $got"; fail=1; }
expect_kind "n/a passes through" "config|n/a"
got="$(SOURCE=api KIND=rollback APPLICABILITY=advisory run "$(img 2)" example-prod/staging/release.yaml "$seven")"
[[ "$got" == true\|* && ! -s "${ARGSLOG}" ]] || { echo "FAIL: glidepath-api's rollback is eligible, records unread: $got"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
