#!/usr/bin/env bash
# Runs the stalled-pipeline detector's real script against a stub kubectl and checks the queue-stall rule:
# a Pipelines-as-Code queued run waiting with free slots alerts; a full queue, a fresh run and another
# repository's runs do not. Needs helm, python3 (pyyaml), jq (1.6+), bash.
# Usage: charts/glidepath-control-plane/tests/stalled_queue_detector_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
failfile="$(mktemp)"  # run() executes in a command substitution; a detector failure is recorded here
script="$(mktemp)"
helm template t charts/glidepath-control-plane -f charts/glidepath-control-plane/tests/values.example.yaml \
  --show-only templates/broker/stalled-pipeline-detector-cronjob.yaml \
  | python3 -c "
import sys,yaml
cj=[d for d in yaml.safe_load_all(sys.stdin.read()) if d and d['kind']=='CronJob'][0]
print(cj['spec']['jobTemplate']['spec']['template']['spec']['containers'][0]['args'][0])" > "$script"

stub="$(mktemp -d)"
cat > "${stub}/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "get repositories.pipelinesascode.tekton.dev") printf '%s' "${REPOS}" ;;
  "get pipelinerun") printf '%s' "${RUNS}" ;;
  "label pipelinerun") echo "LABEL $3 $6" >> "${LOG}" ;;
  "create -f") echo "EVENT $(grep -E '^reason:|^  name:|^message:' | tr '\n' ' ')" >> "${LOG}" ;;
esac
STUB
chmod +x "${stub}/kubectl"

# Timestamps relative to now, without GNU date.
ago() { python3 -c "import datetime as d,sys; print((d.datetime.now(d.timezone.utc)-d.timedelta(minutes=int(sys.argv[1]))).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$1"; }
repo() { jq -n --arg n "$1" --argjson l "$2" '{metadata:{name:$n,namespace:"app-x-cicd"},spec:{concurrency_limit:$l}}'; }
pr() { # name repo state(queued|started|none) minutesOld [finished]
  jq -n --arg n "$1" --arg r "$2" --arg s "$3" --arg c "$(ago "$4")" --arg f "${5:-}" '
    {metadata:{name:$n,namespace:"app-x-cicd",creationTimestamp:$c,labels:({"pipelinesascode.tekton.dev/repository":$r}
       + (if $s=="none" then {} else {"pipelinesascode.tekton.dev/state":$s} end))},
     spec:(if $s=="queued" then {status:"PipelineRunPending"} else {} end),
     status:(if $f=="done" then {completionTime:$c,conditions:[{type:"Succeeded",status:"True"}]} else {} end)}'; }
run() { # repos-json-items runs-json-items
  LOG="$(mktemp)"; export LOG
  PATH="${stub}:${PATH}" REPOS="{\"items\":[$1]}" RUNS="{\"items\":[$2]}" STALL_THRESHOLD_MINUTES=10 QUEUE_STALL_MINUTES=10 \
    bash "$script" >/dev/null 2>&1 || { echo "FAIL: script exited non-zero"; [ "${EXPECT_FAIL:-}" = 1 ] || echo 1 >> "${failfile}"; }
  cat "$LOG"; }
expect() { grep -q -- "$2" <<<"$3" || { echo "FAIL: $1 (wanted '$2', got: ${3:-nothing})"; fail=1; }; }
refute() { grep -q -- "$2" <<<"$3" && { echo "FAIL: $1 (did not want '$2', got: $3)"; fail=1; }; true; }

# Stalled: limit 3, one run started, 25 minutes queued.
out="$(run "$(repo r1 3)" "$(pr q1 r1 queued 25),$(pr s1 r1 started 30)")"
expect "a queue waiting with free slots is stalled" "reason: QueueStalled" "$out"
expect "the event names the queued run" "name: q1" "$out"
expect "the message names the repository" "Repository r1" "$out"
expect "the message gives running against limit" "1 of 3" "$out"
expect "the message carries the recovery hint" "rollout restart deployment/pipelines-as-code-watcher" "$out"
expect "the queued run is labelled so it alerts once" "LABEL q1 hangar.io/stall-alerted=true" "$out"

# A finished run still labelled started does not hold a slot as far as this rule is concerned (it is not running).
out="$(run "$(repo r1 3)" "$(pr q1 r1 queued 25),$(pr s1 r1 started 30 done)")"
expect "a finished run is not counted as running" "reason: QueueStalled" "$out"

# Busy: limit reached.
out="$(run "$(repo r1 2)" "$(pr q1 r1 queued 25),$(pr s1 r1 started 30),$(pr s2 r1 started 30)")"
refute "a queue at its limit is busy, not stalled" "QueueStalled" "$out"

# Fresh: under the threshold.
out="$(run "$(repo r1 3)" "$(pr q1 r1 queued 3)")"
refute "a run queued 3 minutes is not stalled" "QueueStalled" "$out"

# Other repository: its running runs do not fill this repository's slots.
out="$(run "$(repo r1 2),$(repo r2 2)" "$(pr q1 r1 queued 25),$(pr o1 r2 started 30),$(pr o2 r2 started 30)")"
expect "another repository's running runs are not counted" "name: q1" "$out"
# ...and its queue is not affected by r1's.
out="$(run "$(repo r1 2),$(repo r2 2)" "$(pr q2 r2 queued 25),$(pr a1 r1 started 30),$(pr a2 r1 started 30)")"
expect "free slots are judged per repository" "name: q2" "$out"

# One event per repository however many runs wait, all labelled.
out="$(run "$(repo r1 3)" "$(pr q1 r1 queued 25),$(pr q2 r1 queued 20),$(pr q3 r1 queued 15)")"
[ "$(grep -c '^EVENT' <<<"$out")" = 1 ] || { echo "FAIL: want exactly one event for one repository, got: $out"; fail=1; }
expect "every stalled queued run is labelled" "LABEL q3" "$out"
expect "the oldest queued run is the one named" "name: q1" "$out"

# A failed Event create must not leave the run labelled (the next scan would never alert).
sed -i.bak 's/"create -f") echo/"create -f") [ "${EVENT_FAIL:-}" = 1 ] \&\& { echo "CREATE_ATTEMPT" >> "${LOG}"; exit 1; }; echo/' "${stub}/kubectl"
out="$(EVENT_FAIL=1 EXPECT_FAIL=1 run "$(repo r1 3)" "$(pr q1 r1 queued 25)")"
expect "the Event create was attempted (and failed)" "CREATE_ATTEMPT" "$out"
refute "no label is set when the Event could not be created" "LABEL" "$out"

# Not applicable: no limit on the Repository, an already-alerted run, a non-queued pending state.
out="$(run '{"metadata":{"name":"r1","namespace":"app-x-cicd"},"spec":{}}' "$(pr q1 r1 queued 25)")"
refute "a repository without a limit has no queue" "QueueStalled" "$out"
out="$(run "$(repo r1 3)" "$(pr q1 r1 started 25)")"
refute "a started run is not queued" "QueueStalled" "$out"

[ -s "${failfile}" ] && { echo "FAIL: the detector exited non-zero in at least one case"; fail=1; }
[ $fail -eq 0 ] && echo ok || exit 1
