#!/usr/bin/env bash
# Runs resolve-merged-pr.yaml's real script against stubbed GitHub (curl) and token helper.
# Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/resolve_merged_pr_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
tmp="$(mktemp -d)"
helm template t charts/glidepath-catalog --show-only templates/tasks/resolve-merged-pr.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" \
  | sed "s#\$(results.pr-number.path)#${tmp}/pr#; s#\$(results.merged.path)#${tmp}/merged#; s#\$(results.merged-at.path)#${tmp}/mergedat#" > "${tmp}/script.sh"

mkdir -p "${tmp}/lib" "${tmp}/bin"
echo 'github_app_installation_token() { echo faketoken; }' > "${tmp}/lib/github-app.sh"
cat > "${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s' "${PRS}"
STUB
chmod +x "${tmp}/bin/curl"

run() { # revision pr-number merged prs-json -> "pr|merged"
  rm -f "${tmp}/pr" "${tmp}/merged" "${tmp}/mergedat"
  PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x REPO_URL="https://github.com/o/gitops-app.git" \
    REVISION="$1" PR_NUMBER="$2" MERGED="$3" PRS="$4" bash "${tmp}/script.sh" >/dev/null 2>&1 || { echo "FAIL: script exited non-zero"; fail=1; }
  printf '%s|%s' "$(cat "${tmp}/pr" 2>/dev/null)" "$(cat "${tmp}/merged" 2>/dev/null)"
}
check() { [[ "$2" == "$3" ]] || { echo "FAIL $1: got '$2' want '$3'"; fail=1; }; }

merged='[{"number":16,"merged_at":"2026-10-06T02:57:00Z","merge_commit_sha":"abc123"}]'
check "a merge commit resolves to its PR" "$(run abc123 "" "" "$merged")" "16|true"
run abc123 "" "" "$merged" >/dev/null
check "the PR's own merge time is passed on" "$(cat "${tmp}/mergedat")" "2026-10-06T02:57:00Z"
check "a commit the PR merely contains is not its merge" "$(run def456 "" "" "$merged")" "|"
check "an unmerged PR is not a merge" "$(run abc123 "" "" '[{"number":9,"merged_at":null,"merge_commit_sha":"abc123"}]')" "|"
check "a direct commit with no PR" "$(run abc123 "" "" '[]')" "|"
check "the first of several PRs that match" "$(run abc123 "" "" '[{"number":5,"merged_at":null,"merge_commit_sha":"abc123"},{"number":16,"merged_at":"x","merge_commit_sha":"abc123"}]')" "16|true"
check "a PR number passed in passes straight through" "$(run "" 7 true '[]')" "7|true"
check "a passed PR number that did not merge keeps merged as given" "$(run "" 7 false '[]')" "7|false"
check "neither a revision nor a PR number" "$(run "" "" "" '[]')" "|"

[ $fail -eq 0 ] && echo ok || exit 1
