#!/usr/bin/env bash
# Render parity with Clearance's Argo CD plugin (jfillman/clearance providers/argocd/cmp, docs/gate.md "G-ARGO-4").
# The plugin renders `helm template <Application> <chart> --namespace <destination>` (Application = <app>-<env>) with
# the gitops value files and the platform's injected values; a PR's clearance is found at sync time only if the
# clearance-gate task renders the same tree. This runs the task's render step against the plugin's recorded gate-api
# prod fixture (fixtures/clearance-parity; helm is a stand-in that returns the recorded render and keeps its arguments
# and injected values) and holds: the release name, the namespace, the value files, the injected values (equal to the
# plugin's injected.json, and with observability when the cluster sets it), and the tree and every object digest in
# the change request (equal to tekton.json), with every rendered object listed (unchanged ones marked).
# Needs yq, jq, python3 + pyyaml, helm (to render the task).
# Usage: charts/glidepath-catalog/tests/clearance_gate_parity_test.sh
set -u
cd "$(dirname "$0")/../../.."
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/clearance-parity"
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/clearance-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
mkdir -p "$d/bin"
# The stand-in helm: keeps `helm <args>` and answers with the recorded render. A render of the base tree (a path under
# /base/) gets the Rollout's replicas changed, so one object differs and five are unchanged.
cat > "$d/bin/helm" <<'EOF'
#!/usr/bin/env bash
n=$(( $(ls "${WORK}"/helm-*.args 2>/dev/null | wc -l) + 1 ))
printf '%s\n' "$@" > "${WORK}/helm-${n}.args"
if [[ " $* " == *" ${WORK}/base/"* ]]; then sed 's/^  replicas: 1$/  replicas: 3/' "${FIXTURE}/head.yaml"; else cat "${FIXTURE}/head.yaml"; fi
EOF
chmod +x "$d/bin/helm"
expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
step() { # <index> <work dir>
  yq ".spec.steps[$1].script" "$d/task.yaml" | sed -e "s#/work#$2#g" \
    -e "s#/opt/airframe/charts/airframe-application#/chart#g" > "$2/step$1.sh"
}
run() { # <scenario> [observability.yaml content]: lays out what the fetch step leaves, runs render
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
  step 1 "$WORK"
  ( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME=gate-api NAMESPACE=app-gate-api-cicd bash "$WORK/step1.sh" ) > "$WORK/render.log" 2>&1
  echo $? > "$WORK/render.rc"
  [[ "$(cat "$WORK/render.rc")" == 0 ]] || { echo "-- $1: render step log:"; cat "$WORK/render.log"; }
}
change() { echo "$d/$1/eval/hangar-prod__prod/change.json"; }

run plain
w="$d/plain"
expect "render exit" "$(cat "$w/render.rc")" 0
# head is the first helm call, base the second: <template> <release> <chart> --namespace <ns> -f ... -f injected
expect "release name is <app>-<env> (the Application's)" "$(sed -n 2p "$w/helm-1.args")" gate-api-prod
expect "namespace is the destination" "$(sed -n 5p "$w/helm-1.args")" app-gate-api-prod
expect "value files, then the injected values last" "$(sed -n '6,$p' "$w/helm-1.args" | sed "s#$w/repo/##; s#$w/eval/hangar-prod__prod/head.yaml.injected.yaml#INJECTED#" | paste -sd' ' -)" \
  "-f hangar-prod/prod/values.yaml -f hangar-prod/prod/release.yaml -f INJECTED"
expect "the base renders with the same release name" "$(sed -n 2p "$w/helm-2.args")" gate-api-prod
expect "injected values equal the plugin's" \
  "$(yq -o=json -I0 'sort_keys(..)' "$w/eval/hangar-prod__prod/head.yaml.injected.yaml")" "$(jq -S -c . "$fx/injected.json")"
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
expect "observability: render exit" "$(cat "$w/render.rc")" 0
expect "observability is in the injected values, as the ApplicationSet gives it" \
  "$(yq -o=json -I0 'sort_keys(..)' "$w/eval/hangar-prod__prod/head.yaml.injected.yaml")" \
  "$(jq -S -c '. + {observability: {namespace: "observability", instrumentation: "observability/auto", runtime: "python"}}' "$fx/injected.json")"

# Per destination cluster: the fetch step's observability block, run on a two-cluster plan. The control-plane cluster
# (hangar-dev) takes glidepath-app's ConfigMap values; another cluster (hangar-prod) takes its OWN facts, as the broker
# read them from its cluster-defaults.yaml, plus the app's runtime; one that sets none injects nothing; one the broker
# could not read is not rendered at all (it would differ from Argo CD's).
w="$d/clusters"; mkdir -p "$w/observability" "$w/policies"
yq '.spec.steps[0].script' "$d/task.yaml" | sed "s#/work#$w#g" \
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
# ... and the render step refuses a marked cluster rather than render a tree Argo CD would not.
run unreadable-obs
w2="$d/unreadable-obs"; touch "$w2/observability/hangar-prod.yaml.unreadable"; rm -rf "$w2/eval"
( cd "$w2" && PATH="$d/bin:$PATH" APP_NAME=gate-api NAMESPACE=app-gate-api-cicd bash "$w2/step1.sh" ) > "$w2/render.log" 2>&1
expect "unreadable observability: the environment is render-failed" "$(cat "$w2/eval/hangar-prod__prod/status")" render-failed

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
rm -rf "$d/newenv/base/hangar-prod"; mkdir -p "$d/newenv/base"
( cd "$d/newenv" && rm -rf eval helm-*.args && PATH="$d/bin:$PATH" APP_NAME=gate-api NAMESPACE=app-gate-api-cicd bash "$d/newenv/step1.sh" ) > "$d/newenv/render.log" 2>&1
expect "new environment: all added" "$(jq -r '[.objects[].op] | unique | join(",")' "$(change newenv)")" added

[[ "${fail}" == 0 ]] && echo "PASS clearance_gate_parity_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
