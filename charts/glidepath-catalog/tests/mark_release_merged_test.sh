#!/usr/bin/env bash
# Runs mark-release-merged.yaml's real script against a stub kubectl and checks what it
# patches. Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/mark_release_merged_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
script="$(mktemp)"
helm template t charts/glidepath-catalog --show-only templates/tasks/mark-release-merged.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" > "$script"

stubdir="$(mktemp -d)"
cat > "${stubdir}/kubectl" <<'STUB'
#!/usr/bin/env bash
# list: print $RECORDS; get ... jsonpath state: print $STATE; patch: record the body
case "$1" in
  get)
    if [[ "${*: -2}" == "-o json" ]]; then printf '%s' "${RECORDS}"
    elif [[ "$*" == *"data.mergedAt"* ]]; then printf '%s' "${MERGED_AT:-}"
    else printf '%s' "${STATE:-}"; fi ;;
  patch) printf "%s %s\n" "$3" "$(jq -c . <<<"$9")" >> "${PATCHLOG}" ;;
esac
STUB
chmod +x "${stubdir}/kubectl"

run() { # merged state records -> prints the patch log
  PATCHLOG="$(mktemp)"; export PATCHLOG
  PATH="${stubdir}:${PATH}" REPO_URL="https://github.com/o/gitops-app.git" PR_NUMBER=7 MERGED="$1" STATE="$2" \
    RECORDS="$3" NAMESPACE=app-x-cicd bash "$script" >/dev/null 2>&1 || { echo "FAIL: script exited non-zero"; fail=1; }
  cat "${PATCHLOG}"
}
recs='{"items":[{"metadata":{"name":"release-tracking-a"},"data":{"prUrl":"https://github.com/o/gitops-app/pull/6"}},{"metadata":{"name":"release-tracking-b"},"data":{"prUrl":"https://github.com/o/gitops-app/pull/7"}}]}'

out="$(run true proposed "$recs")"
[[ "$out" == release-tracking-b*'"state":"merged"'* ]] || { echo "FAIL merged: $out"; fail=1; }
out="$(run true "" "$recs")"
[[ "$out" == release-tracking-b*'"state":"merged"'* ]] || { echo "FAIL merged from no state: $out"; fail=1; }
out="$(run false proposed "$recs")"
[[ "$out" == release-tracking-b*'"state":"closed"'* ]] || { echo "FAIL closed: $out"; fail=1; }
out="$(run true progressing "$recs")"
[[ "$out" == release-tracking-b*'"mergedAt"'* && "$out" != *'"state"'* ]] || { echo "FAIL: a release already progressing must get mergedAt only, never a state: $out"; fail=1; }
out="$(MERGED_AT=2026-10-06T00:00:00Z run true progressing "$recs")"
[[ -z "$out" ]] || { echo "FAIL: overwrote an existing mergedAt: $out"; fail=1; }
out="$(run false progressing "$recs")"
[[ -z "$out" ]] || { echo "FAIL: an unmerged close touched a release that already ran: $out"; fail=1; }
out="$(run true proposed '{"items":[{"metadata":{"name":"release-tracking-z"},"data":{"prUrl":"https://github.com/o/gitops-app/pull/99"}}]}')"
[[ -z "$out" ]] || { echo "FAIL: patched a record for a different PR: $out"; fail=1; }
out="$(run true proposed '{"items":[]}')"
[[ -z "$out" ]] || { echo "FAIL: patched with no records: $out"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
