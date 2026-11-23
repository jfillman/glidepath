#!/usr/bin/env bash
# At-least-once stage chaining (I-68): the cdevent-send StepAction's delivery confirmation, as rendered. Runs the
# StepAction's script with the real catalog/lib/cdevents.sh against a fake broker (curl) that plays the next step's
# Trigger: it names the PipelineRun the way flow-triggers.yaml does (<flow>-<index>-<stage>-<chain_slug>-<context.id>)
# and refuses a second create of the same name (AlreadyExists), and can lose or delay deliveries. Checks that a lost
# event is re-sent with the same id until the next run exists, that no sequence of re-sends makes a second run, that
# the step fails when the next stage never starts, and that nothing waits when no next stage is expected.
# Needs helm, yq (mikefarah v4), jq, sha256sum.
# Usage: charts/glidepath-catalog/tests/chain_delivery_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
EX=charts/glidepath-catalog/tests/values.example.yaml

helm template t charts/glidepath-catalog -f "$EX" --show-only templates/stepactions/cdevent-send.yaml > "$work/sa.yaml" || exit 1
yq '.spec.script' "$work/sa.yaml" > "$work/send.sh" || exit 1
[[ -s "$work/send.sh" ]] || { echo "FAIL: no script in the rendered cdevent-send StepAction"; exit 1; }
helm template t charts/glidepath-catalog -f "$EX" --show-only templates/tasks/stage-debrief.yaml > "$work/debrief.yaml" || exit 1

# The libs, with the broker token read from the work dir instead of the pod's projected volume.
mkdir -p "$work/lib" "$work/bin"
cp catalog/lib/chain-slug.sh "$work/lib/"
sed "s#/var/run/secrets/platform/broker-token#${work}/broker-token#" catalog/lib/cdevents.sh > "$work/lib/cdevents.sh"
echo "sa-token" > "$work/broker-token"

# --- fakes ------------------------------------------------------------------------------------------------------
# curl: the broker plus the next step's Trigger. Every POST body goes to $FX/sends. $FX/lose N: the first N
# deliveries reach no Trigger (the I-68 interceptor timeout). $FX/delay N: the first N deliveries are held and only
# reach the Trigger together with the next send (a slow first delivery racing a re-send). $FX/broker-down N: the
# first N sends fail at the broker (curl exit 22).
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
data=""
while (( $# )); do case "$1" in -d) data="$2"; shift ;; esac; shift; done
jq -c . <<<"$data" >> "$FX/sends"
n="$(wc -l < "$FX/sends" | tr -d ' ')"
if (( n <= $(cat "$FX/broker-down" 2>/dev/null || echo 0) )); then echo "curl: (22) 503" >&2; exit 22; fi
trigger() {   # trigger <payload>: what the next step's Trigger does with one delivered event
  local id slug name
  id="$(jq -r '.context.id' <<<"$1")"; slug="$(jq -r '.customData.platform.chain_slug' <<<"$1")"
  name="$(cat "$FX/next-prefix")-${slug}-${id}"
  if grep -qx "$name" "$FX/prs" 2>/dev/null; then echo "AlreadyExists $name" >> "$FX/refused"; else echo "$name" >> "$FX/prs"; fi
}
if (( n <= $(cat "$FX/lose" 2>/dev/null || echo 0) )); then :
elif (( n <= $(cat "$FX/delay" 2>/dev/null || echo 0) )); then jq -c . <<<"$data" >> "$FX/held"
else
  if [[ -f "$FX/held" ]]; then while IFS= read -r held; do trigger "$held"; done < "$FX/held"; rm -f "$FX/held"; fi
  trigger "$data"
fi
printf '{"eventListener":"cdevents-broker","eventID":"el-%s"}' "$n"
EOF
# kubectl: the run's own JSON for `get pipelinerun <name> -o json`, the names in $FX/prs plus $FX/other for a
# label-selected list. Logs each call to $FX/kubectl.
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$FX/kubectl"
case " $* " in
  *" -l "*) cat "$FX/prs" "$FX/other" 2>/dev/null; exit 0 ;;
  *" -o json"*) cat "$FX/run.json"; exit 0 ;;
esac
exit 1
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$work/bin/sleep"
chmod +x "$work/bin/"*

CHAIN=4f9c2a71-0b3e-4c55-9d1e-6a7b8c9d0e1f
RUN=ci-2-test-brave-heron-1a2b3c4d
# run_json <flow-label> <step-index> <is-flow-terminal>
run_json() {
  jq -n --arg f "$1" --arg i "$2" --arg t "$3" '{metadata: {name: "'"$RUN"'", labels: (if $f == "" then {} else {"hangar.io/flow": $f, "hangar.io/step-index": $i} end)},
    spec: {params: [{name: "is-flow-terminal", value: $t}]}}'
}

# run_case <name> <confirm-successor> <status> <content-json>; set up $FX before. Output in $FX/out, exit in $FX/rc.
run_case() {
  (
    export PATH="$work/bin:$PATH" FX PLATFORM_LIB="$work/lib"
    export CDEVENTS_BROKER_URL=http://broker.test EVENT_TYPE=dev.cdevents.testcaserun.finished.0.3.0 SUBJECT_TYPE=testCaseRun \
      SUBJECT_CONTENT_JSON="$4" NAMESPACE=app-flight-api-cicd TEKTON_PIPELINE_RUN="$RUN" PLATFORM_CHAIN_ID="$CHAIN" \
      PLATFORM_TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01 PLATFORM_FLOW_START_TIME=2026-11-23T00:30:00Z \
      TEKTON_STATUS="" PLATFORM_CONFIG_JSON="{}" ONLY_ON_SUCCESS=false PIPELINE_STATUS="$3" SUBJECT_CONTENT_JSON_B64="" \
      PLATFORM_CONFIG_JSON_B64="" CONFIRM_SUCCESSOR="$2" CONFIRM_WAIT_SECONDS=6 CONFIRM_RESENDS=3 CONFIRM_POLL_SECONDS=3
    bash "$work/send.sh" > "$FX/out" 2>&1
    echo $? > "$FX/rc"
  )
}
new_fx() { FX="$work/fx-$1"; mkdir -p "$FX"; echo "ci-3-release" > "$FX/next-prefix"; run_json ci 2 false > "$FX/run.json"; }
count() { [[ -f "$1" ]] && wc -l < "$1" | tr -d ' ' || echo 0; }
check() { # check <case> <description> <condition...>
  local c="$1" d="$2"; shift 2
  if "$@"; then echo "ok   $c: $d"; else echo "FAIL $c: $d"; [[ -f "$FX/out" ]] && sed 's/^/       | /' "$FX/out"; fail=1; fi
}
eq() { [[ "$1" == "$2" ]]; }
OK_CONTENT='{"outcome":"Succeeded","image":"ghcr.io/x/y@sha256:1"}'

# 1. Delivered the first time: one send, confirmed, one run.
new_fx delivered; run_case delivered true Succeeded "$OK_CONTENT"
check delivered "exit 0" eq "$(cat "$FX/rc")" 0
check delivered "one send" eq "$(count "$FX/sends")" 1
check delivered "one next-stage run" eq "$(count "$FX/prs")" 1
check delivered "logs the confirmation" grep -q "delivery confirmed: next stage app-flight-api-cicd/ci-3-release-" "$FX/out"

# 2. The I-68 failure: the first two deliveries reach no Trigger. Re-sent until the run exists, exactly one run.
new_fx lost; echo 2 > "$FX/lose"; run_case lost true Completed '{"outcome":"Completed"}'
check lost "exit 0" eq "$(cat "$FX/rc")" 0
check lost "three sends" eq "$(count "$FX/sends")" 3
check lost "exactly one next-stage run" eq "$(count "$FX/prs")" 1
check lost "every send carries the same context.id" eq "$(jq -r '.context.id' "$FX/sends" | sort -u | wc -l | tr -d ' ')" 1
check lost "re-send is logged" grep -q "re-sending the same event" "$FX/out"

# 3. A slow first delivery races the re-send: both reach the Trigger, the second create is refused, one run.
new_fx race; echo 1 > "$FX/delay"; run_case race true Succeeded "$OK_CONTENT"
check race "exit 0" eq "$(cat "$FX/rc")" 0
check race "two sends" eq "$(count "$FX/sends")" 2
check race "exactly one next-stage run" eq "$(count "$FX/prs")" 1
check race "the duplicate create was refused by name" eq "$(count "$FX/refused")" 1

# 4. Never delivered: 1 + confirm-resends sends, then the step fails and says why. No run.
new_fx never; echo 99 > "$FX/lose"; run_case never true Succeeded "$OK_CONTENT"
check never "exit 1" eq "$(cat "$FX/rc")" 1
check never "four sends (1 + 3 re-sends)" eq "$(count "$FX/sends")" 4
check never "no run" eq "$(count "$FX/prs")" 0
check never "names the broken chain" grep -q "next stage never started: no PipelineRun ci-3-<stage>-.* in app-flight-api-cicd" "$FX/out"

# 5. The broker itself refuses the first send (curl fails after its retries): the loop re-sends instead of giving up.
new_fx broker; echo 1 > "$FX/broker-down"; run_case broker true Succeeded "$OK_CONTENT"
check broker "exit 0" eq "$(cat "$FX/rc")" 0
check broker "two sends" eq "$(count "$FX/sends")" 2
check broker "one run" eq "$(count "$FX/prs")" 1

# 6. Another chain's next-step run in the same flow and step is not this event's successor.
new_fx other; echo 99 > "$FX/lose"; echo "ci-3-release-calm-otter-deadbeef" > "$FX/other"; run_case other true Succeeded "$OK_CONTENT"
check other "an unrelated run does not confirm delivery" eq "$(cat "$FX/rc")" 1

# 7-11. No next stage expected: one send, no waiting, no list of PipelineRuns.
new_fx terminal; run_json ci 3 true > "$FX/run.json"; echo 99 > "$FX/lose"; run_case terminal true Succeeded "$OK_CONTENT"
check terminal "last step: exit 0, one send, no lookup" eq "$(cat "$FX/rc") $(count "$FX/sends") $(grep -c -- ' -l ' "$FX/kubectl")" "0 1 0"
new_fx noflow; run_json "" "" false > "$FX/run.json"; echo 99 > "$FX/lose"; run_case noflow true Succeeded "$OK_CONTENT"
check noflow "not in a flow: exit 0, one send" eq "$(cat "$FX/rc") $(count "$FX/sends")" "0 1"
new_fx failed; echo 99 > "$FX/lose"; run_case failed true Failed '{"outcome":"Failed"}'
check failed "failed stage: exit 0, one send" eq "$(cat "$FX/rc") $(count "$FX/sends")" "0 1"
new_fx cancelled; echo 99 > "$FX/lose"; run_case cancelled true Cancelled '{"outcome":"Completed"}'
check cancelled "cancelled stage (outcome still Completed): exit 0, one send" eq "$(cat "$FX/rc") $(count "$FX/sends")" "0 1"
new_fx off; echo 99 > "$FX/lose"; run_case off false Succeeded "$OK_CONTENT"
check off "confirm-successor false: one send, no kubectl at all" eq "$(cat "$FX/rc") $(count "$FX/sends") $(count "$FX/kubectl")" "0 1 0"

# 12. The StepAction's event id is cdevents.sh's, character for character - the dedupe rests on it.
# Case 1 already shows it end to end (the fake Trigger names the run from the payload's context.id, the step finds it
# by its own computed id); this pins it directly: the id the step logs is the context.id cdevents.sh sent.
FX="$work/fx-delivered"
check parity "the step's event id is the sent context.id" \
  eq "$(sed -n 's/.*(subject testCaseRun, id \([0-9a-f]*\)).*/\1/p' "$FX/out")" "$(jq -r '.context.id' "$FX/sends")"

# 13. stage-debrief turns it on for the domain event only, never for pipelinerun.finished.
on="$(yq '[.spec.steps[] | select(.params[]? | (.name == "confirm-successor" and .value == "true")) | .name] | join(",")' "$work/debrief.yaml")"
check debrief "confirm-successor is on for domain-event only" eq "$on" "domain-event"

# 14. The Trigger still names the next run from the event id (the other half of the dedupe).
cat > "$work/cicd.yaml" <<'EOF'
apiVersion: platform/v1
kind: PipelineConfig
build: { agent: nodejs-20 }
test: { name: integration }
deploy: { environments: [{ name: dev, tier: ground }, { name: prod, tier: flight }] }
pipelines:
  ci:
    trigger: { source: git, event: push, branch: main }
    steps: [{ stage: build }, { stage: test, env: dev, testName: integration }, { stage: deploy, env: dev }, { stage: release, env: prod }]
EOF
helm template x charts/glidepath-app --namespace app-x-cicd -f "$work/cicd.yaml" -f charts/glidepath-app/tests/fixtures/control-plane-values.yaml \
  --show-only templates/triggers/flow-triggers.yaml > "$work/flow.yaml" || { echo "FAIL: flow-triggers render"; fail=1; }
names="$(yq -N 'select(.kind == "TriggerBinding") | .spec.params[] | select(.name == "pipeline-run-name") | .value' "$work/flow.yaml")"
check trigger "three flow Triggers rendered" eq "$(grep -c . <<<"$names")" 3
check trigger "every flow Trigger names its run <flow>-<i>-<stage>-<chain_slug>-<context.id>" \
  bash -c '! grep -v -- "-\$(body.customData.platform.chain_slug)-\$(body.context.id)\$" <<<"$1"' _ "$names"
check trigger "no flow TriggerTemplate uses generateName" bash -c '! grep -q generateName "$1"' _ "$work/flow.yaml"
check trigger "the next run carries the flow and step-index labels the lookup selects on" \
  eq "$(yq -N 'select(.kind == "TriggerTemplate") | .spec.resourcetemplates[0].metadata.labels | (.["hangar.io/flow"] + "/" + .["hangar.io/step-index"])' "$work/flow.yaml" | tr '\n' ' ')" "ci/1 ci/2 ci/3 "

exit "$fail"
