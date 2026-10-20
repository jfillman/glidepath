#!/usr/bin/env bash
# Runs comment-pr-check-result.yaml's real script with stubbed kubectl (the PipelineRun's TaskRuns), curl (GitHub)
# and token helper, and checks the provenance gate's <!-- glidepath:signature --> line (provenance phase 3,
# docs/admin/commit-signing.md, Evidence): written on a pass from the verify TaskRun's signature-detail v1 result,
# never on a failure, without a result (a cloud pin PR), for another gate, or for a result that isn't v1.
# Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/comment_signature_marker_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
tmp="$(mktemp -d)"
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/comment-pr-check-result.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" > "${tmp}/script.sh"

mkdir -p "${tmp}/lib" "${tmp}/bin"
echo 'github_app_installation_token() { echo faketoken; }' > "${tmp}/lib/github-app.sh"
cat > "${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
printf '%s' "${TASKRUNS}"
STUB
# A POST/PATCH records the comment body it sends; every GET (existing comments, duplicates) finds none.
cat > "${tmp}/bin/curl" <<STUB
#!/usr/bin/env bash
data=""; write=false; prev=""
for a in "\$@"; do [[ "\${prev}" == "-d" ]] && data="\${a}"; [[ "\${a}" == "POST" || "\${a}" == "PATCH" ]] && write=true; prev="\${a}"; done
if \${write}; then printf '%s' "\${data}" | jq -r .body > "${tmp}/posted"; echo '{"id":1,"html_url":"https://example.invalid/c/1"}'; else echo '[]'; fi
STUB
chmod +x "${tmp}/bin/"*

taskruns() { # status signature-detail-or-empty
  jq -cn --arg st "$1" --arg sig "$2" '{items: [
    {metadata: {name: "v", labels: {"tekton.dev/pipelineTask": "verify"}}, status: {conditions: [{status: $st}],
     results: (if $sig == "" then [] else [{name: "signature-detail", value: $sig}, {name: "findings-summary", value: "provenance: verified via GitHub merge of o/app#7 by alice"}] end)}}]}'
}
run() { # gate status taskruns -> the posted body
  rm -f "${tmp}/posted"
  PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x REPO_URL="https://github.com/o/gitops-app.git" \
    PR_NUMBER=12 REVISION=abc GATE_NAME="$1" PIPELINE_RUN_NAME=pr-1 STATUS="$2" ADVISORY_REASON="" NAMESPACE=ns TASKRUNS="$3" \
    bash "${tmp}/script.sh" >/dev/null 2>"${tmp}/err" || { echo "FAIL: script exited non-zero: $(tail -2 "${tmp}/err")"; fail=1; }
  cat "${tmp}/posted" 2>/dev/null
}
check() { [[ "$2" == $3 ]] || { echo "FAIL $1: got '$2' want '$3'"; fail=1; }; }

SIG='{"v":1,"path":"github-merge","repo":"o/app","commit":"abc","pr":7,"mergedBy":"alice","approvers":[],"key":"B5690EEEBB952194","mode":"merger","requiredApprovals":0}'
body="$(run provenance Succeeded "$(taskruns True "${SIG}")")"
check "a pass carries the marker as its last line" "$(tail -1 <<<"${body}")" "<!-- glidepath:signature ${SIG} -->"
check "and the visible summary above it" "${body}" "*provenance: verified via GitHub merge of o/app#7 by alice*"
check "a failed run carries no marker" "$(run provenance Failed "$(taskruns False "${SIG}")" | grep -c 'glidepath:signature')" "0"
check "a pass with no result (a cloud pin PR) carries none" "$(run provenance Succeeded "$(taskruns True "")" | grep -c 'glidepath:signature')" "0"
check "another gate never carries it" "$(run sbom Succeeded "$(taskruns True "${SIG}")" | grep -c 'glidepath:signature')" "0"
check "a result that is not v1 is left out" "$(run provenance Succeeded "$(taskruns True '{"v":2,"path":"gitsign"}')" | grep -c 'glidepath:signature')" "0"
check "a result with -- in it is left out" "$(run provenance Succeeded "$(taskruns True '{"v":1,"path":"gitsign","signer":"a--b@c.d"}')" | grep -c 'glidepath:signature')" "0"

rm -rf "${tmp}"
[ $fail -eq 0 ] && echo ok || exit 1
