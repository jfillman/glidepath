#!/usr/bin/env bash
# Runs comment-pr-check-result.yaml's real script with stubbed kubectl (TaskRuns, the failed step's log, a gate
# report ConfigMap), curl (GitHub) and token helper, and checks the failure comment's log excerpt (2026-11-17,
# gitops-backstage#317: the clearance-gate comment began "d this release of ..." and repeated its own report):
#   - a gate that left a full report: the comment carries the report and no log excerpt;
#   - no report: the excerpt is whole lines (never starts mid-word), Tekton's entrypoint lines are dropped,
#     and it stays within 1200 bytes.
# Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/comment_failure_excerpt_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
tmp="$(mktemp -d)"
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/comment-pr-check-result.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" > "${tmp}/script.sh"
# the same task with gateReports.transport=log (W-8): its excerpt also drops the gate's report lines
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --set gateReports.transport=log --show-only templates/tasks/comment-pr-check-result.yaml \
  | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" > "${tmp}/script-log.sh"

mkdir -p "${tmp}/lib" "${tmp}/bin"
echo 'github_app_installation_token() { echo faketoken; }' > "${tmp}/lib/github-app.sh"
cat > "${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# configmap -> the gate report (REPORT, may be empty); logs -> LOGS_FILE; a taskrun's podName -> pod-1; else TASKRUNS.
if [[ "$*" == *configmap* ]]; then jq -cn --arg b "${REPORT:-}" 'if $b == "" then {items: []} else {items: [{data: {body: $b}}]} end'
elif [[ "$1" == "logs" ]]; then cat "${LOGS_FILE}"
elif [[ "$*" == *podName* ]]; then echo pod-1
else printf '%s' "${TASKRUNS}"; fi
STUB
cat > "${tmp}/bin/curl" <<STUB
#!/usr/bin/env bash
data=""; write=false; prev=""
for a in "\$@"; do [[ "\${prev}" == "-d" ]] && data="\${a}"; [[ "\${a}" == "POST" || "\${a}" == "PATCH" ]] && write=true; prev="\${a}"; done
if \${write}; then printf '%s' "\${data}" | jq -r .body > "${tmp}/posted"; echo '{"id":1,"html_url":"https://example.invalid/c/1"}'; else echo '[]'; fi
STUB
chmod +x "${tmp}/bin/"*

taskruns='{"items":[{"metadata":{"name":"g","labels":{"tekton.dev/pipelineTask":"register"}},"status":{"conditions":[{"status":"False"}],"results":[]}}]}'
run() { # gate report -> the posted body in ${body}; called directly (not in $(...)) so a failure reaches ${fail}
  rm -f "${tmp}/posted"
  PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x REPO_URL="https://github.com/o/gitops-app.git" \
    PR_NUMBER=12 REVISION=abc GATE_NAME="$1" PIPELINE_RUN_NAME=pr-1 STATUS=Failed ADVISORY_REASON="" NAMESPACE=ns \
    TASKRUNS="${taskruns}" REPORT="$2" LOGS_FILE="${tmp}/log" \
    bash "${tmp}/${SCRIPT:-script.sh}" >/dev/null 2>"${tmp}/err" || { echo "FAIL: script exited non-zero: $(tail -2 "${tmp}/err")"; fail=1; }
  body="$(cat "${tmp}/posted" 2>/dev/null)"
  [[ -n "${body}" ]] || { echo "FAIL: no comment was posted for ${1}"; fail=1; }
}
excerpt() { awk '/^\*\*What happened:\*\*/{f=1; next} f && /^```$/ {if (s) exit; s=1; next} s' ; }
check() { # name condition-result
  if [[ "$2" == ok ]]; then echo "ok: $1"; else echo "FAIL: $1"; fail=1; fi
}

# A long log: 30 lines of 60 characters plus Tekton's entrypoint line at the end.
: > "${tmp}/log"
for i in $(seq -w 1 30); do printf 'line %s the quick brown fox jumps over the lazy dog again %s\n' "$i" "$i" >> "${tmp}/log"; done
echo '2026/11/17 20:25:30 Entrypoint initialization' >> "${tmp}/log"

# 1. A gate that left a full report: report shown, no excerpt.
run clearance-gate '### ⏳ `clearance-gate`: 1 environment(s) holding for people'
check "report replaces the excerpt" "$([[ "${body}" == *'holding for people'* && "${body}" != *'What happened'* && "${body}" != *'line 30'* ]] && echo ok)"
check "report keeps the recommendation and retry" "$([[ "${body}" == *'**Recommendation:**'* && "${body}" == *'/retest clearance-gate-'* ]] && echo ok)"

# 2. No report: the excerpt is whole lines, entrypoint line dropped, within 1200 bytes.
run sast ''
ex="$(printf '%s\n' "${body}" | excerpt)"
check "excerpt present" "$([[ -n "${ex}" ]] && echo ok)"
check "excerpt starts at a line start" "$([[ "$(printf '%s\n' "${ex}" | head -1)" == line\ * ]] && echo ok)"
check "excerpt ends with the last real log line" "$([[ "$(printf '%s\n' "${ex}" | tail -1)" == *'again 30' ]] && echo ok)"
check "entrypoint line dropped" "$([[ "${ex}" != *Entrypoint* ]] && echo ok)"
check "excerpt within 1200 bytes" "$([[ $(printf '%s\n' "${ex}" | LC_ALL=C wc -c) -le 1200 ]] && echo ok)"

# 3. A single line longer than 1200 bytes is still shown (cut to its last 1200 bytes).
python3 -c "print('x' * 3000)" > "${tmp}/log"
run sast ''
ex="$(printf '%s\n' "${body}" | excerpt)"
check "one very long line is cut, not dropped" "$([[ ${#ex} -eq 1200 ]] && echo ok)"

# 4. transport log: a report line is not in the excerpt, a token is redacted before the cut, the other lines stay.
tok="ghp_$(printf 'A%.0s' $(seq 36))"
{ echo 'glidepath:gate-report 1/1 aGVsbG8='; echo "fatal: could not read ${tok}"; echo 'last real line'; } > "${tmp}/log"
SCRIPT=script-log.sh run sast ''
ex="$(printf '%s\n' "${body}" | excerpt)"
check "log transport: report line dropped from the excerpt" "$([[ "${ex}" != *gate-report* && "${ex}" == *'last real line' ]] && echo ok)"
check "log transport: token redacted" "$([[ "${ex}" != *"${tok}"* && "${ex}" == *redacted* ]] && echo ok)"
run sast ''
check "configmap transport: token redacted" "$([[ "${body}" != *"${tok}"* ]] && echo ok)"

rm -rf "${tmp}"
[[ ${fail} == 0 ]] && echo "comment_failure_excerpt_test: all passed" || exit 1
