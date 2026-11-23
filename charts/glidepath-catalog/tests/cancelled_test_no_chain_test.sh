#!/usr/bin/env bash
# A cancelled or stopped test run never chains (I-71). test's testcaserun.finished fires pass or fail, and the from-test
# Trigger chains on its content's outcome Succeeded or Completed (Completed: a passed run with a when-skipped task, I-52).
# Tekton's $(tasks.status) is Completed for a cancelled run too, so the outcome must come from the run's own spec.status.
# End to end, as rendered: test.yaml's content template with $(tasks.status) filled in, stage-debrief's resolve step
# against a fake kubectl that answers the run's spec.status, then cdevent-send's script against a fake broker, and the
# sent outcome checked against the from-test Trigger's own outcome list. Also checks cdevent-send's own guard alone.
# Needs helm, yq (mikefarah v4), jq, sha256sum.
# Usage: charts/glidepath-catalog/tests/cancelled_test_no_chain_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
EX=charts/glidepath-catalog/tests/values.example.yaml

helm template t charts/glidepath-catalog -f "$EX" --show-only templates/stepactions/cdevent-send.yaml > "$work/sa.yaml" || exit 1
yq '.spec.script' "$work/sa.yaml" > "$work/send.sh" || exit 1
helm template t charts/glidepath-catalog -f "$EX" --show-only templates/tasks/stage-debrief.yaml > "$work/debrief.yaml" || exit 1
# The resolve step's script, with each $(step.results.<name>.path) pointing at a file in the case's dir.
yq '.spec.steps[] | select(.name == "resolve") | .script' "$work/debrief.yaml" \
  | sed 's#\$(step\.results\.\([a-z0-9-]*\)\.path)#${RES}/\1#g' > "$work/resolve.sh" || exit 1
helm template t charts/glidepath-catalog -f "$EX" --show-only templates/pipelines/test.yaml > "$work/test.yaml" || exit 1
TEMPLATE="$(yq '.spec.finally[] | select(.name == "debrief") | .params[] | select(.name == "domain-subject-content-json") | .value' "$work/test.yaml")"
ON_SUCCESS_ONLY="$(yq '.spec.finally[] | select(.name == "debrief") | .params[] | select(.name == "domain-event-on-success-only") | .value' "$work/test.yaml")"
for f in "$work/send.sh" "$work/resolve.sh"; do [[ -s "$f" ]] || { echo "FAIL: nothing rendered into $f"; exit 1; }; done
[[ "${TEMPLATE}" == *'"outcome": "$(tasks.status)"'* ]] || { echo "FAIL: test.yaml's content template no longer carries outcome \$(tasks.status): ${TEMPLATE}"; exit 1; }

# The from-test Trigger's outcome list, from a render of flow-triggers.yaml (build -> test -> release).
cat > "$work/cicd.yaml" <<'EOF'
apiVersion: platform/v1
kind: PipelineConfig
build: { agent: nodejs-20 }
test: { name: integration }
deploy: { environments: [{ name: prod, tier: flight }] }
pipelines:
  ci:
    trigger: { source: git, event: push, branch: main }
    steps: [{ stage: build }, { stage: test, env: prod, testName: integration }, { stage: release, env: prod }]
EOF
helm template x charts/glidepath-app --namespace app-x-cicd -f "$work/cicd.yaml" -f charts/glidepath-app/tests/fixtures/control-plane-values.yaml \
  --show-only templates/triggers/flow-triggers.yaml > "$work/flow.yaml" || exit 1
FILTER="$(yq -N 'select(.kind == "Trigger") | .spec.interceptors[]?.params[]? | select(.name == "filter") | .value' "$work/flow.yaml" | grep testcaserun)"
CHAINS_ON="$(sed -n "s/.*body\.subject\.content\.outcome in \[\([^]]*\)\].*/\1/p" <<<"${FILTER}" | tr -d " '")"
[[ "${CHAINS_ON}" == "Succeeded,Completed" ]] || { echo "FAIL: the from-test Trigger's outcome list is '${CHAINS_ON}' (filter: ${FILTER})"; exit 1; }
chains() { [[ ",${CHAINS_ON}," == *",$1,"* ]]; }

# --- fakes ------------------------------------------------------------------------------------------------------
mkdir -p "$work/lib" "$work/bin"
cp catalog/lib/chain-slug.sh "$work/lib/"
sed "s#/var/run/secrets/platform/broker-token#${work}/broker-token#" catalog/lib/cdevents.sh > "$work/lib/cdevents.sh"
echo "sa-token" > "$work/broker-token"
# curl: the broker. Every POST body goes to $FX/sends.
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
data=""
while (( $# )); do case "$1" in -d) data="$2"; shift ;; esac; shift; done
jq -c . <<<"$data" >> "$FX/sends"
printf '{"eventListener":"cdevents-broker","eventID":"el-1"}'
EOF
# kubectl: the run's spec.status ($FX/spec-status) for the jsonpath read, an empty run for -o json, nothing for TaskRuns.
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case " $* " in
  *" get taskrun "*) exit 0 ;;
  *"{.spec.status}"*) cat "$FX/spec-status" 2>/dev/null; exit 0 ;;
  *" -o json"*) echo '{"metadata":{"annotations":{}}}'; exit 0 ;;
esac
exit 1
EOF
chmod +x "$work/bin/"*

RUN=ci-1-test-brave-heron-1a2b3c4d
check() { # check <case> <description> <condition...>
  local c="$1" d="$2"; shift 2
  if "$@"; then echo "ok   $c: $d"; else echo "FAIL $c: $d"; [[ -f "$FX/out" ]] && sed 's/^/       | /' "$FX/out"; fail=1; fi
}
eq() { [[ "$1" == "$2" ]]; }
not() { ! "$@"; }

# stage <case> <$(tasks.status)> <run spec.status>: resolve then send, as stage-debrief's steps run them. Sets $FX.
stage() {
  FX="$work/fx-$1"; RES="$FX/res"; mkdir -p "$RES"; printf '%s' "$3" > "$FX/spec-status"
  (
    export PATH="$work/bin:$PATH" FX RES PLATFORM_LIB="$work/lib"
    PIPELINE_STATUS="$2" APP_NAMESPACE=app-x-cicd PIPELINE_RUN_NAME="$RUN" IMAGE_REF_PARAM=ghcr.io/x/y@sha256:1 \
      IMAGE_REF_FROM_TASK="" CONFIG_JSON_PARAM="{}" CONFIG_JSON_FROM_TASK="" CONTENT_TEMPLATE="${TEMPLATE//\$(tasks.status)/$2}" \
      bash "$work/resolve.sh" > "$FX/out" 2>&1
    export CDEVENTS_BROKER_URL=http://broker.test EVENT_TYPE=dev.cdevents.testcaserun.finished.0.3.0 SUBJECT_TYPE=testCaseRun \
      SUBJECT_CONTENT_JSON="{}" NAMESPACE=app-x-cicd TEKTON_PIPELINE_RUN="$RUN" PLATFORM_CHAIN_ID=4f9c2a71-0b3e-4c55-9d1e-6a7b8c9d0e1f \
      PLATFORM_TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01 PLATFORM_FLOW_START_TIME=2026-11-23T00:30:00Z \
      TEKTON_STATUS="" PLATFORM_CONFIG_JSON="{}" ONLY_ON_SUCCESS="${ON_SUCCESS_ONLY}" PIPELINE_STATUS="$(cat "$RES/status")" \
      SUBJECT_CONTENT_JSON_B64="$(cat "$RES/subject-content-json-b64")" PLATFORM_CONFIG_JSON_B64="" CONFIRM_SUCCESSOR=false
    bash "$work/send.sh" >> "$FX/out" 2>&1
  )
}
sent_outcome() { jq -r '.subject.content.outcome' "$FX/sends" 2>/dev/null; }
sends() { [[ -f "$FX/sends" ]] && wc -l < "$FX/sends" | tr -d ' ' || echo 0; }

eq "${ON_SUCCESS_ONLY}" "false" || { echo "FAIL: test's domain event is no longer sent pass or fail (on-success-only ${ON_SUCCESS_ONLY})"; fail=1; }

# 1. The I-71 failure: cancelled from Tower before any task failed. Tekton says Completed; the event must not chain.
stage cancelled Completed CancelledRunFinally
check cancelled "resolve reports Cancelled" eq "$(cat "$RES/status")" Cancelled
check cancelled "the event is still sent (test fires pass or fail)" eq "$(sends)" 1
check cancelled "its outcome is Cancelled" eq "$(sent_outcome)" Cancelled
check cancelled "the from-test Trigger does not chain on it" not chains "$(sent_outcome)"

# 2. Stopped (StoppedRunFinally) and a plain Cancelled spec.status: the same.
stage stopped Completed StoppedRunFinally
check stopped "outcome Cancelled, no chain" bash -c '[[ "$1" == Cancelled ]] && [[ ",$2," != *",$1,"* ]]' _ "$(sent_outcome)" "${CHAINS_ON}"
stage cancelled-plain Completed Cancelled
check cancelled-plain "outcome Cancelled, no chain" bash -c '[[ "$1" == Cancelled ]] && [[ ",$2," != *",$1,"* ]]' _ "$(sent_outcome)" "${CHAINS_ON}"

# 3. A passed run with a when-skipped task (I-52): Completed, nobody cancelled it. Chains.
stage skipped Completed ""
check skipped "outcome Completed" eq "$(sent_outcome)" Completed
check skipped "the from-test Trigger chains on it" chains "$(sent_outcome)"

# 4. A passed run, nothing skipped. Chains.
stage passed Succeeded ""
check passed "outcome Succeeded, chains" chains "$(sent_outcome)"

# 5. A failed run. Sent with outcome Failed, does not chain.
stage failed Failed ""
check failed "one send, outcome Failed" eq "$(sends) $(sent_outcome)" "1 Failed"
check failed "the from-test Trigger does not chain on it" not chains "$(sent_outcome)"

# 6. Only the outcome differs: the rest of the cancelled run's content is what a passed run sends.
FX="$work/fx-cancelled"
check content "the cancel changes the outcome and nothing else" \
  eq "$(jq -c '.subject.content | del(.outcome)' "$work/fx-cancelled/sends")" "$(jq -c '.subject.content | del(.outcome)' "$work/fx-skipped/sends")"

# 7. cdevent-send's own guard, without resolve's rewrite: status Cancelled with content still saying Completed.
FX="$work/fx-guard"; mkdir -p "$FX"
(
  export PATH="$work/bin:$PATH" FX PLATFORM_LIB="$work/lib"
  export CDEVENTS_BROKER_URL=http://broker.test EVENT_TYPE=dev.cdevents.testcaserun.finished.0.3.0 SUBJECT_TYPE=testCaseRun \
    SUBJECT_CONTENT_JSON='{"outcome":"Completed","image":"i"}' NAMESPACE=app-x-cicd TEKTON_PIPELINE_RUN="$RUN" \
    PLATFORM_CHAIN_ID=4f9c2a71-0b3e-4c55-9d1e-6a7b8c9d0e1f PLATFORM_TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01 \
    PLATFORM_FLOW_START_TIME=2026-11-23T00:30:00Z TEKTON_STATUS="" \
    PLATFORM_CONFIG_JSON="{}" ONLY_ON_SUCCESS=false PIPELINE_STATUS=Cancelled SUBJECT_CONTENT_JSON_B64="" PLATFORM_CONFIG_JSON_B64="" \
    CONFIRM_SUCCESSOR=false
  bash "$work/send.sh" > "$FX/out" 2>&1
)
check guard "status Cancelled overrides a Completed outcome" eq "$(sent_outcome)" Cancelled
check guard "and says so" grep -q "the content says Completed but the stage status is Cancelled" "$FX/out"

exit "$fail"
