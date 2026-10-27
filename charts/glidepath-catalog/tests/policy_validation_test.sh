#!/usr/bin/env bash
# policy-validation-gate.yaml's render and evaluate steps against fixtures (fixtures/policy-validation): a stand-in
# chart, Airframe XRD plurals and a cluster's 30-policy tree with every policy shape the gate reads (Kyverno
# ValidatingPolicy Deny/Audit, admission-only, namespaceSelector, a ClusterPolicy, native VAPs + bindings), with the
# real helm and Kyverno CLI. The fetch step (git and the GitHub API) is replaced by the /work layout it leaves.
# Needs helm, yq, jq, python3 + pyyaml and kyverno (the toolbox's version) on PATH.
# Usage: charts/glidepath-catalog/tests/policy_validation_test.sh
set -u
cd "$(dirname "$0")/../../.."
command -v kyverno >/dev/null || { echo "FAIL: kyverno not on PATH"; exit 1; }
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/policy-validation"
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/policy-validation-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
mkdir -p "$d/bin"
# Paths the steps hardcode, the shared volume and the validator image's Airframe tree, replaced per scenario (the
# render step's Python is a quoted heredoc, so a shell variable would not reach it).
step() { # <index> <work dir>
  yq ".spec.steps[$1].script" "$d/task.yaml" \
    | sed -e "s#/opt/airframe/charts/airframe-application#${fx}/chart#g" -e "s#/opt/airframe/xrds#${fx}/airframe/xrds#g" \
          -e "s#/work#$2#g" -e "s#\$(results.outcome.path)#$2/outcome#g" -e "s#\$(results.findings-summary.path)#$2/summary#g" \
    > "$2/step$1.sh"
}
cat > "$d/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"create configmap"*) for a in "$@"; do [[ "$a" == --from-file=body=* ]] && f="${a#--from-file=body=}"; done
    jq -n --rawfile b "$f" '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"x"},data:{body:$b}}' ;;
  apply*) cat > "${WORK}/applied.json" ;;
  *pipelinerun*) echo uid-1 ;;
esac
EOF
chmod +x "$d/bin/kubectl"

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
has() { # <name> <file> <pattern> [absent]
  if grep -qE -- "$3" "$2"; then
    [[ "${4:-}" == absent ]] && { echo "FAIL $1: '$3' present"; fail=1; } || echo "ok   $1"
  else
    [[ "${4:-}" == absent ]] && echo "ok   $1" || { echo "FAIL $1: '$3' missing"; fail=1; }
  fi
}

# <scenario> <app> <values.yaml content>: lays out what the fetch step leaves, runs render + evaluate.
run() {
  local s="$1" app="$2"
  export WORK="$d/$s"
  mkdir -p "$WORK/repo/hangar-prod/prod" "$WORK/policies/hangar-prod"
  cp "$fx/gitops/hangar-prod/prod/release.yaml" "$WORK/repo/hangar-prod/prod/"
  printf '%s\n' "$3" > "$WORK/repo/hangar-prod/prod/values.yaml"
  cp -R "$fx/policies/30-policy/." "$WORK/policies/hangar-prod/"
  echo '{"repoURL":"https://github.com/acme/gitops-hangar-prod.git","path":"30-policy","revision":"main"}' > "$WORK/policies/hangar-prod.source.json"
  echo "hangar-prod|prod|default" > "$WORK/plan.txt"
  step 1 "$WORK"; step 2 "$WORK"
  ( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME="$app" NAMESPACE="app-${app}-cicd" bash "$WORK/step1.sh" ) > "$WORK/render.log" 2>&1
  echo $? > "$WORK/render.rc"
  ( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME="$app" NAMESPACE="app-${app}-cicd" PIPELINE_RUN=pr-1 bash "$WORK/step2.sh" ) > "$WORK/eval.log" 2>&1
  echo $? > "$WORK/eval.rc"
}

# green: compliant values - every policy passes or does not apply.
run green green 'resources: {limits: {memory: 256Mi}}
serviceLabels: {team: skyport}
serviceType: ClusterIP'
w="$d/green"
expect "green: render exit" "$(cat "$w/render.rc")" 0
has "green: VAP bindings converted, VAP objects and the Application ignored" "$w/render.log" 'hangar-prod: 7 admission policies to evaluate: clusterip-only, locked-namespaces, no-exec, no-nodeport, no-redis-when-red, require-memory-limit, service-team-label'
expect "green: outcome" "$(cat "$w/outcome")" pass
expect "green: exit" "$(cat "$w/eval.rc")" 0
expect "green: Redis plural from the XRD" "$(yq '.metadata.name' "$w/eval/hangar-prod__prod/crds.yaml" | grep -c '^redis.catalog.hangar.io$')" 1
expect "green: Rollout plural guessed" "$(yq '.metadata.name' "$w/eval/hangar-prod__prod/crds.yaml" | grep -c '^rollouts.argoproj.io$')" 1
expect "green: namespace labels for namespaceSelector" "$(yq '.namespaceSelector[0].labels["kubernetes.io/metadata.name"]' "$w/eval/hangar-prod__prod/values.yaml")" app-green-prod
has "green: report ConfigMap labelled" "$w/applied.json" '"glidepath.io/gate-report": "policy-validation"'
has "green: report names the policy source" "$w/report.md" 'https://github.com/acme/gitops-hangar-prod.git `30-policy` @ main'

# warn: Audit failure (no team label) and an offline `error` (spec.type unset) - warnings, exit 0.
run warn warn 'resources: {limits: {memory: 256Mi}}'
w="$d/warn"
expect "warn: outcome" "$(cat "$w/outcome")" warn
expect "warn: exit" "$(cat "$w/eval.rc")" 0
has "warn: Audit policy is a warning" "$w/report.md" '\| ⚠️ \| hangar-prod/prod \| ValidatingPolicy/service-team-label \|.*Service/warn'
has "warn: undecidable VAP rule is a warning" "$w/report.md" '\| ⚠️ \| hangar-prod/prod \| ValidatingAdmissionPolicy/clusterip-only \(binding clusterip-only\)'
has "warn: no deny row" "$w/report.md" '\| ❌ \|' absent

# red: no memory limit, a NodePort Service, a Redis in app-red-prod - four Deny violations, exit 1.
run red red 'serviceType: NodePort
serviceLabels: {team: skyport}'
w="$d/red"
expect "red: outcome" "$(cat "$w/outcome")" fail
expect "red: exit" "$(cat "$w/eval.rc")" 1
has "red: Deny ValidatingPolicy on the Rollout (admission-only, request.namespace)" "$w/report.md" '\| ❌ \| hangar-prod/prod \| ValidatingPolicy/require-memory-limit \|.*Rollout/red \| every container needs a memory limit'
has "red: VAP binding Deny on the Service" "$w/report.md" '\| ❌ \|.*ValidatingAdmissionPolicy/clusterip-only.*Service/red'
has "red: ClusterPolicy Enforce" "$w/report.md" '\| ❌ \|.*ClusterPolicy/no-nodeport.*Service/red'
has "red: namespaceSelector match on a custom kind" "$w/report.md" '\| ❌ \|.*ValidatingPolicy/no-redis-when-red.*Redis/redis'
has "red: a VAP for pods/exec does not apply to other kinds" "$w/report.md" 'no-exec' absent
has "red: a namespaceSelector that does not match is not applied" "$w/report.md" 'locked-namespaces' absent
has "red: headline" "$w/report.md" '^### ❌ `policy-validation`: 4 violation\(s\) the target cluster would refuse'

# broken: a chart that does not render fails the task, never passes.
export WORK="$d/broken"; mkdir -p "$WORK/repo" "$WORK/policies"; echo "hangar-prod|prod|/nonexistent-chart" > "$WORK/plan.txt"
step 1 "$WORK"; step 2 "$WORK"
( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME=x NAMESPACE=app-x-cicd bash "$WORK/step1.sh" ) > "$WORK/render.log" 2>&1; echo $? > "$WORK/render.rc"
( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME=x NAMESPACE=app-x-cicd PIPELINE_RUN="" bash "$WORK/step2.sh" ) > "$WORK/eval.log" 2>&1; echo $? > "$WORK/eval.rc"
expect "broken: render step fails" "$(cat "$WORK/render.rc")" 1
expect "broken: outcome" "$(cat "$WORK/outcome")" fail
expect "broken: exit" "$(cat "$WORK/eval.rc")" 1
has "broken: says it could not evaluate" "$WORK/eval.log" 'could not evaluate 1 environment'

# skipped: nothing changed.
export WORK="$d/skipped"; mkdir -p "$WORK"; : > "$WORK/plan.txt"; step 2 "$WORK"
( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME=x NAMESPACE=app-x-cicd PIPELINE_RUN="" bash "$WORK/step2.sh" ) > "$WORK/eval.log" 2>&1; echo $? > "$WORK/eval.rc"
expect "skipped: outcome" "$(cat "$WORK/outcome")" skipped
expect "skipped: exit" "$(cat "$WORK/eval.rc")" 0

[[ "${fail}" == 0 ]] && echo "PASS policy_validation_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
