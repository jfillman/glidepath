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
  : > "$WORK/observability.yaml"; [[ -n "${2:-}" ]] && printf '%s\n' "$2" > "$WORK/observability.yaml"
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

# The ConfigMap the render reads is glidepath-app's own observabilityValues block (what the ApplicationSets inject), and
# is absent when the cluster sets none.
app=(helm template t charts/glidepath-app --set devClusterName=d --set build.x=1 --set platformIdentity.appName=a --set platformIdentity.runtime=python
     --set defaultChart.repoURL=https://x --set defaultChart.targetRevision=v1 --show-only templates/identity/observability-values.yaml)
expect "glidepath-app writes the observability ConfigMap" \
  "$("${app[@]}" --set observability.namespace=obs --set observability.instrumentation=otel/x | yq '.data."values.yaml"' | yq -o=json -I0 .)" \
  '{"observability":{"namespace":"obs","instrumentation":"otel/x","runtime":"python"}}'
expect "and none when the cluster sets no observability" "$("${app[@]}" 2>&1 | grep -c 'could not find template')" 1

# A new environment (no base tree) lists every object as added.
run newenv
rm -rf "$d/newenv/base/hangar-prod"; mkdir -p "$d/newenv/base"
( cd "$d/newenv" && rm -rf eval helm-*.args && PATH="$d/bin:$PATH" APP_NAME=gate-api NAMESPACE=app-gate-api-cicd bash "$d/newenv/step1.sh" ) > "$d/newenv/render.log" 2>&1
expect "new environment: all added" "$(jq -r '[.objects[].op] | unique | join(",")' "$(change newenv)")" added

[[ "${fail}" == 0 ]] && echo "PASS clearance_gate_parity_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
