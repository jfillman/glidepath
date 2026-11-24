#!/usr/bin/env bash
# Render parity with Clearance's Argo CD plugin (jfillman/clearance providers/argocd/cmp, docs/gate.md "G-ARGO-4").
# The plugin renders `helm template <Application> <chart> --namespace <destination>` (Application = <app>-<env>) with
# the gitops value files and the platform's injected values; a PR's clearance is found at sync time only if the
# clearance-gate task renders the same tree. This runs the task's render step against the plugin's recorded gate-api
# prod fixture (fixtures/clearance-parity; helm is a stand-in that returns the recorded render and keeps its arguments
# and injected values) and holds: the release name, the namespace, the value files, the injected values (equal to the
# plugin's injected.json, and with observability when the cluster sets it), and the tree and every object digest in
# the change request (equal to tekton.json), with every rendered object listed (unchanged ones marked). Since rendering
# moved to the token-less render-manifests task, "the render step" is the chain the Pipeline runs: the gate's publish step,
# render-manifests, then the gate's ingest step (which reads the render as data and builds the change request).
# Needs yq, jq, python3 + pyyaml, helm (to render the task).
# Usage: charts/glidepath-catalog/tests/clearance_gate_parity_test.sh
set -u
cd "$(dirname "$0")/../../.."
# The register step evals the protection library from its environment (files/lib/protection.sh).
PROTECTION_LIB="$(cat charts/glidepath-catalog/files/lib/protection.sh)"; export PROTECTION_LIB
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/clearance-parity"
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/clearance-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/render-manifests.yaml > "$d/rtask.yaml" || { echo "FAIL: render (render-manifests)"; exit 1; }
mkdir -p "$d/bin"
# The stand-in helm: keeps `helm <args>` and answers with the recorded render. A render of the base tree (a path under
# /base/) gets the Rollout's replicas changed, so one object differs and five are unchanged.
cat > "$d/bin/helm" <<'EOF'
#!/usr/bin/env bash
n=$(( $(ls "${WORK}"/helm-*.args 2>/dev/null | wc -l) + 1 ))
printf '%s\n' "$@" > "${WORK}/helm-${n}.args"
if [[ " $* " == *" ${VOL}/inputs/base/"* ]]; then sed 's/^  replicas: 1$/  replicas: 3/' "${FIXTURE}/head.yaml"; else cat "${FIXTURE}/head.yaml"; fi
EOF
chmod +x "$d/bin/helm"
expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
# The paths the steps hardcode, moved under a scenario (one pass, by name): /work /out /rendered /trusted /publish.
loc() { # <work> <out> <rendered> <trusted> <publish>   (the script on stdin)
  sed -E "s#/(work|out|rendered|trusted|publish)([^A-Za-z0-9_-]|\$)#@@\1@@\2#g" \
    | sed -e "s#@@work@@#$1#g" -e "s#@@out@@#$2#g" -e "s#@@rendered@@#$3#g" -e "s#@@trusted@@#$4#g" -e "s#@@publish@@#$5#g" \
          -e "s#/opt/airframe/charts/airframe-application#/chart#g"
}
sstep() { # <task file> <step name>
  yq ".spec.steps[] | select(.name == \"$2\") | .script" "$1"
}
step() { # <index> <work dir>: a step of the gate Task with /work moved (the fetch step's blocks, register, report)
  yq ".spec.steps[$1].script" "$d/task.yaml" | sed -e "s#/work#$2#g" > "$2/step$1.sh"
}
# pipe <scenario> [VAR=value ...]: what the Pipeline does with what the fetch step left in $d/<scenario> (its /work): the gate's
# publish step onto the run's volume ($d/<scenario>/vol), render-manifests (reads vol/inputs, writes vol/rendered), then the gate's
# ingest step in a fresh pod ($d/<scenario>/w is its /work: what the old render step left in /work/eval is in w/eval). Leaves
# render.rc and ingest.rc (the exit codes of the render task and of the ingest step) and their logs.
pipe() {
  local s="$1"; shift
  local W="$d/$s"
  export WORK="$W" FIXTURE="$fx"
  local V="$d/vol-$s"; export VOL="$V"   # outside $W: $W is the fetch step's /work, which publish copies
  rm -rf "$V" "$W/w" "$W/rtmp"; mkdir -p "$V"
  sstep "$d/task.yaml" publish | loc "$W" x x x "$V" > "$d/publish-$s.sh"
  ( cd "$W" && PHASE=fetch PIPELINE_RUN=run-1 bash "$d/publish-$s.sh" ) > "$d/publish-$s.log" 2>&1 || { echo "-- $s: publish failed:"; cat "$d/publish-$s.log"; }
  mkdir -p "$W/w" "$W/rtmp"
  sstep "$d/rtask.yaml" render | loc "$V/inputs" "$V/rendered" x x x > "$W/render.sh"
  ( cd "$W" && PATH="$d/bin:$PATH" RENDER_TMP="$W/rtmp" PROFILE=clearance-gate PIPELINE_RUN=run-1 RENDER_CPU_SECONDS=60 RENDER_MAX_FILE_KB=32768 \
      RENDER_MAX_ENVIRONMENTS=50 APP_NAME=gate-api NAMESPACE=app-gate-api-cicd bash "$W/render.sh" ) > "$W/render.log" 2>&1
  echo $? > "$W/render.rc"
  sstep "$d/task.yaml" ingest | loc "$W/w" x "$V/rendered" "$V/trusted" x > "$W/ingest.sh"
  ( cd "$W/w" && env PATH="$d/bin:$PATH" PHASE=decide PIPELINE_RUN=run-1 GATE=clearance-gate APP_NAME=gate-api NAMESPACE=app-gate-api-cicd \
      RENDER_ONLY=false "$@" bash "$W/ingest.sh" ) > "$W/ingest.log" 2>&1
  echo $? > "$W/ingest.rc"
}
run() { # <scenario> [observability.yaml content]: lays out what the fetch step leaves, then runs the chain
  export WORK="$d/$1" FIXTURE="$fx"
  mkdir -p "$WORK/repo" "$WORK/base"
  cp -R "$fx/hangar-prod" "$WORK/repo/"; cp -R "$fx/hangar-prod" "$WORK/base/"
  mkdir -p "$WORK/observability" "$WORK/policies"; : > "$WORK/policies/unreadable"
  : > "$WORK/observability/hangar-prod.yaml"; [[ -n "${2:-}" ]] && printf '%s\n' "$2" > "$WORK/observability/hangar-prod.yaml"
  echo '{"ttlDays": 3}' > "$WORK/clearance.json"
  jq -n '{owner: "jfillman", repo: "gitops-gate-api", pr: 7, head: ("a" * 40), base: ("b" * 40), author: "dana",
    files: [{filename: "hangar-prod/prod/release.yaml", status: "modified", additions: 1, deletions: 1}], looksAgent: false,
    provenance: null, checkRuns: [], policyCommit: ""}' > "$WORK/meta.json"
  echo "hangar-prod|prod|default|true" > "$WORK/plan.txt"
  pipe "$1"
  [[ "$(cat "$WORK/render.rc")$(cat "$WORK/ingest.rc")" == 00 ]] || { echo "-- $1: render log:"; cat "$WORK/render.log"; echo "-- ingest log:"; cat "$WORK/ingest.log"; }
}
change() { echo "$d/$1/w/eval/hangar-prod__prod/change.json"; }
inj() { echo "$d/$1/rtmp/scratch/injected.hangar-prod.prod.yaml"; }

run plain
w="$d/plain"
expect "render exit" "$(cat "$w/render.rc")$(cat "$w/ingest.rc")" 00
# head is the first helm call, base the second: <template> <release> <chart> --namespace <ns> -f ... -f injected
expect "release name is <app>-<env> (the Application's)" "$(sed -n 2p "$w/helm-1.args")" gate-api-prod
expect "namespace is the destination" "$(sed -n 5p "$w/helm-1.args")" app-gate-api-prod
expect "value files, then the injected values last" "$(sed -n '6,$p' "$w/helm-1.args" | sed "s#$VOL/inputs/repo/##; s#$(inj plain)#INJECTED#" | paste -sd' ' -)" \
  "-f hangar-prod/prod/values.yaml -f hangar-prod/prod/release.yaml -f INJECTED"
expect "the base renders with the same release name" "$(sed -n 2p "$w/helm-2.args")" gate-api-prod
expect "injected values equal the plugin's" \
  "$(yq -o=json -I0 'sort_keys(..)' "$(inj plain)")" "$(jq -S -c . "$fx/injected.json")"
c="$(change plain)"
expect "tree equals the recorded Tekton tree" "$(jq -r .treeDigest "$c")" "$(jq -r .tree "$fx/tekton.json")"
expect "every rendered object is listed" "$(jq '.objects | length' "$c")" "$(jq '.objects | length' "$fx/tekton.json")"
expect "per-object digests equal the recorded ones" \
  "$(jq -S -c '[.objects[] | {key: "\(.apiVersion)/\(.kind)/\(.namespace)/\(.name)", value: .digest}] | from_entries' "$c")" "$(jq -S -c .objects "$fx/tekton.json")"
expect "the changed object is marked changed" "$(jq -r '[.objects[] | select(.op == "changed") | .kind] | join(",")' "$c")" Rollout
expect "the others are marked unchanged" "$(jq '[.objects[] | select(.op == "unchanged")] | length' "$c")" 5

run obs 'observability:
  namespace: "observability"
  instrumentation: "observability/auto"
  runtime: "python"'
w="$d/obs"
expect "observability: render exit" "$(cat "$w/render.rc")$(cat "$w/ingest.rc")" 00
expect "observability is in the injected values, as the ApplicationSet gives it" \
  "$(yq -o=json -I0 'sort_keys(..)' "$(inj obs)")" \
  "$(jq -S -c '. + {observability: {namespace: "observability", instrumentation: "observability/auto", runtime: "python"}}' "$fx/injected.json")"

# Per destination cluster: the fetch step's observability block, run on a two-cluster plan. The control-plane cluster
# (hangar-dev) takes glidepath-app's ConfigMap values; another cluster (hangar-prod) takes its OWN facts, as the broker
# read them from its cluster-defaults.yaml, plus the app's runtime; one that sets none injects nothing; one the broker
# could not read is not rendered at all (it would differ from Argo CD's).
w="$d/clusters"; mkdir -p "$w/observability" "$w/policies"
yq '.spec.steps[] | select(.name == "fetch") | .script' "$d/task.yaml" | sed "s#/work#$w#g" \
  | awk '/# What each protected environment.s destination cluster injects/ {p=1} p {print} p && /^done$/ {exit}' > "$w/pick.sh"
printf 'hangar-dev|prod|default|true\nhangar-prod|prod|default|true\nhangar-qa|prod|default|true\nhangar-lab|prod|default|true\n' > "$w/plan.txt"
printf 'observability:\n  namespace: "dev-obs"\n  instrumentation: "dev-obs/auto"\n  runtime: "python"\n' > "$w/observability/dev-values.yaml"
echo '{"namespace":"obs-prod","instrumentation":"obs-prod/hangar"}' > "$w/policies/hangar-prod.observability.json"
echo 'null' > "$w/policies/hangar-qa.observability.json"
echo hangar-lab > "$w/policies/unreadable"
[[ -s "$w/pick.sh" ]] || { echo "FAIL: the fetch step's observability block was not found"; fail=1; }
( cd "$w" && dev_cluster=hangar-dev runtime=python bash "$w/pick.sh" ) || { echo "FAIL: pick"; fail=1; }
expect "two clusters: the control-plane cluster uses the ConfigMap's block" \
  "$(yq -o=json -I0 .observability "$w/observability/hangar-dev.yaml")" '{"namespace":"dev-obs","instrumentation":"dev-obs/auto","runtime":"python"}'
expect "two clusters: another cluster uses its own facts and the app's runtime" \
  "$(yq -o=json -I0 .observability "$w/observability/hangar-prod.yaml")" '{"namespace":"obs-prod","instrumentation":"obs-prod/hangar","runtime":"python"}'
expect "two clusters: another cluster's block can be appended to the injected values" \
  "$({ printf 'appName: a\nnamespace:\n  labels:\n    x: "y"\n'; cat "$w/observability/hangar-prod.yaml"; } | yq -o=json -I0 .observability.namespace)" '"obs-prod"'
expect "two clusters: a cluster that sets none injects nothing" "$(wc -c < "$w/observability/hangar-qa.yaml" | tr -d ' ')" 0
[[ -e "$w/observability/hangar-lab.yaml.unreadable" ]] && echo "ok   two clusters: an unreadable cluster is marked" || { echo "FAIL unreadable marker"; fail=1; }
# ... and neither the render task nor the gate renders/reads a marked cluster's tree: Argo CD's would differ.
run unreadable-obs
w2="$d/unreadable-obs"; touch "$w2/observability/hangar-prod.yaml.unreadable"; rm -rf "$w2"/helm-*.args
pipe unreadable-obs
expect "unreadable observability: the render task did not render it" "$(ls "$w2"/helm-*.args 2>/dev/null | wc -l | tr -d ' ')" 0
expect "unreadable observability: the environment is render-failed" "$(cat "$w2/w/eval/hangar-prod__prod/status")" render-failed
expect "unreadable observability: the step fails" "$(cat "$w2/ingest.rc")" 1

# The ConfigMap the render reads: the cluster name and runtime always, this cluster's block (glidepath-app's own
# observabilityValues, what the ApplicationSets inject) only when the cluster sets any.
app=(helm template t charts/glidepath-app --set devClusterName=d --set build.x=1 --set platformIdentity.appName=a --set platformIdentity.runtime=python
     --set defaultChart.repoURL=https://x --set defaultChart.targetRevision=v1 --show-only templates/identity/observability-values.yaml)
expect "glidepath-app writes the observability ConfigMap" \
  "$("${app[@]}" --set observability.namespace=obs --set observability.instrumentation=otel/x | yq '.data | [.cluster, .runtime] | join(",")')" "d,python"
expect "with the block" \
  "$("${app[@]}" --set observability.namespace=obs --set observability.instrumentation=otel/x | yq '.data."values.yaml"' | yq -o=json -I0 .)" \
  '{"observability":{"namespace":"obs","instrumentation":"otel/x","runtime":"python"}}'
expect "and without it when the cluster sets none" "$("${app[@]}" | yq '.data | has("values.yaml")')" false

# A new environment (no base tree) lists every object as added.
run newenv
rm -rf "$d/newenv/base/hangar-prod"; mkdir -p "$d/newenv/base"; rm -f "$d/newenv"/helm-*.args
pipe newenv
expect "new environment: all added" "$(jq -r '[.objects[].op] | unique | join(",")' "$(change newenv)")" added

# An `unattested` provenance verdict (an AI co-author trailer outside Clearance) is a person's change, not `unknown`.
run unattested
w="$d/unattested"
jq '.provenance = {identity: {status: "unattested"}, scope: {status: "unattested"}} | .looksAgent = false' "$w/meta.json" > "$w/meta2.json" && mv "$w/meta2.json" "$w/meta.json"
rm -f "$w"/helm-*.args
pipe unattested
expect "unattested: the proposer is a person" "$(jq -r '[.proposerKind, .proposer, (.agent // "none"), .checks.provenance] | join(",")' "$(change unattested)")" "person,user:dana,none,unattested"
run unknown-prov
w="$d/unknown-prov"
jq '.provenance = {identity: {status: "unrecorded"}} | .looksAgent = true' "$w/meta.json" > "$w/meta2.json" && mv "$w/meta2.json" "$w/meta.json"
pipe unknown-prov
expect "an unrecorded agent PR stays unknown" "$(jq -r .proposerKind "$(change unknown-prov)")" unknown

# render-only (the advisory copy's companion): a render failure fails the step; a good render skips every later step.
run ro-ok
w="$d/ro-ok"
pipe ro-ok RENDER_ONLY=true
expect "render-only: a good render exits 0" "$(cat "$w/ingest.rc")" 0
[[ -f "$w/w/skip-rest" ]] && echo "ok   render-only: later steps are skipped" || { echo "FAIL render-only: no skip marker"; fail=1; }
run ro-bad
w="$d/ro-bad"; touch "$w/observability/hangar-prod.yaml.unreadable"
pipe ro-bad RENDER_ONLY=true
rc="$(cat "$w/ingest.rc")"
[[ "${rc}" != 0 && ! -f "$w/w/skip-rest" ]] && echo "ok   render-only: a render failure fails the step (exit ${rc})" || { echo "FAIL render-only: rc=${rc}"; fail=1; }
for step in admission register report; do
  i="$(yq ".spec.steps | to_entries | .[] | select(.value.name == \"${step}\") | .key" "$d/task.yaml")"
  yq ".spec.steps[${i}].script" "$d/task.yaml" | grep -q 'skip-rest' || { echo "FAIL: step ${step} ignores the render-only marker"; fail=1; }
done
# The fetch phase ends at publish: the ingest step marks every later step to skip, and does nothing else.
w="$d/ro-ok"; rm -rf "$w/wf"; mkdir -p "$w/wf"
sstep "$d/task.yaml" ingest | loc "$w/wf" x x x x > "$w/ingest-fetch.sh"
( cd "$w/wf" && env PHASE=fetch PIPELINE_RUN=run-1 bash "$w/ingest-fetch.sh" ) > /dev/null 2>&1
[[ -f "$w/wf/skip-rest" && ! -d "$w/wf/eval" ]] && echo "ok   fetch phase: ingest skips the rest and reads nothing" || { echo "FAIL fetch phase"; fail=1; }

# A plan line whose 4th column is false is never rendered or registered, and a REQUIRED check must still pass for it: render
# skips it (no helm call), register writes outcome `skipped`, report exits 0. The fetch step writes false only on a cluster
# with no Clearance, for an environment cicd.yaml does not mark production: true (files/lib/protection.sh; with a Clearance
# every environment is registered and Clearance decides, tests/clearance_gate_protection_test.sh).
run unprot
w="$d/unprot"; rm -f "$w"/helm-*.args
echo "hangar-staging|staging|default|false" > "$w/plan.txt"
pipe unprot
expect "unprotected: render and ingest exit 0" "$(cat "$w/render.rc")$(cat "$w/ingest.rc")" 00
expect "unprotected: marked skipped" "$(cat "$w/w/eval/hangar-staging__staging/status")" skipped
expect "unprotected: nothing was rendered" "$(ls "$w"/helm-*.args 2>/dev/null | wc -l | tr -d ' ')" 0
w="$d/unprot/w"
for st in register report; do
  i="$(yq ".spec.steps | to_entries | .[] | select(.value.name == \"${st}\") | .key" "$d/task.yaml")"
  yq ".spec.steps[${i}].script" "$d/task.yaml" | sed -e "s#/work#$w#g" -e "s#\$(results.outcome.path)#$w/r.outcome#g" \
    -e "s#\$(results.findings-summary.path)#$w/r.summary#g" -e "s#\$(results.change-ids.path)#$w/r.ids#g" \
    -e "s#/var/run/secrets/clearance#$w#g" > "$w/${st}.sh"
  ( cd "$w" && PATH="$d/bin:$PATH" APP_NAME=gate-api CLEARANCE_URL= TOWER_URL= PIPELINE_RUN=pr1 REPO_URL=https://github.com/o/gitops-gate-api NAMESPACE=x bash "$w/${st}.sh" ) > "$w/${st}.log" 2>&1
  expect "unprotected: ${st} exits 0 (a required check passes)" "$?" 0
done
expect "unprotected: outcome is skipped" "$(cat "$w/r.outcome")" skipped

# The Pipeline: fetch and render first (never ignored), then register; the advisory copy gets a hard render-check after it.
pl() { helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml "$@" --show-only templates/pipelines/clearance-gate-check.yaml; }
adv="$(pl | yq '[.spec.tasks[] | .name + ((.onError // "") | select(. != "") | ":" + .)] | join(" ")')"
expect "advisory pipeline: fetch, render (neither ignored), register-advisory (ignored), a render-check (not ignored) and not-applicable (CG-3)" "${adv}" "fetch render register-advisory:continue render-check not-applicable"
printf 'releaseGuardrailOverrides:\n  clearance-gate: { required: true }\n' > "$d/req.yaml"
req="$(pl -f "$d/req.yaml" | yq '[.spec.tasks[] | .name] | join(" ")')"
expect "required pipeline: fetch, render, one hard register task and not-applicable (CG-3)" "${req}" "fetch render register not-applicable"

[[ "${fail}" == 0 ]] && echo "PASS clearance_gate_parity_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
