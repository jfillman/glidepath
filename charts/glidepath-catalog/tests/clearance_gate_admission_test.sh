#!/usr/bin/env bash
# clearance-gate.yaml's render, admission and register steps (C-8: the offline admission verdict sent with the
# change) against the policy-validation fixtures (fixtures/policy-validation: a stand-in chart, Airframe XRD plurals
# and a cluster's 30-policy tree), with the real helm and Kyverno CLI. The fetch step (git, the GitHub API and the
# broker) is replaced by the /work layout it leaves, and curl by a stand-in Clearance that keeps what it was sent.
# Needs helm, yq, jq, python3 + pyyaml and kyverno (the toolbox's version) on PATH.
# Usage: charts/glidepath-catalog/tests/clearance_gate_admission_test.sh
set -u
cd "$(dirname "$0")/../../.."
command -v kyverno >/dev/null || { echo "FAIL: kyverno not on PATH"; exit 1; }
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/policy-validation"
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/clearance-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
mkdir -p "$d/bin"
step() { # <index> <work dir>
  yq ".spec.steps[$1].script" "$d/task.yaml" \
    | sed -e "s#/work#$2#g" -e "s#\$(results.outcome.path)#$2/outcome#g" -e "s#\$(results.findings-summary.path)#$2/summary#g" \
          -e "s#\$(results.change-ids.path)#$2/change-ids#g" -e "s#/var/run/secrets/clearance/clearance-token#$2/token#g" \
    | sed -e "s#/opt/airframe/charts/airframe-application#${fx}/chart#g" -e "s#/opt/airframe/xrds#${fx}/airframe/xrds#g" \
    > "$2/step$1.sh"
}
# The stand-in Clearance: keeps each body it is sent (posted-<n>.json) and answers as CLEARANCE_ANSWER says: "ok" a
# cleared change, "pre-c8" a 400 for the admission field on the first call (a Clearance before C-8), then ok.
cat > "$d/bin/curl" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do [[ "${prev:-}" == --data ]] && f="${a#@}"; prev="$a"; done
n=$(( $(ls "${WORK}"/posted-*.json 2>/dev/null | wc -l) + 1 ))
cp "${f}" "${WORK}/posted-${n}.json"
if [[ "${CLEARANCE_ANSWER}" == pre-c8 && "${n}" == 1 ]]; then
  printf '{"error":"unknown field %s"}\n400' "'admission'"; exit 0
fi
if [[ "${CLEARANCE_ANSWER}" == pre-c8-dq && "${n}" == 1 ]]; then  # the same refusal, Go-quoted
  printf '{"error":"json: unknown field \\"policyCommit\\""}\n400'; exit 0
fi
jq -c '{id: "ch-0123456789abcdef", state: "cleared", tier: 1, reasons: [], requirement: null, approvals: [], cleared_by: "policy",
        deny_reason: null, expires_at: "2026-11-10T00:00:00+00:00", admission: .admission}' "${f}"
printf '200'
EOF
chmod +x "$d/bin/curl"

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

commit=4b1d000000000000000000000000000000000007
policy_commit=9f2c0000000000000000000000000000000000e1
# <scenario> <app> <values.yaml content> [unreadable]: lays out what the fetch step leaves, runs render, admission
# and register.
run() {
  local s="$1" app="$2"
  export WORK="$d/$s"
  mkdir -p "$WORK/repo/hangar-prod/prod" "$WORK/base/hangar-prod/prod" "$WORK/policies/hangar-prod"
  cp "$fx/gitops/hangar-prod/prod/release.yaml" "$WORK/repo/hangar-prod/prod/"
  cp "$fx/gitops/hangar-prod/prod/release.yaml" "$WORK/base/hangar-prod/prod/"
  printf '%s\n' "$3" > "$WORK/repo/hangar-prod/prod/values.yaml"
  printf 'replicas: 1\n' > "$WORK/base/hangar-prod/prod/values.yaml"
  : > "$WORK/policies/unreadable"
  if [[ "${4:-}" == unreadable ]]; then
    echo hangar-prod > "$WORK/policies/unreadable"; rm -rf "$WORK/policies/hangar-prod"
  else
    cp -R "$fx/policies/30-policy/." "$WORK/policies/hangar-prod/"
    echo "{\"repoURL\":\"https://github.com/acme/gitops-hangar-prod.git\",\"path\":\"30-policy\",\"revision\":\"main\",\"commit\":\"${commit}\",\"files\":3}" \
      > "$WORK/policies/hangar-prod.source.json"
  fi
  echo '{"ttlDays": 3}' > "$WORK/clearance.json"
  jq -n --arg pc "${policy_commit}" '{owner: "acme", repo: "gitops-x", pr: 7, head: ("a" * 40), base: ("b" * 40), author: "dana",
    files: [{filename: "hangar-prod/prod/values.yaml", status: "modified", additions: 2, deletions: 1}], looksAgent: false,
    provenance: null, checkRuns: [], policyCommit: $pc}' > "$WORK/meta.json"
  echo "hangar-prod|prod|default|true" > "$WORK/plan.txt"
  echo token > "$WORK/token"
  step 1 "$WORK"; step 2 "$WORK"; step 3 "$WORK"
  ( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME="$app" NAMESPACE="app-${app}-cicd" bash "$WORK/step1.sh" ) > "$WORK/render.log" 2>&1
  echo $? > "$WORK/render.rc"
  ( cd "$WORK" && PATH="$d/bin:$PATH" bash "$WORK/step2.sh" ) > "$WORK/admission.log" 2>&1
  echo $? > "$WORK/admission.rc"
  ( cd "$WORK" && PATH="$d/bin:$PATH" APP_NAME="$app" NAMESPACE="app-${app}-cicd" CLEARANCE_URL=http://clearance CLEARANCE_ANSWER="${CLEARANCE_ANSWER:-ok}" \
      TOWER_URL="" PIPELINE_RUN="" bash "$WORK/step3.sh" ) > "$WORK/register.log" 2>&1
  echo $? > "$WORK/register.rc"
  [[ "$(cat "$WORK/render.rc")" == 0 ]] || { echo "-- ${s}: render step log:"; cat "$WORK/render.log"; }
}
posted() { jq -r "$2" "$d/$1/posted-${3:-1}.json"; }

# green: compliant values - the change carries a pass verdict, the engine's version and the rule set's commit.
run green green 'resources: {limits: {memory: 256Mi}}
serviceLabels: {team: skyport}
serviceType: ClusterIP'
w="$d/green"
expect "green: render exit" "$(cat "$w/render.rc")" 0
expect "green: admission exit" "$(cat "$w/admission.rc")" 0
expect "green: verdict" "$(posted green .admission.verdict)" pass
expect "green: engine is the CLI's version" "$(posted green .admission.engine)" "kyverno/$(kyverno version | sed -nE 's/^Version: *v?//p' | head -1)"
expect "green: rule set at its commit" "$(posted green .admission.bundle)" "https://github.com/acme/gitops-hangar-prod.git//30-policy@${commit}"
expect "green: counts" "$(posted green '"\(.admission.deny)/\(.admission.warn)"')" 0/0
expect "green: checks.admission is the verdict" "$(posted green .checks.admission)" pass
expect "green: clearance.yaml's commit" "$(posted green .policyCommit)" "${policy_commit}"
expect "green: clearance.yaml sent" "$(posted green .policy.ttlDays)" 3
has "green: the comment shows the verdict" "$w/report.md" '\| ✅ \| hangar-prod/prod \| 1 \| cleared \| pass \|'
expect "green: register exit" "$(cat "$w/register.rc")" 0

# warn: an Audit failure and an offline `error` are warnings, never a fail.
run warn warn 'resources: {limits: {memory: 256Mi}}'
expect "warn: verdict" "$(posted warn .admission.verdict)" warn
expect "warn: no deny" "$(posted warn .admission.deny)" 0
[[ "$(posted warn .admission.warn)" -ge 1 ]] && echo "ok   warn: warnings counted" || { echo "FAIL warn: warnings counted"; fail=1; }

# red: Deny violations are a fail verdict; the change is still registered (the policy-validation check fails on it).
run red red 'serviceType: NodePort
serviceLabels: {team: skyport}'
expect "red: verdict" "$(posted red .admission.verdict)" fail
expect "red: four deny findings" "$(posted red .admission.deny)" 4
expect "red: checks.admission" "$(posted red .checks.admission)" fail

# unreadable: the broker could not read the bundle - an error verdict, no rule set, still registered.
run unreadable unreadable 'resources: {limits: {memory: 256Mi}}' unreadable
expect "unreadable: verdict" "$(posted unreadable .admission.verdict)" error
expect "unreadable: no rule set" "$(posted unreadable '.admission.bundle // "none"')" none
expect "unreadable: register exit" "$(cat "$d/unreadable/register.rc")" 0

# pre-c8: a Clearance that refuses the admission field gets the change again without it (rollout order).
CLEARANCE_ANSWER=pre-c8 run pre-c8 green 'resources: {limits: {memory: 256Mi}}
serviceLabels: {team: skyport}
serviceType: ClusterIP'
expect "pre-c8: first body has the result" "$(posted pre-c8 .admission.verdict 1)" pass
expect "pre-c8: second body has neither field" "$(posted pre-c8 '[has("admission"), has("policyCommit")] | any' 2)" false
has "pre-c8: says why" "$d/pre-c8/register.log" 'does not take the admission result yet'
has "pre-c8: registered" "$d/pre-c8/report.md" 'cleared'

CLEARANCE_ANSWER=pre-c8-dq run pre-c8-dq green 'resources: {limits: {memory: 256Mi}}
serviceLabels: {team: skyport}
serviceType: ClusterIP'
expect "pre-c8-dq: a double-quoted refusal also falls back" "$(posted pre-c8-dq '[has("admission"), has("policyCommit")] | any' 2)" false

[[ "${fail}" == 0 ]] && echo "PASS clearance_gate_admission_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
