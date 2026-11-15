#!/usr/bin/env bash
# The chain the gate Pipelines run around the token-less render, for the shell tests that exercise a gate's later steps
# (policy_validation_test.sh, clearance_gate_admission_test.sh). Source it; it needs $d (a scratch dir), $fx (fixtures/policy-validation)
# and yq. Layout of one scenario directory $W:
#   $W/pub     the fetch step's /work: repo, base, charts, plan.txt, policies, meta.json ... (the test lays this out)
#   $W/vol     the run's volume: the gate's publish step writes inputs/ and trusted/, render-manifests writes rendered/
#   $W        the decide phase's /work: ingest writes eval/ here, and the steps after it (admission, register, evaluate) read it
# gate_chain publishes, renders (real helm) and ingests; it leaves $W/render.rc (the render task's exit code), $W/ingest.rc and the
# logs (render.log, ingest.log; $W/chain.log is both, in order). The steps after ingest are the caller's.

gate_loc() { # <work> <out> <rendered> <trusted> <publish>   (a script on stdin: the paths it hardcodes, moved; one pass, by name)
  sed -E "s#/(work|out|rendered|trusted|publish)([^A-Za-z0-9_-]|\$)#@@\1@@\2#g" \
    | sed -e "s#@@work@@#$1#g" -e "s#@@out@@#$2#g" -e "s#@@rendered@@#$3#g" -e "s#@@trusted@@#$4#g" -e "s#@@publish@@#$5#g" \
          -e "s#\$(results.outcome.path)#$1/outcome#g" -e "s#\$(results.findings-summary.path)#$1/summary#g" \
          -e "s#\$(results.change-ids.path)#$1/change-ids#g" \
          -e "s#/opt/airframe/charts/airframe-application#${fx}/chart#g" -e "s#/opt/airframe/xrds#${fx}/airframe/xrds#g"
}
gate_sstep() { # <task file> <step name>
  yq ".spec.steps[] | select(.name == \"$2\") | .script" "$1"
}
gate_chain() { # <gate task yaml> <render-manifests task yaml> <clearance-gate|policy-validation> <scenario dir> <app> [VAR=value ...]
  local task="$1" rtask="$2" gate="$3" W="$4" app="$5"; shift 5
  local gate_dir="$gate"
  rm -rf "$W/vol" "$W/rtmp" "$W/eval"; mkdir -p "$W/vol" "$W/rtmp"
  gate_sstep "$task" publish | gate_loc "$W/pub" x x x "$W/vol" > "$W/publish.sh"
  ( cd "$W/pub" && PHASE=fetch PIPELINE_RUN=run-1 bash "$W/publish.sh" ) > "$W/publish.log" 2>&1 || { echo "-- publish failed:"; cat "$W/publish.log"; }
  gate_sstep "$rtask" render | gate_loc "$W/vol/inputs" "$W/vol/rendered" x x x > "$W/render.sh"
  ( cd "$W" && PATH="$d/bin:$PATH" RENDER_TMP="$W/rtmp" PROFILE="$gate" PIPELINE_RUN=run-1 RENDER_CPU_SECONDS=60 RENDER_MAX_FILE_KB=32768 RENDER_MAX_ENVIRONMENTS=50 \
      APP_NAME="$app" NAMESPACE="app-${app}-cicd" bash "$W/render.sh" ) > "$W/render.log" 2>&1
  echo $? > "$W/render.rc"
  gate_sstep "$task" ingest | gate_loc "$W" x "$W/vol/rendered" "$W/vol/trusted" x > "$W/ingest.sh"
  ( cd "$W" && env PATH="$d/bin:$PATH" PHASE=decide PIPELINE_RUN=run-1 GATE="$gate_dir" APP_NAME="$app" NAMESPACE="app-${app}-cicd" RENDER_ONLY=false \
      "$@" bash "$W/ingest.sh" ) > "$W/ingest.log" 2>&1
  echo $? > "$W/ingest.rc"
  cat "$W/render.log" "$W/ingest.log" > "$W/chain.log"
}
