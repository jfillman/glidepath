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

# release-log-emit: its real emit step, against stub curl, kubectl and platform libs. The record it sends carries
# clearance_id and clearance_tree when the release record has them, and neither when it does not.
step release-log-emit emit > "$d/emit.sh"
mkdir -p "$d/lib" "$d/ebin"
cat > "$d/lib/otel.sh" <<'LIB'
otel_log_send() { printf '%s' "$1" > "${SENT}"; }
LIB
echo 'github_app_installation_token() { echo tok; }' > "$d/lib/github-app.sh"
cat > "$d/ebin/curl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  */check-runs*) echo '{"check_runs":[]}' ;;
  */reviews*) echo '[]' ;;
  */comments*) echo '[]' ;;
  *) echo '{"head":{"sha":"abc"},"merged":true,"merged_by":{"login":"dana"},"title":"release"}' ;;
esac
STUB
cat > "$d/ebin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *clearanceId*) printf '%s' "${CM_ID:-}" ;;
  *clearanceTree*) printf '%s' "${CM_TREE:-}" ;;
esac
STUB
chmod +x "$d/ebin/"*
emit() { # id tree -> the record the step sent
  SENT="$d/sent.$RANDOM"; export SENT
  PATH="$d/ebin:${PATH}" PLATFORM_LIB="$d/lib" CM_ID="$1" CM_TREE="$2" APP_NAMESPACE=app-shop-cicd APP_NAME=shop ENV=prod \
    CLUSTER=hangar-prod STATUS=Succeeded GIT_URL=u GIT_REVISION=r PR_URL=https://github.com/o/gitops-shop/pull/7 CHAIN_ID=c7 \
    FLOW_START_TIME= PR_CREATED_AT= FINISHED_AT= OTEL_EXPORTER_OTLP_ENDPOINT=http://otel GITHUB_TOKEN_BROKER_URL=http://b \
    bash "$d/emit.sh" >"$d/emit.out" 2>&1 || { echo "FAIL: emit exited non-zero"; cat "$d/emit.out"; fail=1; }
  cat "${SENT}" 2>/dev/null
}
rec="$(emit chg-77 sha256:feed)"
[[ "$(jq -r '.clearance_id' <<<"$rec")" == chg-77 && "$(jq -r '.clearance_tree' <<<"$rec")" == sha256:feed && "$(jq -r '.pr_number' <<<"$rec")" == 7 ]] \
  || { echo "FAIL: the release log record must carry the clearance join: $rec"; fail=1; }
rec="$(emit "" "")"
[[ -n "$rec" && "$(jq 'has("clearance_id") or has("clearance_tree")' <<<"$rec")" == false ]] \
  || { echo "FAIL: a release the gate never saw has no clearance keys: $rec"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
