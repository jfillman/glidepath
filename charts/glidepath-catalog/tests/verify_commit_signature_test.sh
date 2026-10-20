#!/usr/bin/env bash
# Runs verify-commit-signature.yaml's real script against recorded commits (fixtures/commits,
# see its README) with GitHub's real web-flow keyring (catalog/toolbox/github-web-flow.asc) and
# real gpgv. Stubbed: the GitHub API (curl), kubectl (the two ConfigMaps), the token helper,
# `git fetch` (loads a fixture object instead of the network) and gitsign (its Sigstore
# verification needs the network; a stub passes only the SHAs in GITSIGN_OK and records the
# identity regexp it was given).
# Needs helm, python3 (pyyaml), jq, git, gpg and gpgv.
# Usage: charts/glidepath-catalog/tests/verify_commit_signature_test.sh
set -u
cd "$(dirname "$0")/../../.."
root="$(pwd)"
fail=0
tmp="$(mktemp -d)"
fixtures="${root}/charts/glidepath-catalog/tests/fixtures/commits"

for f in "${fixtures}"/*.commit; do
  want="$(basename "${f}" .commit)"
  got="$(git hash-object -t commit "${f}")"
  [[ "${got}" == "${want}" ]] || { echo "FAIL fixture ${want}: hashes to ${got}"; fail=1; }
done

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/verify-commit-signature.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" \
  | sed "s#\$(results.signature-path.path)#${tmp}/out/path#; s#\$(results.signature-detail.path)#${tmp}/out/detail#; s#\$(results.findings-summary.path)#${tmp}/out/summary#" > "${tmp}/script.sh"

gpg --dearmor < catalog/toolbox/github-web-flow.asc > "${tmp}/web-flow.gpg"

mkdir -p "${tmp}/lib" "${tmp}/bin" "${tmp}/out"
echo 'github_app_installation_token() { echo faketoken; }' > "${tmp}/lib/github-app.sh"
echo ':' > "${tmp}/lib/retry.sh"

# curl: the last argument is the URL; answer from the case's API_* variables.
cat > "${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
url="${!#}"
case "${url}" in
  */repos/o/gitops-app/pulls/77/commits*) printf '%s' "${GITOPS_COMMITS}" ;;
  */repos/jfillman/app) printf '%s' '{"default_branch":"main"}' ;;
  */repos/jfillman/app/commits/*/pulls) printf '%s' "${API_PULLS}" ;;
  */repos/jfillman/app/pulls/171) printf '%s' "${API_PR}" ;;
  */repos/jfillman/app/compare/*) printf '%s' "${API_COMPARE}" ;;
  */repos/jfillman/app/pulls/171/commits*page=1) printf '%s' "${API_PR_COMMITS}" ;;
  */repos/jfillman/app/pulls/171/commits*) printf '%s' '[]' ;;
  */repos/jfillman/app/pulls/171/reviews*) printf '%s' "${API_REVIEWS}" ;;
  *) echo "curl stub: unexpected URL ${url}" >&2; exit 22 ;;
esac
STUB
cat > "${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *"configmap release-signing-policy"*) printf '%s' "${POLICY}" ;;
  *"configmap app-policy-config"*) [[ -n "${APP_CONFIG}" ]] && printf '%s' "${APP_CONFIG}" || exit 1 ;;
  *) echo "kubectl stub: unexpected $*" >&2; exit 1 ;;
esac
STUB
# git fetch <url> <sha>: write the fixture object and FETCH_HEAD; anything else is real git.
cat > "${tmp}/bin/git" <<STUB
#!/usr/bin/env bash
if [[ "\$1" == "fetch" ]]; then
  sha="\${!#}"
  [[ -f "${fixtures}/\${sha}.commit" ]] || { echo "git stub: no fixture for \${sha}" >&2; exit 128; }
  "${REAL_GIT:=$(command -v git)}" hash-object -t commit -w "${fixtures}/\${sha}.commit" >/dev/null
  printf '%s\t\t%s\n' "\${sha}" "fixture" > "\$("${REAL_GIT}" rev-parse --git-dir)/FETCH_HEAD"
  exit 0
fi
exec "${REAL_GIT}" "\$@"
STUB
cat > "${tmp}/bin/gitsign" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${tmp}/gitsign-calls"
grep -qw -- "\${!#}" <<<"\${GITSIGN_OK:-}" && exit 0
echo "gitsign stub: \${!#} not signed by an allowed signer" >&2; exit 1
STUB
chmod +x "${tmp}/bin/"*

trailers() { jq -cn --arg c "$1" '[{commit: {message: ("Release: promote app\n\nX-App-Repo: https://github.com/jfillman/app\nX-App-Commit: " + $c)}}]'; }
pulls_for() { jq -cn --arg sha "$1" '[{number: 171, merged_at: "2026-10-20T00:00:00Z", merge_commit_sha: $sha, base: {ref: "main", repo: {full_name: "jfillman/app"}}}]'; }
pr_for() { jq -cn --arg sha "$1" --arg head "$2" --arg by "${3:-jfillman}" '{number: 171, merged: true, merge_commit_sha: $sha, merged_by: {login: $by}, head: {sha: $head}, commits: 1}'; }

M=7455b7654343f22298643c27d5d88c1695b043e2   # backstage merge of PR 171
H=4922bf52856b1b9df64d40ffcebc6a4911ff665a   # its head, unsigned
G=6872b86214c333ddfdaeadbd27b042f7b1b84af8   # gitsign-signed
S=4416b513b49a5170c26232f0197a3f7f1c907d31   # squash
OLD=3bb62d4724b324bb5d6f0395aca93c9b9834cab8 # 2023 squash, retired key
WEB=4b0bd2bda73c72fddcaad64300d02199e11c227d # web editor
UPD=f07b3ee7bc9c837235e270b3209af4b7732b53a1 # update-branch merge
ROGUE=1338a87a5ce7699c592b00e821bde5e6bfd3634f

SIGNERS='["crossfader@gmail.com","7268337+jfillman@users.noreply.github.com"]'
policy() { # enabled mode mergers-json approvals [signers-json]
  jq -cn --argjson en "$1" --arg mode "$2" --argjson m "$3" --argjson a "$4" --argjson s "${5:-${SIGNERS}}" \
    '{defaults: {allowedCommitSigners: $s, githubMerge: {enabled: false, mode: "merger", allowedMergers: [], requiredApprovals: 0}},
      apps: {app: {githubMerge: {enabled: $en, mode: $mode, allowedMergers: $m, requiredApprovals: $a}}}}'
}

# run <commit> -> "rc|path|last stderr line"; the case's other inputs come from the environment.
run() {
  rm -f "${tmp}/out/"* "${tmp}/gitsign-calls"
  local wd; wd="$(mktemp -d)"
  ( cd "${wd}" && TMPDIR="${wd}" PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x \
      REPO_URL="https://github.com/o/gitops-app.git" REVISION=deadbeef PR_NUMBER=77 \
      APP_NAMESPACE=app-app-cicd APP_NAME=app CATALOG_NAMESPACE=platform-catalog \
      WEB_FLOW_KEYRING="${tmp}/web-flow.gpg" GITOPS_COMMITS="$(trailers "$1")" \
      bash "${tmp}/script.sh" >"${tmp}/stdout" 2>"${tmp}/stderr" )
  local rc=$?
  printf '%s|%s|%s' "${rc}" "$(cat "${tmp}/out/path" 2>/dev/null)" "$(grep -E '^FAIL' "${tmp}/stderr" | tail -1)"
}
# check <name> <got> <want>: <want> is a glob (* matches anything).
check() { [[ "$2" == $3 ]] || { echo "FAIL $1: got '$2' want '$3'"; [[ -s "${tmp}/stderr" ]] && sed 's/^/    /' "${tmp}/stderr" | tail -5; fail=1; }; }

export API_PULLS API_PR API_COMPARE API_PR_COMMITS API_REVIEWS POLICY APP_CONFIG GITSIGN_OK
API_COMPARE='{"status":"ahead"}'; API_PR_COMMITS="[{\"sha\":\"${H}\"}]"; API_REVIEWS='[]'; APP_CONFIG=''; GITSIGN_OK=''

# --- the gitsign path, unchanged -------------------------------------------------------------
POLICY="$(policy false merger '[]' 0)"; GITSIGN_OK="${G}"
check "a gitsign-signed commit passes on the gitsign path" "$(run "${G}")" "0|gitsign|"
check "the gitsign detail names the verified signer" "$(jq -r '"\(.v) \(.path) \(.signer)"' "${tmp}/out/detail")" "1 gitsign 7268337+jfillman@users.noreply.github.com"
check "and the summary does too" "$(cat "${tmp}/out/summary")" "*Sigstore certificate: 7268337+jfillman@users.noreply.github.com)"
check "gitsign gets the escaped signer regexp" "$(cat "${tmp}/gitsign-calls")" "*crossfader@gmail\\\\.com|7268337\\\\+jfillman@users\\\\.noreply\\\\.github\\\\.com*"
GITSIGN_OK=''
check "a gitsign signature from a signer not on the list fails (gitsign's own verdict is final)" "$(run "${G}")" "1||"
POLICY="$(policy false merger '[]' 0 '[]')"
check "an empty signer list fails closed" "$(run "${G}")" "1||FAIL: the release-signing policy for app has no allowedCommitSigners entries*"
check "an unsigned commit fails" "$(run "${H}")" "1||FAIL: *is not signed at all"

# --- the GitHub-merge path ---------------------------------------------------------------------
API_PULLS="$(pulls_for "${M}")"; API_PR="$(pr_for "${M}" "${H}")"
POLICY="$(policy false merger '["jfillman"]' 0)"
check "a GitHub merge fails while the path is off (the default)" "$(run "${M}")" "1||FAIL: *the GitHub-merge path is off for app*"
POLICY="$(policy true merger '["JFillman"]' 0)"
check "backstage#171's merge passes in merger mode (login case-insensitive)" "$(run "${M}")" "0|github-merge|"
check "the summary names the PR, merger and key" "$(cat "${tmp}/out/summary")" "provenance: verified via GitHub merge of jfillman/app#171 by jfillman (merge commit, web-flow key B5690EEEBB952194)"
check "the detail is compact JSON" "$(jq -r '"\(.pr) \(.mergedBy) \(.key) \(.approvers | length)"' "${tmp}/out/detail")" "171 jfillman B5690EEEBB952194 0"
check "and carries contract v1 with the policy applied" "$(jq -r '"\(.v) \(.mode) \(.requiredApprovals) \(has("strictCommits"))"' "${tmp}/out/detail")" "1 merger 0 false"
check "the detail ends at its closing brace (no trailing newline in the result)" "$(tail -c1 "${tmp}/out/detail" | od -An -tx1 | tr -d ' \n')" "7d"
check "gitsign is never called in merger mode" "$(cat "${tmp}/gitsign-calls" 2>/dev/null)" ""
POLICY="$(policy true merger '[]' 0)"
check "an empty merger list fails closed" "$(run "${M}")" "1||FAIL: *no githubMerge.allowedMergers entries*"
POLICY="$(policy true merger '["someone-else"]' 0)"
check "a merger not on the list fails" "$(run "${M}")" "1||FAIL: jfillman/app#171 was merged by jfillman, not in allowed-github-mergers for app"

POLICY="$(policy true merger '["jfillman"]' 0)"
API_PR="$(pr_for "${M}" "0000000000000000000000000000000000000000")"
check "a merge commit whose second parent is not the PR head fails" "$(run "${M}")" "1||FAIL: *second parent 4922bf52856b is not jfillman/app#171's head 000000000000"
API_PR="$(pr_for "${M}" "${H}")"
API_COMPARE='{"status":"diverged"}'
check "a merge that is not on the default branch fails" "$(run "${M}")" "1||FAIL: *is not on main (compare status 'diverged')"
API_COMPARE='{"status":"ahead"}'
API_PULLS='[]'
check "a web-flow commit with no PR fails" "$(run "${M}")" "1||FAIL: *not the merge commit of any pull request merged into main*"
API_PULLS="$(jq -c '.[0].base.ref = "develop"' <<<"$(pulls_for "${M}")")"
check "a PR merged into another branch fails" "$(run "${M}")" "1||FAIL: *not the merge commit of any pull request merged into main*"
API_PULLS="$(jq -c '.[0].merge_commit_sha = "abc"' <<<"$(pulls_for "${M}")")"
check "a PR that merely contains the commit is not its merge" "$(run "${M}")" "1||FAIL: *not the merge commit of any pull request*"

# Real web-flow commits that are not a PR's own merge: the API answers truthfully (no PR).
API_PULLS='[]'
check "a web-editor commit (real, web-flow-signed) fails" "$(run "${WEB}")" "1||FAIL: *not the merge commit of any pull request*"
check "an update-branch merge (real, web-flow-signed) fails" "$(run "${UPD}")" "1||FAIL: *not the merge commit of any pull request*"
check "a commit signed by an unknown key posing as GitHub fails" "$(run "${ROGUE}")" "1||FAIL: *does not verify against GitHub's web-flow keys (ERRSIG 45AFD63612C7BADC)*"

# A tampered copy of the real merge: same signature, edited message.
tampered="$(sed 's/^Merge pull request #171/Merge pull request #999/' "${fixtures}/${M}.commit")"
T="$(printf '%s' "${tampered}" | git hash-object -t commit --stdin)"
printf '%s' "${tampered}" > "${fixtures}/${T}.commit"
API_PULLS="$(pulls_for "${T}")"; API_PR="$(pr_for "${T}" "${H}")"
check "an edited copy of a GitHub-signed merge fails the signature" "$(run "${T}")" "1||FAIL: *does not verify against GitHub's web-flow keys (BADSIG*"
rm -f "${fixtures}/${T}.commit"

API_PULLS="$(pulls_for "${S}")"; API_PR="$(pr_for "${S}" "1111111111111111111111111111111111111111")"
check "a squash merge (one parent) passes" "$(run "${S}")" "0|github-merge|"
API_PULLS="$(pulls_for "${OLD}")"; API_PR="$(pr_for "${OLD}" "1111111111111111111111111111111111111111")"
check "a 2023 merge signed by the retired key passes" "$(run "${OLD}")" "0|github-merge|"
check "and names the retired key" "$(jq -r .key "${tmp}/out/detail")" "4AEE18F83AFDEB23"

# --- strict mode -----------------------------------------------------------------------------
API_PULLS="$(pulls_for "${M}")"; API_PR="$(pr_for "${M}" "${H}")"
POLICY="$(policy true strict '["jfillman"]' 0)"
check "strict fails on PR #171's unsigned commit" "$(run "${M}")" "1||FAIL: strict mode: jfillman/app#171 commit 4922bf52856b is not gitsign-signed by an allowed signer"
GITSIGN_OK="${H}"
check "strict passes when every PR commit is gitsign-signed" "$(run "${M}")" "0|github-merge-strict|"
check "and records how many commits it verified" "$(jq -r '"\(.mode) \(.strictCommits)"' "${tmp}/out/detail")" "strict 1"
GITSIGN_OK=''
POLICY="$(policy true merger '["jfillman"]' 0)"; APP_CONFIG='{"data":{"github-merge-mode":"strict","github-merge-required-approvals":"0"}}'
check "cicd.yaml can tighten merger to strict" "$(run "${M}")" "1||FAIL: strict mode: *"
POLICY="$(policy true strict '["jfillman"]' 0)"; APP_CONFIG='{"data":{"github-merge-mode":"merger","github-merge-required-approvals":"0"}}'
check "cicd.yaml cannot loosen strict to merger" "$(run "${M}")" "1||FAIL: strict mode: *"
POLICY="$(policy false merger '["jfillman"]' 0)"; APP_CONFIG='{"data":{"github-merge-enabled":"true"}}'
check "cicd.yaml cannot turn the path on" "$(run "${M}")" "1||FAIL: *the GitHub-merge path is off for app*"
APP_CONFIG=''

# --- required approvals ------------------------------------------------------------------------
review() { jq -cn --arg u "$1" --arg s "$2" --arg c "$3" --arg t "$4" '{user: {login: $u}, state: $s, commit_id: $c, submitted_at: $t}'; }
POLICY="$(policy true merger '["jfillman","riley-roooo"]' 1)"
API_REVIEWS='[]'
check "one approval required, none given" "$(run "${M}")" "1||FAIL: jfillman/app#171 has 0 qualifying approval(s)*requires 1*"
API_REVIEWS="[$(review riley-roooo APPROVED "${H}" 2026-10-20T01:00:00Z)]"
check "an allowed reviewer's approval of the final head passes" "$(run "${M}")" "0|github-merge|"
check "and is recorded" "$(jq -r '.approvers | join(",")' "${tmp}/out/detail")" "riley-roooo"
API_REVIEWS="[$(review riley-roooo APPROVED 2222222222222222222222222222222222222222 2026-10-20T01:00:00Z)]"
check "an approval of an earlier push does not count" "$(run "${M}")" "1||FAIL: *0 qualifying*"
API_REVIEWS="[$(review jfillman APPROVED "${H}" 2026-10-20T01:00:00Z)]"
check "the merger's own approval does not count" "$(run "${M}")" "1||FAIL: *0 qualifying*"
API_REVIEWS="[$(review outsider APPROVED "${H}" 2026-10-20T01:00:00Z)]"
check "a reviewer not on the list does not count" "$(run "${M}")" "1||FAIL: *0 qualifying*"
API_REVIEWS="[$(review riley-roooo APPROVED "${H}" 2026-10-20T01:00:00Z),$(review riley-roooo CHANGES_REQUESTED "${H}" 2026-10-20T02:00:00Z)]"
check "a later request for changes withdraws the approval" "$(run "${M}")" "1||FAIL: *0 qualifying*"
API_REVIEWS="[$(review riley-roooo APPROVED "${H}" 2026-10-20T01:00:00Z),$(review riley-roooo COMMENTED "${H}" 2026-10-20T02:00:00Z)]"
check "a later comment leaves the approval standing" "$(run "${M}")" "0|github-merge|"
POLICY="$(policy true merger '["jfillman","riley-roooo"]' 0)"; APP_CONFIG='{"data":{"github-merge-mode":"","github-merge-required-approvals":"2"}}'
check "cicd.yaml can raise the approval count" "$(run "${M}")" "1||FAIL: *requires 2*"
APP_CONFIG=''

# --- policy plumbing -------------------------------------------------------------------------
POLICY=''
check "a missing policy fails closed" "$(run "${G}")" "1||FAIL: release-signing-policy in platform-catalog has no policy.json*"
POLICY="$(jq -cn --argjson s "${SIGNERS}" '{defaults: {allowedCommitSigners: $s, githubMerge: {enabled: true, allowedMergers: ["jfillman"]}}, apps: {app: {githubMerge: {enabled: false}}}}')"
check "an app's enabled: false overrides a default true" "$(run "${M}")" "1||FAIL: *the GitHub-merge path is off for app*"
POLICY="$(jq -cn '{defaults: {allowedCommitSigners: ["a@example.com"]}, apps: {app: {allowedCommitSigners: ["crossfader@gmail.com"]}}}')"; GITSIGN_OK="${G}"
run "${G}" >/dev/null
check "an app's signer list replaces the default list" "$(cat "${tmp}/gitsign-calls")" "*^(crossfader@gmail\\\\.com)\$*"
GITSIGN_OK=''
POLICY="$(policy true sloppy '["jfillman"]' 0)"
check "an unknown mode fails closed" "$(run "${M}")" "1||FAIL: release-signing policy for app: githubMerge.mode 'sloppy' is not merger or strict"

rm -rf "${tmp}"
[ $fail -eq 0 ] && echo ok || exit 1
