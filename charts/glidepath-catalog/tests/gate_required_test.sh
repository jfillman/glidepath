#!/usr/bin/env bash
# The registry's per-gate `required` (docs/admin/release-guardrails.md): renders the catalog with the chart default
# and with releaseGuardrailOverrides, and checks what each gate Pipeline, detect-bypass-merge, release-log-emit,
# wait-for-release-guardrails, open-release-pr and hack/apply-guardrail-ruleset.sh make of it; bash -n on every
# rendered script that ranges over the registry; and that sast/provenance refuse required: false.
# Needs helm, python3 (pyyaml), jq, yq. Usage: charts/glidepath-catalog/tests/gate_required_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
tmp="$(mktemp -d)"
values=charts/glidepath-catalog/tests/values.example.yaml

render() { # name [extra helm args...] -> $tmp/<name>.yaml
  local name="$1"; shift
  helm template t charts/glidepath-catalog -f "${values}" --namespace platform-catalog "$@" > "${tmp}/${name}.yaml" \
    || { echo "FAIL: render ${name}"; fail=1; }
}
# tasks PIPELINE RENDERED -> "name[,onError][,when]" per task, one per line
tasks() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml
want, path = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(path)):
    if d and d.get("kind") == "Pipeline" and d["metadata"]["name"] == want:
        for t in d["spec"]["tasks"]:
            print(t["name"] + ("+continue" if t.get("onError") == "continue" else "") + ("+when" if t.get("when") else ""))
        for t in d["spec"].get("finally", []):
            for p in t.get("params", []):
                if p["name"] == "advisory-reason": print("advisory-reason=" + p["value"])
PY
}
script_of() { # TASK RENDERED -> the first step's script
  python3 - "$1" "$2" <<'PY'
import sys, yaml
want, path = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(path)):
    if d and d.get("kind") == "Task" and d["metadata"]["name"] == want:
        print(d["spec"]["steps"][0]["script"])
PY
}
expect() { # label got want
  [[ "$2" == "$3" ]] || { echo "FAIL $1:"; echo "  got:  $2"; echo "  want: $3"; fail=1; }
}

# ---- chart default: qa advisory, every other real gate required
render default
expect "qa-check default" "$(tasks qa-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image verify-check-ride-advisory+continue advisory-reason=not-required "
expect "image-scan-check default" "$(tasks image-scan-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image image-scan+when image-scan-advisory+continue+when advisory-reason=rollback "
expect "sast-check default" "$(tasks sast-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image verify-sast-attestation+when verify-sast-attestation-advisory+continue+when advisory-reason=rollback "
expect "sbom-check default" "$(tasks sbom-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image generate-sbom+when generate-sbom-advisory+continue+when advisory-reason=rollback "
expect "values-check default" "$(tasks values-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "validate advisory-reason=rollback "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr comment-pr-check-result; do
  script_of "${task}" "${tmp}/default.yaml" > "${tmp}/${task}.sh"
  [[ -s "${tmp}/${task}.sh" ]] || { echo "FAIL: no script for ${task}"; fail=1; }
  bash -n "${tmp}/${task}.sh" || { echo "FAIL: bash -n ${task}"; fail=1; }
done
expect "detect-bypass-merge gate loop (no qa)" "$(grep -o 'for gate in .*; do' "${tmp}/detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation ; do"
expect "release-log-emit records every gate" "$(grep -o 'for gate in .*; do' "${tmp}/release-log-emit.sh")" \
  "for gate in sast image-scan provenance sbom values itsm qa policy-validation ; do"
expect "release-log-emit bypass set (no qa)" "$(grep -o 'required_gates=".*"' "${tmp}/release-log-emit.sh")" \
  'required_gates=" sast image-scan provenance sbom values itsm policy-validation "'
expect "wait-for-release-guardrails siblings (no qa)" "$(grep -o 'for g in .*; do' "${tmp}/wait-for-release-guardrails.sh")" \
  'for g in "sast" "image-scan" "provenance" "sbom" "values" "itsm" "policy-validation" ; do'
grep -q '| `qa` | [^|]* | real, advisory - reports without blocking' "${tmp}/open-release-pr.sh" \
  || { echo "FAIL: the PR body table does not mark qa advisory"; fail=1; }
grep -q '| `sast` | [^|]* | real |' "${tmp}/open-release-pr.sh" || { echo "FAIL: the PR body table does not mark sast real"; fail=1; }
grep -q '| `itsm` | [^|]* | not yet real' "${tmp}/open-release-pr.sh" || { echo "FAIL: the PR body table does not mark itsm a stub"; fail=1; }

# ---- a cluster's overrides: qa required, sbom and values advisory (a map, so the list is untouched)
cat > "${tmp}/override-values.yaml" <<'YAML'
releaseGuardrailOverrides:
  qa: { required: true }
  sbom: { required: false }
  values: { required: false }
  itsm: { required: false }
YAML
render override -f "${tmp}/override-values.yaml"
expect "qa-check required" "$(tasks qa-check "${tmp}/override.yaml" | tr "\n" " ")" \
  "extract-promoted-image verify-check-ride+when verify-check-ride-advisory+continue+when advisory-reason=rollback "
expect "sbom-check advisory" "$(tasks sbom-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "extract-promoted-image generate-sbom-advisory+continue advisory-reason=not-required "
expect "values-check advisory" "$(tasks values-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "validate-advisory+continue advisory-reason=not-required "
script_of detect-bypass-merge "${tmp}/override.yaml" > "${tmp}/detect-override.sh"
expect "detect-bypass-merge with overrides (stub itsm stays; sbom/values out; qa in)" \
  "$(grep -o 'for gate in .*; do' "${tmp}/detect-override.sh")" \
  "for gate in sast image-scan provenance itsm qa policy-validation ; do"

# ---- the ruleset script applies the same rule
rs() { hack/apply-guardrail-ruleset.sh --dry-run "$@" x/y | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
  | sed 's#Pipelines as Code CI / ##' | tr '\n' ' '; }
expect "ruleset default" "$(rs)" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- "
expect "ruleset with overrides" "$(rs -f "${tmp}/override-values.yaml")" "sast- image-scan- provenance- itsm- qa- policy-validation- "

# ---- sast and provenance cannot be made advisory
for locked in sast provenance; do
  printf 'releaseGuardrailOverrides:\n  %s: { required: false }\n' "${locked}" > "${tmp}/locked.yaml"
  if helm template t charts/glidepath-catalog -f "${values}" -f "${tmp}/locked.yaml" --namespace platform-catalog > /dev/null 2> "${tmp}/err"; then
    echo "FAIL: ${locked} required: false rendered"; fail=1
  elif ! grep -q "${locked} cannot be required: false" "${tmp}/err"; then
    echo "FAIL: ${locked} refusal has the wrong message: $(cat "${tmp}/err")"; fail=1
  fi
done

rm -rf "${tmp}"
[[ ${fail} -eq 0 ]] && echo "OK: the registry's required field drives every gate, the bypass tooling and the ruleset"
exit ${fail}
