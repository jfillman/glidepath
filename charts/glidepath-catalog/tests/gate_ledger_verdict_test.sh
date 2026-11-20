#!/usr/bin/env bash
# Pipeline trust T4 step 3, ledger option (a): the gate ledger is written by the record-gate-verdict FINALLY task of every gate
# Pipeline (as release-record-writer), which works out the gate's verdict itself. Checks
#   - the wiring: every Pipeline that has comment-pr has record-gate-verdict beside it in finally, with the same gate, repository,
#     revision, status and `when`, no runAfter and no result of another finally task (Tekton refuses both), and comment-pr itself
#     no longer patches a ConfigMap;
#   - the verdict mapping: for the same inputs the `derive` step and comment-pr-check-result's `comment` step (the code that decides
#     what the gate records and what the PR comment says) reach the same verdict. Succeeded and Completed pass; Failed fails, unless
#     only an -advisory TaskRun failed (ADR-0021); anything else (None, empty) fails. The values are success and failure, as in the
#     ledger since ADR-0029 (no "skipped": a skipped task leaves $(tasks.status) Succeeded);
#   - the tasks-timeout rule, with the numbers of promotion_fan_in_test.sh (in time, timed out, within a minute: nothing recorded);
#   - the pull request: the pipelinesascode.tekton.dev/pull-request label first, the numeric pr-number param when the label is
#     missing, nothing when neither is a number (a /retest's literal template text, no label) and for a repository that is not a gitops-<app>.
# Needs helm, yq (mikefarah v4), python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/gate_ledger_verdict_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
VALUES=charts/glidepath-catalog/tests/values.example.yaml

# ---- wiring
helm template t charts/glidepath-catalog -f "$VALUES" --set promotion.enabled=true --set promotion.pinDigest=true --set registries.flight.namespace=riley-roooo --show-only 'templates/pipelines/*.yaml' > "$work/pipelines.yaml" || exit 1
python3 - "$work/pipelines.yaml" > "$work/wiring.out" <<'PY'
import sys, yaml
def ref(t):
    return [p["value"] for p in t.get("taskRef", {}).get("params", []) if p["name"] == "name"][0]
def params(t):
    return {p["name"]: p["value"] for p in t["params"]}
n = 0
for d in yaml.safe_load_all(open(sys.argv[1])):
    if not d or d.get("kind") != "Pipeline":
        continue
    fin = d["spec"].get("finally", [])
    by = {ref(t): t for t in fin}
    name = d["metadata"]["name"]
    if "comment-pr-check-result" not in by:
        if "record-gate-verdict" in by:
            print("FAIL %s has no comment-pr and must not have record-gate-verdict either" % name)
        continue
    n += 1
    c, r = by["comment-pr-check-result"], by.get("record-gate-verdict")
    if r is None:
        print("FAIL %s: no record-gate-verdict in finally" % name); continue
    ok = True
    for k in ("repo-url", "revision", "pr-number", "gate-name", "pipeline-run-name", "status"):
        if params(r).get(k) != params(c).get(k):
            print("FAIL %s: %s differs: %r vs %r" % (name, k, params(r).get(k), params(c).get(k))); ok = False
    if r.get("when") != c.get("when"):
        print("FAIL %s: when differs" % name); ok = False
    if "runAfter" in r or "runAfter" in c:
        print("FAIL %s: a finally task with runAfter" % name); ok = False
    for t in fin:
        for v in params(t).values():
            if any("$(tasks.%s." % f["name"] in v for f in fin):
                print("FAIL %s: %s reads the result of a finally task: %s" % (name, t["name"], v)); ok = False
    if len([t for t in fin if ref(t) == "record-gate-verdict"]) != 1:
        print("FAIL %s: record-gate-verdict is not exactly once" % name); ok = False
    if ok:
        print("ok   %s: record-gate-verdict wired like comment-pr" % name)
print("pipelines-with-comment-pr %d" % n)
PY
cat "$work/wiring.out" | grep -v '^pipelines-with' | grep FAIL && fail=1
grep -v FAIL "$work/wiring.out" | grep -v '^pipelines-with' | tail -1
expect "wiring: all 15 gate Pipelines carry it" "$(grep '^pipelines-with' "$work/wiring.out")" "pipelines-with-comment-pr 15"
helm template t charts/glidepath-catalog -f "$VALUES" --set promotion.enabled=true --set promotion.pinDigest=true --set registries.flight.namespace=riley-roooo --show-only templates/tasks/comment-pr-check-result.yaml > "$work/comment.yaml"
expect "comment-pr no longer patches a ConfigMap" "$(yq '[.spec.steps[].script] | join("\n")' "$work/comment.yaml" | grep -c 'kubectl patch')" 0
helm template t charts/glidepath-catalog -f "$VALUES" --show-only templates/tasks/record-gate-verdict.yaml > "$work/record.yaml"
yq '.spec.steps[] | select(.name == "derive") | .script' "$work/record.yaml" > "$work/derive.sh"
yq '.spec.steps[] | select(.name == "comment") | .script' "$work/comment.yaml" > "$work/comment.sh"
[[ -s "$work/derive.sh" && -s "$work/comment.sh" ]] || { echo "FAIL: steps not rendered"; exit 1; }
expect "derive uses no token, no GitHub API and no Secret" "$(grep -c 'curl\|Authorization\|/var/run/secrets\|github-app\|get secret' "$work/derive.sh")" 0

# ---- fakes: kubectl (the run, its TaskRuns), the github-app lib, curl (never called with post-comment=false)
mkdir -p "$work/bin" "$work/lib"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "get pipelinerun"*) [[ -f "$FX/pipelinerun.json" ]] && cat "$FX/pipelinerun.json" || { echo "not found" >&2; exit 1; } ;;
  "get taskrun -n"*) cat "$FX/taskruns.json" ;;
  "get taskrun "*) echo pod-x ;;
  "logs "*) exit 0 ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
STUB
printf '#!/usr/bin/env bash\necho "unexpected curl $*" >&2; exit 22\n' > "$work/bin/curl"
printf '#!/usr/bin/env bash\nexit 0\n' > "$work/bin/sleep"
chmod +x "$work/bin/"*

taskruns() { # <hard|hardfail|advfail|advfail+hardfail|none>: the run's TaskRuns
  case "$1" in
    hard) echo '{"items":[{"metadata":{"name":"a","labels":{"tekton.dev/pipelineTask":"extract-promoted-image"}},"status":{"conditions":[{"status":"True"}]}}]}' ;;
    hardfail) echo '{"items":[{"metadata":{"name":"a","labels":{"tekton.dev/pipelineTask":"extract-promoted-image"}},"status":{"conditions":[{"status":"False"}]}}]}' ;;
    advfail) echo '{"items":[{"metadata":{"name":"a","labels":{"tekton.dev/pipelineTask":"extract-promoted-image"}},"status":{"conditions":[{"status":"True"}]}},{"metadata":{"name":"b","labels":{"tekton.dev/pipelineTask":"verify-sast-attestation-advisory"}},"status":{"conditions":[{"status":"False"}]}}]}' ;;
    advfail+hardfail) echo '{"items":[{"metadata":{"name":"a","labels":{"tekton.dev/pipelineTask":"extract-promoted-image"}},"status":{"conditions":[{"status":"False"}]}},{"metadata":{"name":"b","labels":{"tekton.dev/pipelineTask":"verify-sast-attestation-advisory"}},"status":{"conditions":[{"status":"False"}]}}]}' ;;
    none) echo '{"items":[]}' ;;
  esac
}
derive() { # <name> <status> <taskruns kind> [env...]: runs the derive step; the files it wrote are in $FX/d
  FX="$work/$1"; mkdir -p "$FX/d" "$FX/gate"; export FX; taskruns "$3" > "$FX/taskruns.json"
  local status="$2"; shift 3
  ( cd "$FX" && env PATH="$work/bin:$PATH" DERIVED="$FX/d" REPO_URL="${REPO_URL:-https://github.com/o/gitops-app}" REVISION=0123456789abcdef0123456789abcdef01234567 \
      PR_NUMBER="${PR_NUMBER:-7}" GATE_NAME=sast PIPELINE_RUN_NAME=sast-x1 STATUS="${status}" NAMESPACE=app-x-cicd "$@" bash "$work/derive.sh" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}
comment() { # <name> <status> <taskruns kind>: runs comment-pr's comment step (post-comment false); gate/verdict is what it decided
  FX="$work/$1"; mkdir -p "$FX/gate"; export FX; taskruns "$3" > "$FX/taskruns.json"
  ( cd "$FX" && env PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GATE_WORK="$FX/gate" REPO_URL=https://github.com/o/gitops-app PR_NUMBER=7 \
      REVISION=0123456789abcdef0123456789abcdef01234567 GATE_NAME=sast PIPELINE_RUN_NAME=sast-x1 STATUS="$2" ADVISORY_REASON=rollback \
      NAMESPACE=app-x-cicd POST_COMMENT=false bash "$work/comment.sh" ) > "$FX/out-comment" 2>&1
}

# ---- the verdict mapping equals comment-pr-check-result's
n=0
while IFS='|' read -r status kind want; do
  n=$((n + 1))
  derive "map$n" "$status" "$kind"; comment "map$n" "$status" "$kind"
  got_derive="$(cat "$work/map$n/d/verdict")"; got_comment="$(cat "$work/map$n/gate/verdict" 2>/dev/null)"
  expect "verdict $status/$kind: record-gate-verdict = comment-pr" "${got_derive}" "${got_comment}"
  expect "verdict $status/$kind: is $want" "${got_derive}" "${want}"
done <<'CASES'
Succeeded|hard|success
Completed|hard|success
Succeeded|advfail|success
Failed|hardfail|failure
Failed|advfail|success
Failed|advfail+hardfail|failure
Failed|none|failure
None|hard|failure
|hard|failure
CASES
expect "verdict table is not vacuous" "$n" 9

# ---- the tasks timeout (same numbers as promotion_fan_in_test.sh: timeouts.tasks 8m)
TIMEOUTS='{"tasks":"8m0s","finally":"4m","pipeline":"12m"}'
started() { # <name> <seconds ago> [timeouts json]: this gate's PipelineRun (with the PaC label) as $work/<name>/pipelinerun.json
  mkdir -p "$work/$1"
  jq -n --arg s "$(date -u -r $(( $(date -u +%s) - $2 )) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$(( $(date -u +%s) - $2 ))" +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson t "${3:-$TIMEOUTS}" \
    '{metadata: {labels: {"pipelinesascode.tekton.dev/pull-request": "7"}}, spec: {timeouts: $t}, status: {startTime: $s}}' > "$work/$1/pipelinerun.json"
}
derive_with_run() { # <name> <seconds ago> <status> <taskruns> [timeouts]: derive against that run (derive keeps the files of its own dir)
  started "$1" "$2" "${5:-}"; cp "$work/$1/pipelinerun.json" "$work/$1.keep"; derive "$1" "$3" "$4"
}
derive_with_run t-in-time 300 Succeeded hard;          expect "timeout in-time: success" "$(cat "$work/t-in-time/d/verdict")" success
derive_with_run t-timed-out 500 Succeeded hard;        expect "timeout timed-out: failure" "$(cat "$work/t-timed-out/d/verdict")" failure
expect "timeout timed-out: says why" "$(grep -c 'past its tasks timeout' "$work/t-timed-out/out")" 1
derive_with_run t-near 450 Succeeded hard;             expect "timeout near: nothing recorded" "$(cat "$work/t-near/d/verdict")" ""
expect "timeout near: exit 0" "$(cat "$work/t-near/rc")" 0
derive_with_run t-pipe 500 Succeeded hard '{"pipeline":"1h0m0s"}';       expect "timeout pipeline-only: success" "$(cat "$work/t-pipe/d/verdict")" success
derive_with_run t-pipe-late 3700 Succeeded hard '{"pipeline":"1h0m0s"}'; expect "timeout pipeline-only-late: failure" "$(cat "$work/t-pipe-late/d/verdict")" failure
# a red verdict is never re-judged by the clock, and an unreadable run is not judged either
derive_with_run t-late-red 500 Failed hardfail
expect "timeout late-red: stays failure" "$(cat "$work/t-late-red/d/verdict")" failure
derive t-no-run Succeeded hard; expect "no readable run: success stands" "$(cat "$work/t-no-run/d/verdict")" success

# ---- the pull request
sha=0123456789abcdef0123456789abcdef01234567
labelled() { jq -n --arg n "$1" '{metadata: {labels: {"pipelinesascode.tekton.dev/pull-request": $n}}}'; }
mk() { mkdir -p "$work/$1"; echo "$2" > "$work/$1/pipelinerun.json"; }
mk pr-label "$(labelled 53)"; derive pr-label Succeeded hard PR_NUMBER=7
expect "pr: the label wins over the param" "$(cat "$work/pr-label/d/pr-url")" "https://github.com/o/gitops-app/pull/53"
expect "pr: head sha is the revision" "$(cat "$work/pr-label/d/head-sha")" "$sha"
mk pr-param '{"metadata":{"labels":{}}}'; derive pr-param Succeeded hard
expect "pr: no label, numeric param" "$(cat "$work/pr-param/d/pr-url")" "https://github.com/o/gitops-app/pull/7"
mk pr-retest '{"metadata":{"labels":{}}}'; derive pr-retest Succeeded hard PR_NUMBER='{{ body.pull_request.number }}'
expect "pr: no label, literal template text: nothing recorded" "$(cat "$work/pr-retest/d/pr-url")" ""
expect "pr: ... and says why" "$(grep -c 'no pull request number' "$work/pr-retest/out")" 1
expect "pr: ... and never fails the gate" "$(cat "$work/pr-retest/rc")" 0
mk pr-retest-label "$(labelled 9)"; derive pr-retest-label Succeeded hard PR_NUMBER='{{ body.pull_request.number }}'
expect "pr: literal template text but a label: the label" "$(cat "$work/pr-retest-label/d/pr-url")" "https://github.com/o/gitops-app/pull/9"
mk pr-app "$(labelled 4)"; REPO_URL=https://github.com/o/some-app derive pr-app Succeeded hard
expect "pr: an app repository is not a release: nothing" "$(cat "$work/pr-app/d/pr-url")" ""
mk pr-bad "$(labelled 0x1)"; derive pr-bad Succeeded hard PR_NUMBER=''
expect "pr: a label that is not a number: nothing" "$(cat "$work/pr-bad/d/pr-url")" ""
derive pr-noget Succeeded hard PR_NUMBER=11
expect "pr: the run cannot be read: the param" "$(cat "$work/pr-noget/d/pr-url")" "https://github.com/o/gitops-app/pull/11"
mk pr-dotgit "$(labelled 5)"; REPO_URL=https://github.com/o/gitops-app.git/ derive pr-dotgit Succeeded hard
expect "pr: .git and a trailing slash" "$(cat "$work/pr-dotgit/d/pr-url")" "https://github.com/o/gitops-app/pull/5"

[[ ${fail} -eq 0 ]] && echo "PASS gate_ledger_verdict_test" || { echo "FAIL gate_ledger_verdict_test"; exit 1; }
