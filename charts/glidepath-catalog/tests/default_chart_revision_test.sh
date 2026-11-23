#!/usr/bin/env bash
# The default chart is the cluster's default chart AT ITS PINNED REVISION (W-49; glidepath#289 review). The gates render an environment
# on the default chart with the chart baked into the validator image (/opt/airframe/charts), so an environment that names the default
# repository and path at another targetRevision is an own chart: fetched at that revision and rendered with it, like any other.
#   1. one source of truth: the validator image's tag and the gates' DEFAULT_CHART_REVISION are both defaultChart.targetRevision;
#      an override image (valuesValidatorImage) must declare the revision it bakes (valuesValidatorChartRevision), equal to it
#   2. clearance-gate and policy-validation-gate: the fetch step's plan loop (cut from the rendered Task) per chart spec
#   3. validate-values: the same spec decides whether a values file is validated by the image or rendered with its own chart
# Needs helm, yq (mikefarah), jq and python3. Usage: charts/glidepath-catalog/tests/default_chart_revision_test.sh
set -u
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/../../.."
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
vals=charts/glidepath-catalog/tests/values.example.yaml
REPO="$(yq '.defaultChart.repoURL' "${vals}")"; CPATH="$(yq '.defaultChart.path' "${vals}")"; REV="$(yq '.defaultChart.targetRevision' "${vals}")"
PROTECTION_LIB="$(cat charts/glidepath-catalog/files/lib/protection.sh)"; export PROTECTION_LIB

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
render() { # <out> <task> [helm args...]
  local out="$1" task="$2"; shift 2
  helm template t charts/glidepath-catalog -f "${vals}" "$@" --show-only "templates/tasks/${task}.yaml" > "${out}" 2> "${out}.err"
}

# --- 1. one source of truth -------------------------------------------------------------------------------------------------
for t in clearance-gate policy-validation-gate validate-values render-manifests; do
  render "$d/${t}.yaml" "${t}" || { echo "FAIL render ${t}: $(cat "$d/${t}.yaml.err")"; exit 1; }
  img="$(yq '.spec.params[] | select(.name == "validator-image") | .default' "$d/${t}.yaml")"
  expect "${t}: the validator image is tagged with defaultChart.targetRevision" "${img}" "$(yq '.valuesValidatorRepository' "${vals}"):${REV}"
  [[ "${t}" == render-manifests ]] && continue
  expect "${t}: the fetch step compares against the same revision" \
    "$(yq '[.spec.steps[].env[]? | select(.name == "DEFAULT_CHART_REVISION") | .value] | unique | .[]' "$d/${t}.yaml")" "${REV}"
done
render "$d/o1.yaml" clearance-gate --set valuesValidatorImage=registry.example/own-validator:fips
expect "override image without a declared revision: the render fails" "$?" 1
grep -q 'valuesValidatorChartRevision must be set with valuesValidatorImage' "$d/o1.yaml.err" && echo "ok   ... and says what to declare" \
  || { echo "FAIL override undeclared: $(cat "$d/o1.yaml.err")"; fail=1; }
render "$d/o2.yaml" clearance-gate --set valuesValidatorImage=registry.example/own-validator:fips --set valuesValidatorChartRevision=v9.9.9
expect "override image baking another revision: the render fails" "$?" 1
grep -q "valuesValidatorChartRevision (v9.9.9) is not defaultChart.targetRevision (${REV})" "$d/o2.yaml.err" && echo "ok   ... and names both revisions" \
  || { echo "FAIL override mismatch: $(cat "$d/o2.yaml.err")"; fail=1; }
render "$d/o3.yaml" clearance-gate --set valuesValidatorImage=registry.example/own-validator:fips --set valuesValidatorChartRevision="${REV}"
expect "override image declaring the default revision: renders" "$?" 0
expect "... with the override image" "$(yq '.spec.params[] | select(.name == "validator-image") | .default' "$d/o3.yaml")" registry.example/own-validator:fips

# --- 2. the gates' plan loops -------------------------------------------------------------------------------------------------
yq '.spec.steps[] | select(.name == "fetch") | .script' "$d/clearance-gate.yaml" | awk '/^mkdir -p \/work\/charts$/,/^done < \/work\/envs.txt$/' > "$d/cg-loop.sh"
yq '.spec.steps[] | select(.name == "fetch") | .script' "$d/policy-validation-gate.yaml" | awk '/^mkdir -p \/work\/charts \/work\/policies$/,/^done < \/work\/envs.txt$/' > "$d/pv-loop.sh"
[[ -s "$d/cg-loop.sh" && -s "$d/pv-loop.sh" ]] || { echo "FAIL: a plan loop was not found in a fetch step"; exit 1; }
plan() { # <loop> <scenario> <cicd.yaml text>: runs the loop for hangar-prod/prod and hangar-prod/qa; prints "<env>=<chart>[@<fetched rev>]"
  local loop="$1" W="$d/$2"
  mkdir -p "${W}"; printf '%s\n' "$3" > "${W}/cicd.yaml"; printf 'hangar-prod|prod\nhangar-prod|qa\n' > "${W}/envs.txt"; : > "${W}/plan.txt"; : > "${W}/fetched"
  sed "s#/work#${W}#g" "${loop}" > "${W}/loop.sh"
  ( cd "${W}" && cicd="$(cat cicd.yaml)" CLEARANCE_URL=http://clearance DEFAULT_CHART_REPO_URL="${REPO}" DEFAULT_CHART_PATH="${CPATH}" DEFAULT_CHART_REVISION="${REVOVR-${REV}}" \
      FETCHED="${W}/fetched" bash -c 'set -euo pipefail; eval "${PROTECTION_LIB}"; envs_json="$(echo "${cicd}" | yq -o json ".deploy.environments // []" 2>/dev/null || true)"
        valid_chart_source() { return 0; }; fetch_repo() { mkdir -p "$3"; echo "$1@$2" >> "${FETCHED}"; }  # no network: a fetch is recorded
        source "$1"' _ "${W}/loop.sh" ) > "${W}/plan.log" 2>&1; local rc=$?
  echo "${rc}" > "${W}/rc"
  if (( rc != 0 )) && [[ -z "${EXPECT_FAIL:-}" ]]; then echo "FAIL $2: the loop failed: $(cat "${W}/plan.log")"; fail=1; fi
  cut -d'|' -f2,3 "${W}/plan.txt" | sed "s#${W}/charts/#own:#; s#|#=#" | paste -sd ' ' -
  [[ -s "${W}/fetched" ]] && echo "fetched $(paste -sd ' ' "${W}/fetched")"
}
for loop in cg pv; do
  expect "${loop}: no chart spec: the default chart" "$(plan "$d/${loop}-loop.sh" "${loop}-none" 'deploy: {}')" "prod=default qa=default"
  expect "${loop}: the default revision named: the default chart" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-same" "deploy: {chart: {targetRevision: ${REV}}}")" "prod=default qa=default"
  expect "${loop}: default repo, path and revision named in full: the default chart" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-full" "deploy: {chart: {repoURL: ${REPO}, path: ${CPATH}, targetRevision: ${REV}}}")" "prod=default qa=default"
  expect "${loop}: another revision of the default chart: an own chart, fetched at that revision" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-rev" 'deploy: {chart: {targetRevision: v0.1.0}}')" \
    "prod=own:1/${CPATH} qa=own:2/${CPATH}
fetched ${REPO}@v0.1.0 ${REPO}@v0.1.0"
  expect "${loop}: default repo and path at another revision, named in full: an own chart" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-fullrev" "deploy: {chart: {repoURL: ${REPO}, path: ${CPATH}, targetRevision: v0.1.0}}")" \
    "prod=own:1/${CPATH} qa=own:2/${CPATH}
fetched ${REPO}@v0.1.0 ${REPO}@v0.1.0"
  expect "${loop}: one environment's canary revision: only that environment is an own chart" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-canary" $'deploy:\n  environments:\n    - name: qa\n      chart: {targetRevision: v0.1.0}')" \
    "prod=default qa=own:1/${CPATH}
fetched ${REPO}@v0.1.0"
  expect "${loop}: another path in the default repository inherits the default revision (not main)" \
    "$(plan "$d/${loop}-loop.sh" "${loop}-path" 'deploy: {chart: {path: charts/other}}')" \
    "prod=own:1/charts/other qa=own:2/charts/other
fetched ${REPO}@${REV} ${REPO}@${REV}"
done

# --- 3. validate-values: validated by the image, or rendered as an own chart --------------------------------------------------
yq '.spec.steps[] | select(.name == "clone-and-list") | .script' "$d/validate-values.yaml" > "$d/vv.sh"
awk '/^# BEGIN input-guards$/,/^# END input-guards$/' "$d/vv.sh" > "$d/vv-guards.sh"
awk '/^if \[\[ -n "\$\{cicd\}" && -s \/work\/files.txt \]\]; then$/{p=1} p{print} p && /^fi$/{exit}' "$d/vv.sh" > "$d/vv-loop.sh"
awk '/^for v in DEFAULT_CHART_REPO_URL/{p=1} p{print} p && /^done$/{exit}' "$d/vv.sh" > "$d/vv-inputs.sh"
[[ -s "$d/vv-inputs.sh" ]] || { echo "FAIL: the validate-values input check was not found"; exit 1; }
[[ -s "$d/vv-guards.sh" && -s "$d/vv-loop.sh" ]] || { echo "FAIL: the validate-values file loop was not found"; exit 1; }
vv() { # <scenario> <cicd.yaml text>: one changed file, hangar-prod/prod/values.yaml; prints "kept" or "own <targetRevision>"
  local W="$d/$1"
  mkdir -p "${W}"; printf '%s\n' "$2" > "${W}/cicd.yaml"; echo hangar-prod/prod/values.yaml > "${W}/files.txt"
  sed "s#/work#${W}#g" "$d/vv-loop.sh" > "${W}/loop.sh"
  ( cd "${W}" && cicd="$(cat cicd.yaml)" DEFAULT_CHART_REPO_URL="${REPO}" DEFAULT_CHART_PATH="${CPATH}" DEFAULT_CHART_REVISION="${REVOVR-${REV}}" \
      bash -c 'set -euo pipefail; source "$1"; source "$2"; source "$3"' _ "$d/vv-guards.sh" "$d/vv-inputs.sh" "${W}/loop.sh" ) > "${W}/loop.log" 2>&1; local rc=$?
  echo "${rc}" > "${W}/rc"
  if (( rc != 0 )) && [[ -z "${EXPECT_FAIL:-}" ]]; then echo "FAIL $1: the loop failed: $(cat "${W}/loop.log")"; fail=1; fi
  if [[ -s "${W}/own.txt" ]]; then echo "own $(cut -d'|' -f4- "${W}/own.txt" | jq -r .targetRevision)"; else [[ -s "${W}/files.txt" ]] && echo kept; fi
}
expect "values: the default revision: validated by the image" "$(vv vv-same "deploy: {chart: {targetRevision: ${REV}}}")" kept
expect "values: another revision of the default chart: rendered with its own chart" "$(vv vv-rev 'deploy: {chart: {targetRevision: v0.1.0}}')" "own v0.1.0"
expect "values: no chart spec: validated by the image" "$(vv vv-none $'deploy:\n  environments:\n    - name: prod')" kept

# --- 4. the chart lookup fails closed (I-57) ---------------------------------------------------------------------------------------
# A lookup error, or an unset input, fails the task and names what is at fault; it is never the default chart. An environment whose
# cicd.yaml names no chart (above) is the one legitimate default and is unchanged.
failed_naming() { # <name> <rc file> <log file> <text the message must contain>
  local rc; rc="$(cat "$2")"
  if [[ "${rc}" != 0 ]] && grep -q -- "$4" "$3"; then echo "ok   $1"; else echo "FAIL $1: rc=${rc}, log: $(cat "$3")"; fail=1; fi
}
for loop in cg pv; do
  EXPECT_FAIL=1 plan "$d/${loop}-loop.sh" "${loop}-err" 'deploy: [unclosed' > /dev/null
  failed_naming "${loop}: a lookup error (unreadable cicd.yaml): the task fails, naming the chart lookup" "$d/${loop}-err/rc" "$d/${loop}-err/plan.log" 'chart lookup'
  [[ ! -s "$d/${loop}-err/plan.txt" ]] && echo "ok   ... and plans no default chart" || { echo "FAIL ${loop}: a lookup error still planned a chart"; fail=1; }
  for var in DEFAULT_CHART_REVISION DEFAULT_CHART_REPO_URL DEFAULT_CHART_PATH; do
    REVOVR="" EXPECT_FAIL=1 plan "$d/${loop}-loop.sh" "${loop}-unset" 'deploy: {}' > /dev/null  # the revision input empty
    break
  done
  failed_naming "${loop}: an unset DEFAULT_CHART_REVISION: the task fails, naming the input" "$d/${loop}-unset/rc" "$d/${loop}-unset/plan.log" 'DEFAULT_CHART_REVISION is not set'
  [[ ! -s "$d/${loop}-unset/plan.txt" ]] && echo "ok   ... and plans no default chart" || { echo "FAIL ${loop}: an unset input still planned a chart"; fail=1; }
  expect "${loop}: a valid 'no chart' (cicd.yaml names none) is still the default chart" "$(plan "$d/${loop}-loop.sh" "${loop}-nochart" $'deploy:\n  environments:\n    - name: prod')" "prod=default qa=default"
done
EXPECT_FAIL=1 vv vv-err 'deploy: [unclosed' > /dev/null
failed_naming "values: a lookup error: the task fails, naming the chart lookup" "$d/vv-err/rc" "$d/vv-err/loop.log" 'chart lookup'
REVOVR="" EXPECT_FAIL=1 vv vv-unset 'deploy: {}' > /dev/null
failed_naming "values: an unset DEFAULT_CHART_REVISION: the task fails, naming the input" "$d/vv-unset/rc" "$d/vv-unset/loop.log" 'DEFAULT_CHART_REVISION is not set'
# the input check does not depend on cicd.yaml being present (CodeRabbit): an absent (empty) cicd.yaml with an unset input still fails
# the production block around the check, run with cicd="" (no cicd.yaml): from the input check to the end of the file loop. It fails if the
# check is moved back inside the non-empty-cicd condition.
awk '/^for v in DEFAULT_CHART_REPO_URL/{p=1} p{print} p && /^fi$/{exit}' "$d/vv.sh" > "$d/vv-block.sh"
run_block() { # <repo> <path> <revision>: prints "failed: <message>" or "passed"
  ( cd "$d" && mkdir -p vv-block-w && cd vv-block-w && : > files.txt && sed 's#/work#'"$d/vv-block-w"'#g' "$d/vv-block.sh" > block.sh
    out="$(DEFAULT_CHART_REPO_URL="$1" DEFAULT_CHART_PATH="$2" DEFAULT_CHART_REVISION="$3" cicd="" bash -c 'set -euo pipefail; source "$1"; source "$2"; echo passed' _ "$d/vv-guards.sh" block.sh 2>&1)" && echo "${out}" || echo "failed: ${out}" )
}
out="$(run_block "${REPO}" "${CPATH}" "")"
[[ "${out}" == "failed:"*"DEFAULT_CHART_REVISION is not set"* ]] && echo "ok   values: an unset input fails with no cicd.yaml at all (the production block, cicd empty)" || { echo "FAIL values: block, empty revision: ${out}"; fail=1; }
out="$(run_block "" "${CPATH}" "${REV}")"
[[ "${out}" == "failed:"*"DEFAULT_CHART_REPO_URL is not set"* ]] && echo "ok   ... an empty repository fails" || { echo "FAIL values: block, empty repo: ${out}"; fail=1; }
out="$(run_block "${REPO}" "" "${REV}")"
[[ "${out}" == "failed:"*"DEFAULT_CHART_PATH is not set"* ]] && echo "ok   ... an empty path fails" || { echo "FAIL values: block, empty path: ${out}"; fail=1; }
expect "values: all three inputs set and no cicd.yaml: passes" "$(run_block "${REPO}" "${CPATH}" "${REV}")" "passed"
expect "values: a valid 'no chart' is still validated by the image" "$(vv vv-nochart 'deploy: {}')" kept

# --- 5. the cicd.yaml read: a token that cannot be minted fails the task (CodeRabbit); 404 only ever means "no cicd.yaml" -------------
# The read as each Task carries it, from the rendered script, with the token helper and curl stubbed (no network).
for t in clearance-gate policy-validation-gate validate-values; do
  case "${t}" in clearance-gate) step=fetch ;; policy-validation-gate) step=fetch ;; validate-values) step=clone-and-list ;; esac
  yq ".spec.steps[] | select(.name == \"${step}\") | .script" "$d/${t}.yaml" | awk '/app_token="\$\(github_app_installation_token/{p=1} p{print} p && /^ *esac$/{exit}' > "$d/${t}-read.sh"
  [[ -s "$d/${t}-read.sh" ]] || { echo "FAIL ${t}: the cicd.yaml read was not found"; fail=1; continue; }
  sed 's#/work/#'"$d/${t}"'-w/#g' "$d/${t}-read.sh" > "$d/${t}-read2.sh"; mkdir -p "$d/${t}-w"
  run_read() { # <mint rc> <http code>: prints "rc=<rc> cicd=<value>"; the curl stub records that it ran
    ( cd "$d" && owner=o APP_NAME=app cicd=x CURLED="$d/${t}-curled"; rm -f "${CURLED}"
      export owner APP_NAME CURLED MINT_RC="$1" CODE="$2"
      bash -c 'set -euo pipefail
        github_app_installation_token() { [[ "${MINT_RC}" == 0 ]] && echo tok; }
        curl() { echo ran > "${CURLED}"; local o=""; while [[ $# -gt 0 ]]; do [[ "$1" == -o ]] && o="$2"; shift; done; : > "${o}"; echo "${CODE}"; }
        source "$1"; echo "cicd=[${cicd}]"' _ "$d/${t}-read2.sh" ) 2>&1 | tr '\n' ' '
  }
  out="$(run_read 1 200)"
  [[ "${out}" == *"cannot mint a read token"* && "${out}" != *"cicd=["* && ! -e "$d/${t}-curled" ]] && echo "ok   ${t}: a token that cannot be minted fails the task, before any request" \
    || { echo "FAIL ${t}: mint failure: ${out}"; fail=1; }
  out="$(run_read 0 404)"
  expect "${t}: a 404 with a token is no cicd.yaml (the default chart)" "${out}" "cicd=[] "
  out="$(run_read 0 503)"
  [[ "${out}" == *"HTTP 503"* && "${out}" != *"cicd=["* ]] && echo "ok   ${t}: a 5xx fails the task" || { echo "FAIL ${t}: 503: ${out}"; fail=1; }
done

if (( fail )); then echo "FAILED${KEEP:+ (kept $d)}"; exit 1; fi
echo "PASS default_chart_revision_test"
