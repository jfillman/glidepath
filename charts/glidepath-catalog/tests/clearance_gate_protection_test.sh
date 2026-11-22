#!/usr/bin/env bash
# Which environments clearance-gate registers, and what Clearance's answer means for the PR comment (files/lib/protection.sh,
# docs/admin/clearance-gate.md "Which environments are protected"). The platform decides protection, never the app's cicd.yaml.
#   1. the decision itself: cicd.yaml's declaration (true | false | missing) x Clearance's answer (protected | unprotected | unknown)
#   2. the rendered Task carries the library and its fetch step decides with it, not with cicd.yaml's flag
#   3. the fetch step's own plan loop (cut from the rendered Task) on flight-api's cicd.yaml as it was for gitops-flight-api#48
#   4. #48 end to end: publish, render (real helm), ingest and register against a stand-in Clearance. It is registered and
#      held, with a drift warning, where it used to be skipped as "not a protected environment"
#   5. an app that already declares production: true (gate-api, backstage) is registered exactly as before, with no warning
#   6. fail closed: no answer from Clearance fails the check and the environment counts as protected
# Needs helm, yq (mikefarah), jq and python3 + pyyaml. Usage: charts/glidepath-catalog/tests/clearance_gate_protection_test.sh
set -u
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/../../.."
fxp="$(pwd)/charts/glidepath-catalog/tests/fixtures/clearance-protection"
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/policy-validation"
lib=charts/glidepath-catalog/files/lib/protection.sh
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
PROTECTION_LIB="$(cat "${lib}")"; export PROTECTION_LIB
# shellcheck source=../files/lib/protection.sh
source "${lib}"

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
has() { # <name> <file> <pattern> [absent]
  if grep -qE -- "$3" "$2" 2>/dev/null; then
    [[ "${4:-}" == absent ]] && { echo "FAIL $1: '$3' present"; fail=1; } || echo "ok   $1"
  else
    [[ "${4:-}" == absent ]] && echo "ok   $1" || { echo "FAIL $1: '$3' missing"; fail=1; }
  fi
}

# --- 1. the decision ------------------------------------------------------------------------------------------------------
envs='[{"name":"prod","production":true},{"name":"staging","production":false},{"name":"dev"},{"name":"qa","production":"true"},{"name":"odd","production":"yes"}]'
expect "declared: production: true" "$(protection_declared "${envs}" prod)" true
expect "declared: production: false" "$(protection_declared "${envs}" staging)" false
expect "declared: no production key" "$(protection_declared "${envs}" dev)" missing
expect "declared: the string \"true\"" "$(protection_declared "${envs}" qa)" true
expect "declared: any other value is not true" "$(protection_declared "${envs}" odd)" false
expect "declared: an environment cicd.yaml does not list" "$(protection_declared "${envs}" perf)" missing
expect "declared: no cicd.yaml at all" "$(protection_declared '[]' prod)" missing
expect "declared: cicd.yaml that is not a list" "$(protection_declared '{"prod":{"production":true}}' prod)" missing
expect "declared: unreadable JSON" "$(protection_declared 'not json' prod)" missing

# Registered: every environment with a Clearance, whatever cicd.yaml says; without one, only production: true (which fails: no Clearance).
for decl in true false missing; do
  expect "register: Clearance configured, declared ${decl}" "$(protection_register "${decl}" http://clearance)" true
done
expect "register: no Clearance, declared true" "$(protection_register true "")" true
expect "register: no Clearance, declared false" "$(protection_register false "")" false
expect "register: no Clearance, declared missing" "$(protection_register missing "")" false

expect "platform: protected: true" "$(protection_platform <<<'{"protected":true,"reasons":[]}')" protected
expect "platform: protected: false" "$(protection_platform <<<'{"protected":false,"reasons":[]}')" unprotected
expect "platform: the protected field wins over reasons" "$(protection_platform <<<'{"protected":true,"reasons":[{"rule":"unprotected"}]}')" protected
expect "platform: no field, an unprotected reason" "$(protection_platform <<<'{"reasons":[{"where":"c/e","rule":"unprotected","tier":0}]}')" unprotected
expect "platform: no field, other reasons" "$(protection_platform <<<'{"reasons":[{"rule":"image"}]}')" unknown
expect "platform: an empty object" "$(protection_platform <<<'{}')" unknown
expect "platform: not JSON" "$(protection_platform <<<'Bad Gateway')" unknown
expect "platform: nothing" "$(protection_platform </dev/null)" unknown

# The full matrix: declared x platform -> {protected, warning}. Unknown is protected; any disagreement is a warning.
verdict() { protection_verdict hangar-prod/prod "$1" "$2" | jq -r '"\(.protected) \(if .warning then "warn" else "none" end)"'; }
expect "verdict: declared true,    platform protected"   "$(verdict true protected)"      "true none"
expect "verdict: declared false,   platform protected"   "$(verdict false protected)"     "true warn"
expect "verdict: declared missing, platform protected"   "$(verdict missing protected)"   "true warn"
expect "verdict: declared true,    platform unprotected" "$(verdict true unprotected)"    "false warn"
expect "verdict: declared false,   platform unprotected" "$(verdict false unprotected)"   "false none"
expect "verdict: declared missing, platform unprotected" "$(verdict missing unprotected)" "false none"
expect "verdict: declared true,    platform unknown"     "$(verdict true unknown)"        "true warn"
expect "verdict: declared false,   platform unknown"     "$(verdict false unknown)"       "true warn"
expect "verdict: declared missing, platform unknown"     "$(verdict missing unknown)"     "true warn"
w="$(protection_verdict hangar-prod/prod missing protected | jq -r .warning)"
[[ "${w}" == *'Clearance protects `hangar-prod/prod`, but cicd.yaml does not say `production: true`'* ]] && echo "ok   verdict: drift names the environment and the missing flag" \
  || { echo "FAIL verdict: drift text: ${w}"; fail=1; }
w="$(protection_verdict hangar-prod/prod true unprotected | jq -r .warning)"
[[ "${w}" == *'Clearance does not protect it'*'tier 0'* ]] && echo "ok   verdict: an app's production: true the platform does not back says so" \
  || { echo "FAIL verdict: unbacked declaration text: ${w}"; fail=1; }

# --- 2. the rendered Task -------------------------------------------------------------------------------------------------
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/clearance-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/render-manifests.yaml > "$d/rtask.yaml" || { echo "FAIL: render (render-manifests)"; exit 1; }
for st in fetch register; do
  yq ".spec.steps[] | select(.name == \"${st}\") | .env[] | select(.name == \"PROTECTION_LIB\") | .value" "$d/task.yaml" > "$d/${st}.lib"
  if [[ "$(cat "$d/${st}.lib")" == "${PROTECTION_LIB}" ]]; then echo "ok   render: ${st} carries files/lib/protection.sh in its environment"
  else echo "FAIL render: ${st}'s PROTECTION_LIB is not files/lib/protection.sh"; fail=1; fi
done
yq '.spec.steps[] | select(.name == "fetch") | .script' "$d/task.yaml" > "$d/fetch.sh"
has "render: fetch decides with the library" "$d/fetch.sh" 'protected="\$\(protection_register "\$\{declared\}" "\$\{CLEARANCE_URL\}"\)"'
has "render: fetch no longer reads production as the filter" "$d/fetch.sh" '\.production // false' absent
has "render: fetch records the declaration for the register step" "$d/fetch.sh" '>> /work/declared.txt'

# --- 3. the fetch step's plan loop ----------------------------------------------------------------------------------------
# The loop that writes plan.txt, cut from the rendered fetch step (no git, no API: an environment on the default chart).
awk '/^mkdir -p \/work\/charts$/,/^done < \/work\/envs.txt$/' "$d/fetch.sh" > "$d/plan-loop.sh"
[[ -s "$d/plan-loop.sh" ]] || { echo "FAIL: the plan loop was not found in the fetch step"; exit 1; }
plan() { # <scenario dir> <cicd.yaml> <clearance url> <cluster|env>...: runs the loop; leaves plan.txt and declared.txt
  local W="$1" f="$2" url="$3"; shift 3
  rm -rf "${W}"; mkdir -p "${W}"; export FETCHED="${W}/fetched"; : > "${FETCHED}"
  printf '%s\n' "$@" > "${W}/envs.txt"; : > "${W}/plan.txt"; : > "${W}/declared.txt"
  sed "s#/work#${W}#g" "$d/plan-loop.sh" > "${W}/loop.sh"
  ( cd "${W}" && cicd="$(cat "${f}")" CLEARANCE_URL="${url}" DEFAULT_CHART_REPO_URL=https://github.com/acme/airframe DEFAULT_CHART_PATH=charts/airframe-application DEFAULT_CHART_REVISION=v0.0.0 \
      bash -c 'set -euo pipefail; eval "${PROTECTION_LIB}"; envs_json="$(echo "${cicd}" | yq -o json ".deploy.environments // []")"
        valid_chart_source() { return 0; }; fetch_repo() { mkdir -p "$3"; echo "fetched $1@$2" >> "${FETCHED}"; }  # no network: a chart fetch is recorded
        source "$1"' _ "${W}/loop.sh" ) > "${W}/plan.log" 2>&1
  echo $? > "${W}/plan.rc"
}
plan "$d/p48" "${fxp}/flight-api-before-45.cicd.yaml" http://clearance hangar-prod\|prod
expect "#48 plan: the loop ran" "$(cat "$d/p48/plan.rc")" 0
expect "#48 plan: prod is registered though cicd.yaml does not mark it" "$(cat "$d/p48/plan.txt")" "hangar-prod|prod|default|true"
expect "#48 plan: the declaration is kept for the comment" "$(cat "$d/p48/declared.txt")" "hangar-prod|prod|missing"
plan "$d/p48n" "${fxp}/flight-api-before-45.cicd.yaml" "" hangar-prod\|prod
expect "no Clearance: an unmarked environment is still skipped (nothing could gate it)" "$(cat "$d/p48n/plan.txt")" "hangar-prod|prod|default|false"
plan "$d/pga" "${fxp}/gate-api.cicd.yaml" http://clearance hangar-prod\|prod hangar-prod\|staging
expect "gate-api plan: prod and staging are both registered" "$(cut -d'|' -f1,2,4 "$d/pga/plan.txt" | paste -sd ' ' -)" "hangar-prod|prod|true hangar-prod|staging|true"
expect "gate-api plan: the declarations" "$(paste -sd ' ' "$d/pga/declared.txt")" "hangar-prod|prod|true hangar-prod|staging|missing"
plan "$d/pgan" "${fxp}/gate-api.cicd.yaml" "" hangar-prod\|prod
expect "no Clearance: a production: true environment is registered (and fails: no Clearance)" "$(cat "$d/pgan/plan.txt")" "hangar-prod|prod|default|true"
# An environment deploy.environments does not list is registered too, so it must render with the app's deploy.chart, not the default.
printf 'deploy:\n  chart: {repoURL: https://github.com/acme/charts, path: app, targetRevision: v1}\n  environments:\n    - name: prod\n      chart: {targetRevision: v2}\n' > "$d/own-chart.cicd.yaml"
plan "$d/pown" "$d/own-chart.cicd.yaml" http://clearance hangar-prod\|prod hangar-prod\|perf
expect "own chart: the loop ran" "$(cat "$d/pown/plan.rc")" 0
expect "own chart: a listed environment keeps its own revision" "$(sed -n 1p "$d/pown/fetched")" "fetched https://github.com/acme/charts@v2"
expect "own chart: an unlisted environment gets deploy.chart" "$(sed -n 2p "$d/pown/fetched")" "fetched https://github.com/acme/charts@v1"
expect "own chart: and is planned with it" "$(grep '^hangar-prod|perf|' "$d/pown/plan.txt" | cut -d'|' -f3,4)" "$d/pown/charts/2/app|true"

# --- 4-6. render, ingest and register ---------------------------------------------------------------------------------------
mkdir -p "$d/bin"
source "${here}/lib/gate_chain.sh"
# The stand-in Clearance: keeps each body (posted-<n>.json) and answers as CLEARANCE_ANSWER says, with `protected` as the core decided.
cat > "$d/bin/curl" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do [[ "${prev:-}" == --data ]] && f="${a#@}"; prev="$a"; done
n=$(( $(ls "${WORK}"/posted-*.json 2>/dev/null | wc -l) + 1 ))
cp "${f}" "${WORK}/posted-${n}.json"
case "${CLEARANCE_ANSWER}" in
  down) printf 'upstream unavailable\n503'; exit 0 ;;
  holding) jq -c '{id: "ch-48", state: "holding", tier: 2, protected: true, reasons: [{where: "Rollout/flight-api", rule: "runtime-shape", tier: 2}],
                   requirement: {owner: 1, ownerGroup: "group:default/flight-api", outside: 0, outsideGroups: []}, approvals: [], cleared_by: null,
                   deny_reason: null, expires_at: "2026-11-29T00:00:00+00:00", tree_digest: .treeDigest, pr: .pr}' "${f}" ;;
  cleared) jq -c '{id: "ch-1", state: "cleared", tier: 1, protected: true, reasons: [{where: "Rollout/x", rule: "image", tier: 1}], requirement: null,
                   approvals: [], cleared_by: "policy", deny_reason: null, expires_at: "2026-11-29T00:00:00+00:00", tree_digest: .treeDigest, pr: .pr}' "${f}" ;;
  unprotected) jq -c '{id: "ch-0", state: "cleared", tier: 0, protected: false, reasons: [{where: "\(.cluster)/\(.env)", rule: "unprotected", tier: 0}],
                   requirement: null, approvals: [], cleared_by: "policy", deny_reason: null, expires_at: "2026-11-29T00:00:00+00:00", tree_digest: .treeDigest, pr: .pr}' "${f}" ;;
esac
printf '200'
EOF
chmod +x "$d/bin/curl"

# <scenario> <app> <plan dir from section 3>: lays out what the fetch step leaves (a release.yaml change to hangar-prod/prod),
# then publish, render and ingest.
chain() {
  local s="$1" app="$2" pd="$3"
  export WORK="$d/$s"
  local P="$WORK/pub"
  rm -rf "${WORK}"; mkdir -p "$P/repo/hangar-prod/prod" "$P/base/hangar-prod/prod" "$P/observability" "$P/policies"
  : > "$P/observability/hangar-prod.yaml"; : > "$P/policies/unreadable"
  cp "$fx/gitops/hangar-prod/prod/release.yaml" "$P/repo/hangar-prod/prod/"
  sed 's/tag: .*/tag: "2.0.13"/' "$fx/gitops/hangar-prod/prod/release.yaml" > "$P/base/hangar-prod/prod/release.yaml"
  printf 'replicas: 1\n' > "$P/repo/hangar-prod/prod/values.yaml"; cp "$P/repo/hangar-prod/prod/values.yaml" "$P/base/hangar-prod/prod/"
  jq -n '{owner: "acme", repo: "gitops-x", pr: 48, head: ("a" * 40), base: ("b" * 40), author: "glidepath[bot]",
    files: [{filename: "hangar-prod/prod/release.yaml", status: "modified", additions: 1, deletions: 1}], looksAgent: false,
    provenance: null, checkRuns: [], policyCommit: ""}' > "$P/meta.json"
  grep '^hangar-prod|prod|' "${pd}/plan.txt" > "$P/plan.txt"; grep '^hangar-prod|prod|' "${pd}/declared.txt" > "$P/declared.txt"
  gate_chain "$d/task.yaml" "$d/rtask.yaml" clearance-gate "$WORK" "$app"
  [[ "$(cat "$WORK/render.rc")$(cat "$WORK/ingest.rc")" == 00 ]] || { echo "-- ${s}: render and ingest log:"; cat "$WORK/chain.log"; }
  gate_sstep "$d/task.yaml" register | gate_loc "$WORK" x x x x \
    | sed -e "s#\$(results.joins.path)#${WORK}/joins#g" -e "s#/var/run/secrets/clearance/clearance-token#${WORK}/token#g" > "$WORK/register.sh"
  echo token > "$WORK/token"
}
register() { # <scenario> <app> <answer> [clearance url]
  local W="$d/$1"; export WORK="${W}"
  rm -f "${W}"/posted-*.json "${W}/report.md" "${W}/outcome" "${W}/summary"
  ( cd "${W}" && PATH="$d/bin:$PATH" APP_NAME="$2" NAMESPACE="app-$2-cicd" CLEARANCE_URL="${4-http://clearance}" CLEARANCE_ANSWER="$3" \
      TOWER_URL="" PIPELINE_RUN="" REPO_URL=https://github.com/acme/gitops-x bash "${W}/register.sh" ) > "${W}/register.log" 2>&1
  echo $? > "${W}/register.rc"
}

# 4. gitops-flight-api#48: the release to hangar-prod/prod, cicd.yaml without production: true, Clearance protects it (hangar-prod/*).
chain f48 flight-api "$d/p48"
w="$d/f48"
expect "#48: rendered and read" "$(cat "$w/ingest.rc")" 0
[[ -f "$w/eval/hangar-prod__prod/change.json" && ! -f "$w/eval/hangar-prod__prod/status" ]] && echo "ok   #48: a change was built (not marked skipped)" \
  || { echo "FAIL #48: no change for hangar-prod/prod"; cat "$w/chain.log"; fail=1; }
has "#48: the old skip line is gone" "$w/chain.log" 'not a protected environment' absent
register f48 flight-api holding
expect "#48: registered with Clearance" "$(jq -r '"\(.app) \(.cluster)/\(.env) protected=\(.protected)"' "$w/posted-1.json" 2>/dev/null)" "flight-api hangar-prod/prod protected=true"
expect "#48: outcome is Clearance's (holding), not skipped" "$(cat "$w/outcome")" holding
has "#48: the comment shows the holding row" "$w/report.md" '\| ⏳ \| hangar-prod/prod \| 2 \| holding \|'
has "#48: the comment warns about the drift" "$w/report.md" '> \*\*Warning:\*\* Clearance protects `hangar-prod/prod`, but cicd.yaml does not say `production: true`'
has "#48: the headline counts the warning" "$w/summary" '1 protection warning\(s\): cicd.yaml and Clearance disagree'
# Before the fix (and still, on a cluster with no Clearance): the same PR is skipped.
chain f48n flight-api "$d/p48n"
expect "#48 without Clearance: ingest marks it skipped" "$(cat "$d/f48n/eval/hangar-prod__prod/status" 2>/dev/null)" skipped
register f48n flight-api holding ""
expect "#48 without Clearance: outcome skipped" "$(cat "$d/f48n/outcome")" skipped
expect "#48 without Clearance: nothing was sent" "$(ls "$d/f48n"/posted-*.json 2>/dev/null | wc -l | tr -d ' ')" 0

# 5. gate-api's prod (production: true), protected: registered as before, no warning, no protection paragraph.
chain gprod gate-api "$d/pga"
register gprod gate-api cleared
w="$d/gprod"
expect "flagged: outcome cleared" "$(cat "$w/outcome")" cleared
has "flagged: the row as before" "$w/report.md" '\| ✅ \| hangar-prod/prod \| 1 \| cleared \|'
has "flagged: no protection paragraph" "$w/report.md" 'Protection:' absent
has "flagged: no warning in the headline" "$w/summary" 'protection warning' absent
expect "flagged: the same body as before (protected: true)" "$(jq -r .protected "$w/posted-1.json")" true
# The app says production: true, the platform does not protect it: cleared at tier 0, with a warning (the flag cannot protect it alone).
register gprod gate-api unprotected
expect "declared but unprotected: cleared" "$(cat "$w/outcome")" cleared
has "declared but unprotected: warned" "$w/report.md" 'cicd.yaml says `production: true` for `hangar-prod/prod`, but Clearance does not protect it'
# An unmarked environment the platform does not protect: cleared at tier 0, recorded, no warning (they agree).
register f48 flight-api unprotected
expect "unmarked and unprotected: cleared" "$(cat "$d/f48/outcome")" cleared
has "unmarked and unprotected: no warning" "$d/f48/report.md" 'Warning:' absent

# 6. fail closed: Clearance does not answer. The check fails and the environment counts as protected.
register f48 flight-api down
w="$d/f48"
expect "no answer: outcome fail" "$(cat "$w/outcome")" fail
has "no answer: the row is an error" "$w/report.md" '\| ❌ \| hangar-prod/prod \|  \| error: HTTP 503'
expect "no answer: counted as protected" "$(jq -r 'select(.env == "hangar-prod/prod") | .protected' "$w/protection.jsonl")" true
has "no answer: says it was treated as protected" "$w/report.md" 'Clearance did not say whether `hangar-prod/prod` is protected, so it is treated as protected'
# And the report step turns it red.
gate_sstep "$d/task.yaml" report | gate_loc "$w" x x x x > "$w/report.sh"
( cd "$w" && bash "$w/report.sh" ) > "$w/report.log" 2>&1
expect "no answer: the check is red" "$?" 1

[[ "${fail}" == 0 ]] && echo "PASS clearance_gate_protection_test" || { echo "FAILED (KEEP=1 keeps ${d})"; exit 1; }
