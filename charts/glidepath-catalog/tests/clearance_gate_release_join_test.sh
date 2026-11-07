#!/usr/bin/env bash
# G-ARGO-6: runs clearance-gate.yaml's real register step and release-log-emit's join lookup against stub curl and
# kubectl, and checks that the change id and tree digest reach the release-tracking record and the release log body.
# Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/clearance_gate_release_join_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
step() { # <template> <step name> -> the step's script
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only "templates/tasks/$1.yaml" \
    | python3 -c "import sys,yaml; d=yaml.safe_load(sys.stdin.read()); print([s for s in d['spec']['steps'] if s['name']=='$2'][0]['script'])"
}
# Tekton's result paths and the /work volume are redirected into the temp dir (/work first: see policy_validation_test.sh).
step clearance-gate register \
  | sed -e "s|/work|$d/w|g" -e "s|\$(results.outcome.path)|$d/outcome|" -e "s|\$(results.findings-summary.path)|$d/summary|" \
        -e "s|\$(results.change-ids.path)|$d/ids|" -e "s|/var/run/secrets/clearance|$d/secret|" > "$d/register.sh"
mkdir -p "$d/w/eval/hangar-prod-prod" "$d/secret" "$d/bin"
echo tok > "$d/secret/clearance-token"
echo plan > "$d/w/plan.txt"
echo '{"cluster":"hangar-prod","env":"prod","treeDigest":"sha256:feed"}' > "$d/w/eval/hangar-prod-prod/change.json"

cat > "$d/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n200' '{"id":"chg-77","state":"cleared","tier":1,"tree_digest":"sha256:feed","reasons":[],"requirement":null,"approvals":[],"cleared_by":"policy","deny_reason":null,"expires_at":"2026-12-01T00:00:00Z"}'
STUB
cat > "$d/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  get) if [[ "$*" == *"-l "* ]]; then printf '%s' "${RECORDS}"; fi ;;
  patch) printf '%s %s\n' "$3" "$(jq -c . <<<"${*: -1}")" >> "${PATCHLOG}" ;;
esac
STUB
chmod +x "$d/bin/"*

run() { # records json -> the patch log
  PATCHLOG="$d/patch.$RANDOM"; : > "${PATCHLOG}"; export PATCHLOG
  PATH="$d/bin:${PATH}" RECORDS="$1" APP_NAME=shop CLEARANCE_URL=http://clearance TOWER_URL= PIPELINE_RUN= \
    REPO_URL=https://github.com/o/gitops-shop.git PR_NUMBER=7 NAMESPACE=app-shop-cicd bash "$d/register.sh" >"$d/out" 2>&1 \
    || { echo "FAIL: register exited non-zero"; cat "$d/out"; fail=1; }
  cat "${PATCHLOG}"
}
rec='{"items":[{"metadata":{"name":"release-tracking-other"},"data":{"prUrl":"https://github.com/o/gitops-shop/pull/6"}},{"metadata":{"name":"release-tracking-c7"},"data":{"prUrl":"https://github.com/o/gitops-shop/pull/7"}}]}'
out="$(run "$rec")"
[[ "$out" == release-tracking-c7*'"clearanceId":"chg-77"'*'"clearanceTree":"sha256:feed"'* ]] \
  || { echo "FAIL: the change id and tree digest must be patched onto this PR's record only: $out"; fail=1; }
[[ "$(wc -l <<<"$out" | tr -d ' ')" == 1 ]] || { echo "FAIL: exactly one record patched: $out"; fail=1; }
out="$(run '{"items":[{"metadata":{"name":"release-tracking-z"},"data":{"prUrl":"https://github.com/o/gitops-shop/pull/99"}}]}')"
[[ -z "$out" ]] || { echo "FAIL: patched a record of a different PR: $out"; fail=1; }
[[ "$(cat "$d/outcome")" == cleared ]] || { echo "FAIL: a missing record must not fail the check: $(cat "$d/outcome")"; fail=1; }

# release-log-emit: the record body gains clearance_id and clearance_tree only when the gate wrote them.
grep -q 'clearance_id: \$clearanceId, clearance_tree: \$clearanceTree' charts/glidepath-catalog/templates/tasks/release-log-emit.yaml \
  || { echo "FAIL: release-log-emit does not carry clearance_id"; fail=1; }
body="$(jq -nc --arg clearanceId chg-77 --arg clearanceTree sha256:feed '{a:1} + (if $clearanceId != "" then {clearance_id: $clearanceId, clearance_tree: $clearanceTree} else {} end)')"
[[ "$body" == *'"clearance_id":"chg-77"'* ]] || { echo "FAIL: join expression: $body"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
