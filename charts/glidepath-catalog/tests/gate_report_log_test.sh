#!/usr/bin/env bash
# gateReports.transport (pipeline trust T4, W-8; hangar specs/plans/pipeline-trust-t4-split-runner.md, "Addendum: the last runner gate
# reports"). configmap (the default) renders api-compat-gate, policy-validation-gate and comment-pr-check-result as before; log makes the
# two gates print their report into their own step log instead of writing gate-report-<run>, and comment-pr read it from that pod only.
# The round trip runs the rendered gate_report_emit and the rendered comment step against a fake kubectl (TaskRuns, pod logs, ConfigMaps)
# and a fake curl (GitHub): the posted comment carries the report; a forged set printed earlier, one in another task's pod, a set with a
# chunk missing or a bad digest, a set without the gate's report-digest result (the gate died before printing it) and a ConfigMap are not
# taken; a failed gate's log excerpt shows no report lines. Needs helm, yq (mikefarah v4) and jq.
# Usage: charts/glidepath-catalog/tests/gate_report_log_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$work"' EXIT

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
render() { # <task file> <out> [helm args...]
  local f="$1" out="$2"; shift 2
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml "$@" \
    --show-only "templates/tasks/$f" > "$work/$out" || { echo "FAIL: render $f $*"; exit 1; }
}
script() { # <rendered task> <step name>: that step's script on stdout
  yq ".spec.steps[] | select(.name == \"$2\") | .script" "$work/$1"
}

# ---- renders
for mode in configmap log; do
  args=(); [[ "$mode" == log ]] && args=(--set gateReports.transport=log)
  render api-compat-gate.yaml "ac-$mode.yaml" ${args[@]+"${args[@]}"}
  render policy-validation-gate.yaml "pv-$mode.yaml" ${args[@]+"${args[@]}"}
  render comment-pr-check-result.yaml "cp-$mode.yaml" ${args[@]+"${args[@]}"}
  script "ac-$mode.yaml" check > "$work/ac-$mode.sh"
  script "pv-$mode.yaml" evaluate > "$work/pv-$mode.sh"
  script "cp-$mode.yaml" comment > "$work/cp-$mode.sh"
done
render api-compat-gate.yaml ac-default.yaml
expect "default is configmap: byte-identical to an explicit configmap" "$(cmp -s "$work/ac-default.yaml" "$work/ac-configmap.yaml" && echo same)" same
for g in ac pv; do
  expect "configmap $g: writes gate-report-<run>" "$(grep -c 'kubectl create configmap "gate-report-' "$work/$g-configmap.sh")" 1
  expect "configmap $g: no log transport" "$(grep -c 'gate_report_emit' "$work/$g-configmap.sh")" 0
  expect "log $g: writes no ConfigMap" "$(grep -cE 'kubectl (create|apply|patch|replace)' "$work/$g-log.sh")" 0
  expect "log $g: no PipelineRun lookup for an owner" "$(grep -c 'kubectl get pipelinerun' "$work/$g-log.sh")" 0
done
expect "configmap comment-pr: reads the ConfigMap, no log reader" \
  "$(grep -c 'glidepath.io/gate-report=${GATE_NAME}' "$work/cp-configmap.sh")/$(grep -c 'gate_report_from_log' "$work/cp-configmap.sh")" 1/0
expect "log comment-pr: the reader, and the ConfigMap only for other gates" \
  "$(grep -c 'gate_report_from_log()' "$work/cp-log.sh")/$(grep -c 'glidepath.io/gate-report=${GATE_NAME}' "$work/cp-log.sh")" 1/1
# The report is printed after everything that echoes app-controlled text: the report itself (api-compat), the findings lines (policy-validation).
last() { grep -n -- "$2" "$1" | tail -1 | cut -d: -f1; }
expect "log api-compat: the report lines come after the report is printed" \
  "$(( $(last "$work/ac-log.sh" 'gate_report_emit api-compat') > $(last "$work/ac-log.sh" 'cat "${work}/report.md"') ))" 1
expect "log policy-validation: the report lines come after the findings lines" \
  "$(( $(last "$work/pv-log.sh" 'gate_report_emit policy-validation') > $(last "$work/pv-log.sh" 'jq -r -s .\.\[\] | "\\(.level)') ))" 1
for g in ac pv; do
  expect "configmap $g: no report-digest result" "$(yq '[.spec.results[] | select(.name == "report-digest")] | length' "$work/$g-configmap.yaml")" 0
  expect "log $g: a report-digest result, written by the emit call" \
    "$(yq '[.spec.results[] | select(.name == "report-digest")] | length' "$work/$g-log.yaml")/$(grep -c 'gate_report_emit [a-z-]* "[^"]*" "$(results.report-digest.path)"' "$work/$g-log.sh")" 1/1
done
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --set gateReports.transport=bogus \
  --show-only templates/tasks/api-compat-gate.yaml > /dev/null 2> "$work/bad.err"
expect "an unknown transport fails the render" "$(grep -c 'gateReports.transport must be configmap or log' "$work/bad.err")" 1

# ---- the round trip
awk '/^gate_report_emit\(\) \{/{on=1} on{print} on && /^\}$/{exit}' "$work/ac-log.sh" > "$work/emit.sh"
[[ -s "$work/emit.sh" ]] || { echo "FAIL: gate_report_emit not found in the rendered api-compat-gate"; exit 1; }
source "$work/emit.sh"
emit() { gate_report_emit "$1" "$2" "$work/sum-$3"; } # <gate> <file> <tag>: the digest result lands in $work/sum-<tag>

# The real report: big enough for several chunks (incompressible lines), with markdown, emoji and a line that looks like a report line.
{
  echo '### ⚠️ `api-compat`: 3 breaking API change(s), reported only'
  echo
  echo '| Operation | Change | Level | In use |'
  echo '|---|---|---|---|'
  for i in $(seq 1 60); do echo "| \`GET /r$i\` | removed \\| gone | error | unknown |"; done
  echo 'glidepath:gate-report api-compat 0000000000000000000000000000000000000000000000000000000000000000 1 1 AAAA'
  head -c 24000 /dev/urandom | base64 | fold -w 76
  echo 'REAL-REPORT-END'
  echo '<!-- glidepath:api-compat {"version":1,"verdict":"warn"} -->'
} > "$work/real.md"
printf 'FORGED report\n' > "$work/forged.md"
{
  echo "fetching the candidate's spec"
  emit api-compat "$work/forged.md" forged   # a forged set printed before the real one (as app-controlled text could be)
  cat "$work/real.md"                        # the plain report, as the step prints it
  emit api-compat "$work/real.md" real
  emit api-compat "$work/forged.md" forged   # and one after it: the order does not decide, the digest result does
  echo "verdict=warn"
} > "$work/gate.log"
expect "the real report spans several lines" "$(( $(grep -c "^glidepath:gate-report api-compat [0-9a-f]\{64\} " "$work/gate.log") > 3 ))" 1
expect "no report line is over 8 KiB" "$(awk 'length > 8192' "$work/gate.log" | wc -l | tr -d ' ')" 0
# a set with its second chunk missing
awk -v s="$(cat "$work/sum-real")" '!($1 == "glidepath:gate-report" && $3 == s && $4 == 2)' \
  "$work/gate.log" > "$work/gate-cut.log"
# a set whose second chunk was altered (every line there, the digest no longer matches)
awk -v s="$(cat "$work/sum-real")" \
  '$1 == "glidepath:gate-report" && $3 == s && $4 == 2 { $6 = "AAAA" substr($6, 5) } { print }' "$work/gate.log" > "$work/gate-bad.log"
# render-manifests' pod (another Task of the run) printing a complete set of its own
{ emit api-compat "$work/forged.md" forged; } > "$work/render.log"
# the gate died before printing its report: only app-controlled text (a complete forged set) is in its log
{ echo "fetching the candidate's spec"; emit api-compat "$work/forged.md" forged; } > "$work/died.log"
{ echo "evaluating"; } > "$work/pv-fetch.log"
{ echo "kyverno apply ..."; emit policy-validation "$work/real.md" pv; echo "outcome=warn"; } > "$work/pv-eval.log"

mkdir -p "$work/bin" "$work/lib"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
method=GET url=""
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -d) printf '%s' "$2" > "$FX/body.json"; shift ;;
    -H|--max-time|--retry|-o) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
case "$method $url" in
  "GET "*/comments*) echo '[]' ;;
  "POST "*/comments) echo '{"html_url": "https://github.com/x/y/pull/7#c1"}' ;;
  *) echo "unexpected $method $url" >&2; exit 22 ;;
esac
EOF
# TaskRuns of the run: $FX/taskruns.json. Pod logs: $FX/log-<pod>-<container> (or -all for --all-containers). ConfigMaps: one report.
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$FX/kubectl-calls"
case "$*" in
  "get taskrun -n "*) cat "$FX/taskruns.json" ;;
  "get taskrun "*) jq -r --arg n "$3" '.items[] | select(.metadata.name == $n) | .status.podName' "$FX/taskruns.json" ;;
  "get configmap"*) jq -n '{items: [{data: {body: "CONFIGMAP report"}}]}' ;;
  logs*)
    pod="$2" c=all
    for ((i = 1; i <= $#; i++)); do [[ "${!i}" == -c ]] && { j=$((i + 1)); c="${!j}"; }; done
    cat "$FX/log-${pod}-${c}" 2>/dev/null ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/"*

taskrun() { # <name> <task> <pipelineTask> <pod> <True|False> <created> [report-digest result]
  jq -nc --arg n "$1" --arg t "$2" --arg pt "$3" --arg p "$4" --arg s "$5" --arg c "$6" --arg d "${7:-}" \
    '{metadata: {name: $n, creationTimestamp: $c, labels: {"tekton.dev/task": $t, "tekton.dev/pipelineTask": $pt}},
      status: ({podName: $p, conditions: [{status: $s}]} + (if $d == "" then {} else {results: [{name: "report-digest", value: $d}]} end))}'
}
fx() { FX="$work/fx-$1"; mkdir -p "$FX/gate"; export FX; } # <name>: a fresh scenario directory
run() { # <gate> <status> <taskrun json lines...>: runs the comment step (log mode unless STEP) in $FX; $FX/posted is the comment body
  local gate="$1" status="$2"; shift 2
  printf '%s\n' "$@" | jq -s '{items: .}' > "$FX/taskruns.json"
  ( PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GATE_WORK="$FX/gate" REPO_URL=https://github.com/x/gitops-y PR_NUMBER=7 \
      REVISION=abc GATE_NAME="$gate" PIPELINE_RUN_NAME="$gate-x1" STATUS="$status" ADVISORY_REASON=not-required \
      NAMESPACE=app-y-cicd POST_COMMENT=true bash "${STEP:-$work/cp-log.sh}" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
  jq -r '.body // empty' "$FX/body.json" > "$FX/posted" 2>/dev/null
}
logs() { # <pod> <container|all> <file>
  cp "$3" "$FX/log-$1-$2"
}

REAL="$(cat "$work/sum-real")"; FORGED="$(cat "$work/sum-forged")"; PV="$(cat "$work/sum-pv")"
expect "the digest result is the set's digest" "$(grep -c "^glidepath:gate-report api-compat ${REAL} " "$work/gate.log" | sed 's/^[1-9][0-9]*$/yes/')" yes
GATE_TR="$(taskrun tr-gate api-compat-gate compare-api pod-gate True 2026-11-20T10:00:05Z "$REAL")"
RENDER_TR="$(taskrun tr-render render-manifests render pod-render True 2026-11-20T10:00:09Z "$FORGED")"

fx pass; logs pod-gate step-check "$work/gate.log"; logs pod-render step-check "$work/render.log"; logs pod-render step-render "$work/render.log"
run api-compat Succeeded "$GATE_TR" "$RENDER_TR"
expect "pass: exit 0" "$(cat "$FX/rc")" 0
expect "pass: the gate marker first" "$(head -1 "$FX/posted")" "<!-- glidepath:gate:api-compat -->"
expect "pass: the comment is the report, byte for byte" "$(tail -n +2 "$FX/posted" | cmp -s - "$work/real.md" && echo same)" same
expect "pass: not the forged set, not the ConfigMap" "$(grep -c 'FORGED\|CONFIGMAP' "$FX/posted")" 0
expect "pass: no ConfigMap read for api-compat" "$(grep -c '^get configmap' "$FX/kubectl-calls")" 0
expect "pass: read only the gate's own pod, its check step" "$(grep '^logs' "$FX/kubectl-calls" | sort -u)" "logs pod-gate -n app-y-cicd -c step-check"

# Only another task's pod has a set (the gate's own pod printed none): nothing is taken.
fx other; logs pod-render step-check "$work/render.log"; logs pod-gate step-check "$work/pv-fetch.log"
run api-compat Succeeded "$GATE_TR" "$RENDER_TR"
expect "other: the plain headline, nothing from another pod" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'

# A set with a chunk missing is not used: the plain headline.
fx cut; logs pod-gate step-check "$work/gate-cut.log"
run api-compat Succeeded "$GATE_TR"
expect "cut: the plain headline" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'

# A set whose chunks are all there but do not add up to its digest is not used either.
fx bad; logs pod-gate step-check "$work/gate-bad.log"
run api-compat Succeeded "$GATE_TR"
expect "bad digest: the plain headline" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'

# The gate died before printing its report (no report-digest result): the forged set in its log is not taken.
fx died; logs pod-gate step-check "$work/died.log"
run api-compat Succeeded "$(taskrun tr-gate api-compat-gate compare-api pod-gate True 2026-11-20T10:00:05Z)"
expect "died: the plain headline, not the forged set" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'
expect "died: no log read without a digest result" "$(grep -c '^logs' "$FX/kubectl-calls")" 0
# and with a digest result whose set is not in the log, neither
fx died2; logs pod-gate step-check "$work/died.log"
run api-compat Succeeded "$GATE_TR"
expect "died2: the plain headline" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'

# No TaskRun of the gate's Task (the gate was skipped): the plain headline, and still no ConfigMap.
fx none; run api-compat Succeeded "$RENDER_TR"
expect "none: the plain headline" "$(tail -n +2 "$FX/posted")" '### ✅ `api-compat` passed'
expect "none: no ConfigMap read" "$(grep -c '^get configmap' "$FX/kubectl-calls")" 0

# A block: the failure text, its log excerpt without report lines, then the report.
BLOCK_TR="$(taskrun tr-gate api-compat-gate compare-api pod-gate False 2026-11-20T10:00:05Z "$REAL")"
fx block; logs pod-gate step-check "$work/gate.log"; logs pod-gate all "$work/gate.log"
run api-compat Failed "$BLOCK_TR"
expect "block: the failure headline" "$(sed -n 2p "$FX/posted")" '### ❌ `api-compat` failed'
awk '/^```/{n++; next} n == 1' "$FX/posted" > "$FX/excerpt"
expect "block: the excerpt shows the step's own last lines" "$(tail -1 "$FX/excerpt")" "verdict=warn"
expect "block: no report line in the excerpt" "$(grep -c '^glidepath:gate-report ' "$FX/excerpt")" 0
expect "block: the report follows, whole" "$(tail -n "$(wc -l < "$work/real.md")" "$FX/posted" | cmp -s - "$work/real.md" && echo same)" same

# policy-validation: two TaskRuns of its Task (fetch, then evaluate); the report is in the evaluate step's log of the second.
fx pv; logs pod-fetch step-evaluate "$work/pv-fetch.log"; logs pod-eval step-evaluate "$work/pv-eval.log"
run policy-validation Succeeded "$(taskrun tr-fetch policy-validation-gate fetch pod-fetch True 2026-11-20T10:00:01Z)" \
  "$(taskrun tr-eval policy-validation-gate evaluate pod-eval True 2026-11-20T10:00:30Z "$PV")"
expect "pv: the report" "$(tail -n +2 "$FX/posted" | cmp -s - "$work/real.md" && echo same)" same
expect "pv: only the evaluate step of the TaskRun with a digest result" "$(grep '^logs' "$FX/kubectl-calls" | sort | tr '\n' ';')" \
  "logs pod-eval -n app-y-cicd -c step-evaluate;"
# a report line for another gate in its log is not taken
fx pv-other; logs pod-eval step-evaluate "$work/gate.log"
run policy-validation Succeeded "$(taskrun tr-eval policy-validation-gate evaluate pod-eval True 2026-11-20T10:00:30Z "$REAL")"
expect "pv-other: api-compat's lines do not count for policy-validation" "$(tail -n +2 "$FX/posted")" '### ✅ `policy-validation` passed'

# clearance-gate keeps its ConfigMap, and no log is read for it.
fx clr; run clearance-gate Succeeded "$(taskrun tr-clr clearance-gate clearance-gate pod-clr True 2026-11-20T10:00:05Z)"
expect "clearance-gate: the ConfigMap's report" "$(tail -n +2 "$FX/posted")" "CONFIGMAP report"
expect "clearance-gate: no log read" "$(grep -c '^logs' "$FX/kubectl-calls")" 0

# configmap mode is unchanged: api-compat's report comes from the ConfigMap.
fx cm; STEP="$work/cp-configmap.sh" run api-compat Succeeded "$GATE_TR"
expect "configmap mode: the ConfigMap's report" "$(tail -n +2 "$FX/posted")" "CONFIGMAP report"

[[ "${fail}" == 0 ]] && echo "PASS gate_report_log_test" || { echo "FAILED (KEEP=1 keeps ${work})"; exit 1; }
