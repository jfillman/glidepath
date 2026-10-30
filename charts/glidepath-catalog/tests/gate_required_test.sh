#!/usr/bin/env bash
# The registry's per-gate `required` (docs/admin/release-guardrails.md): renders the catalog with the chart default
# and with releaseGuardrailOverrides, and checks what each gate Pipeline, detect-bypass-merge, release-log-emit,
# wait-for-release-guardrails, open-release-pr and hack/apply-guardrail-ruleset.sh make of it; bash -n on every
# rendered script that ranges over the registry; and that sast/provenance refuse required: false. 2026-10-28: the
# per-app map (releaseGuardrailOverridesByApp) - the bypass set, the PR body table and the ruleset for one app, the
# gate Pipelines left as they are, and its render refusals.
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
expect "api-compat-check default (advisory)" "$(tasks api-compat-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image compare-api-advisory+continue advisory-reason=not-required "
expect "values-check default" "$(tasks values-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "validate advisory-reason=rollback "
expect "policy-validation-check default (required, no rollback copy)" "$(tasks policy-validation-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "evaluate advisory-reason=rollback "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr comment-pr-check-result; do
  script_of "${task}" "${tmp}/default.yaml" > "${tmp}/${task}.sh"
  [[ -s "${tmp}/${task}.sh" ]] || { echo "FAIL: no script for ${task}"; fail=1; }
  bash -n "${tmp}/${task}.sh" || { echo "FAIL: bash -n ${task}"; fail=1; }
done
expect "detect-bypass-merge gate loop (no qa)" "$(grep -o 'for gate in .*; do' "${tmp}/detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation ; do"
expect "release-log-emit records every gate" "$(grep -o 'for gate in .*; do' "${tmp}/release-log-emit.sh")" \
  "for gate in sast image-scan provenance sbom values itsm api-compat qa policy-validation ; do"
expect "release-log-emit bypass set (no qa)" "$(grep -o 'required_gates=".*"' "${tmp}/release-log-emit.sh")" \
  'required_gates=" sast image-scan provenance sbom values itsm policy-validation "'
expect "wait-for-release-guardrails siblings (no qa)" "$(grep -o 'for g in .*; do' "${tmp}/wait-for-release-guardrails.sh")" \
  'for g in "sast" "image-scan" "provenance" "sbom" "values" "itsm" "policy-validation" ; do'
grep -q '| `qa` | [^|]* | real, advisory - reports without blocking' "${tmp}/open-release-pr.sh" \
  || { echo "FAIL: the PR body table does not mark qa advisory"; fail=1; }
grep -q '| `api-compat` | [^|]* | real, advisory - reports without blocking' "${tmp}/open-release-pr.sh" \
  || { echo "FAIL: the PR body table does not mark api-compat advisory"; fail=1; }
grep -q '| `sast` | [^|]* | real |' "${tmp}/open-release-pr.sh" || { echo "FAIL: the PR body table does not mark sast real"; fail=1; }
grep -q '| `itsm` | [^|]* | not yet real' "${tmp}/open-release-pr.sh" || { echo "FAIL: the PR body table does not mark itsm a stub"; fail=1; }
grep -q '| `policy-validation` | [^|]* | real |' "${tmp}/open-release-pr.sh" || { echo "FAIL: the PR body table does not mark policy-validation real"; fail=1; }

# ---- a cluster's overrides: qa required, sbom, values and policy-validation advisory (a map, so the list is untouched)
cat > "${tmp}/override-values.yaml" <<'YAML'
releaseGuardrailOverrides:
  qa: { required: true }
  sbom: { required: false }
  values: { required: false }
  itsm: { required: false }
  api-compat: { required: true }
  policy-validation: { required: false }
YAML
render override -f "${tmp}/override-values.yaml"
expect "qa-check required" "$(tasks qa-check "${tmp}/override.yaml" | tr "\n" " ")" \
  "extract-promoted-image verify-check-ride+when verify-check-ride-advisory+continue+when advisory-reason=rollback "
expect "api-compat-check required" "$(tasks api-compat-check "${tmp}/override.yaml" | tr "\n" " ")" \
  "extract-promoted-image compare-api+when compare-api-advisory+continue+when advisory-reason=rollback "
expect "sbom-check advisory" "$(tasks sbom-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "extract-promoted-image generate-sbom-advisory+continue advisory-reason=not-required "
expect "values-check advisory" "$(tasks values-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "validate-advisory+continue advisory-reason=not-required "
expect "policy-validation-check advisory" "$(tasks policy-validation-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "evaluate-advisory+continue advisory-reason=not-required "
script_of detect-bypass-merge "${tmp}/override.yaml" > "${tmp}/detect-override.sh"
expect "detect-bypass-merge with overrides (stub itsm stays; sbom/values out; qa in)" \
  "$(grep -o 'for gate in .*; do' "${tmp}/detect-override.sh")" \
  "for gate in sast image-scan provenance itsm api-compat qa ; do"

# ---- the ruleset script applies the same rule
rs() { hack/apply-guardrail-ruleset.sh --dry-run "$@" x/y | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
  | sed 's#Pipelines as Code CI / ##' | tr '\n' ' '; }
expect "ruleset default" "$(rs)" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- "
expect "ruleset with overrides" "$(rs -f "${tmp}/override-values.yaml")" "sast- image-scan- provenance- itsm- api-compat- qa- "
printf 'promotion:\n  enabled: true\n  pinDigest: true\n' > "${tmp}/promotion-values.yaml"
expect "ruleset with promotion on (image-promotion required, as the chart's effective registry)" \
  "$(rs -f "${tmp}/promotion-values.yaml")" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- image-promotion- "

# ---- sast and provenance cannot be made advisory
for locked in sast provenance; do
  printf 'releaseGuardrailOverrides:\n  %s: { required: false }\n' "${locked}" > "${tmp}/locked.yaml"
  if helm template t charts/glidepath-catalog -f "${values}" -f "${tmp}/locked.yaml" --namespace platform-catalog > /dev/null 2> "${tmp}/err"; then
    echo "FAIL: ${locked} required: false rendered"; fail=1
  elif ! grep -q "${locked} cannot be required: false" "${tmp}/err"; then
    echo "FAIL: ${locked} refusal has the wrong message: $(cat "${tmp}/err")"; fail=1
  fi
done

# ---- releaseGuardrailOverridesByApp (2026-10-28): advisory for one app at runtime, the render-time variants untouched
cat > "${tmp}/perapp-values.yaml" <<'YAML'
releaseGuardrailOverridesByApp:
  backstage:
    image-scan: { required: false }
YAML
render perapp -f "${tmp}/perapp-values.yaml"
expect "image-scan-check per-app render (still the hard check)" "$(tasks image-scan-check "${tmp}/perapp.yaml" | tr '\n' ' ')" \
  "extract-promoted-image image-scan+when image-scan-advisory+continue+when advisory-reason=rollback "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr; do
  script_of "${task}" "${tmp}/perapp.yaml" > "${tmp}/perapp-${task}.sh"
  bash -n "${tmp}/perapp-${task}.sh" || { echo "FAIL: bash -n ${task} (per-app)"; fail=1; }
  grep -q '^    backstage) echo "image-scan" ;;$' "${tmp}/perapp-${task}.sh" || { echo "FAIL: ${task} has no per-app lookup"; fail=1; }
done
expect "detect-bypass-merge gate loop unchanged by the per-app map" "$(grep -o 'for gate in .*; do' "${tmp}/perapp-detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation ; do"
helpers() { sed -n '/^advisory_gates_for_app() {$/,/^gate_advisory_for_app() /p' "$1"; }
# release-log-emit's bypass set for backstage and another app: the helper and the two lines that build the set.
log_set() { # app
  { helpers "${tmp}/perapp-release-log-emit.sh"; grep -E '^ *(required_gates=" |log_app=|for g in \$\(advisory_gates_for_app)' "${tmp}/perapp-release-log-emit.sh"
    echo 'printf "%s" "${required_gates}"'; } > "${tmp}/log-set.sh"
  APP_NAME=ignored repo="gitops-$1" bash "${tmp}/log-set.sh"
}
expect "release-log-emit bypass set, backstage" "$(log_set backstage)" " sast provenance sbom values itsm policy-validation "
expect "release-log-emit bypass set, gate-api" "$(log_set gate-api)" " sast image-scan provenance sbom values itsm policy-validation "
# open-release-pr's PR body table: the image-scan row says advisory for backstage only.
table_for() { # app
  { helpers "${tmp}/perapp-open-release-pr.sh"
    # from the rendered table through the per-app rewrite's last line (anchored on it: nothing after it may run here)
    sed -n '/^governance_text="\$(printf /,/^  governance_text="\${table%/p' "${tmp}/perapp-open-release-pr.sh"
    printf 'fi\nprintf "%%s" "${governance_text}"\n'; } > "${tmp}/table.sh"
  grep -q 'curl\|git ' "${tmp}/table.sh" && { echo "FAIL: the PR-body extract reaches past the table"; fail=1; return 1; }
  APP_NAME="$1" bash "${tmp}/table.sh"
}
table_for backstage > "${tmp}/table-backstage.md"; table_for gate-api > "${tmp}/table-other.md"
grep -q '^| `image-scan` | [^|]* | real, advisory for backstage - its check can fail without holding this release' "${tmp}/table-backstage.md" \
  || { echo "FAIL: the backstage PR body table does not mark image-scan advisory for backstage"; cat "${tmp}/table-backstage.md"; fail=1; }
grep -q '^| `sbom` | [^|]* | real |$' "${tmp}/table-backstage.md" || { echo "FAIL: the backstage table changed sbom too"; fail=1; }
grep -q '^| `image-scan` | [^|]* | real |$' "${tmp}/table-other.md" || { echo "FAIL: another app's table marks image-scan advisory"; fail=1; }
expect "PR body table: only the image-scan row differs" "$(diff "${tmp}/table-backstage.md" "${tmp}/table-other.md" | grep -c '^[<>]')" 2
# the ruleset for gitops-backstage leaves image-scan out; for any other repo it stays.
rs_repo() { hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/perapp-values.yaml" "$1" | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
  | sed 's#Pipelines as Code CI / ##' | tr '\n' ' '; }
expect "ruleset gitops-backstage" "$(rs_repo x/gitops-backstage)" "sast- provenance- sbom- values- itsm- policy-validation- "
expect "ruleset gitops-gate-api" "$(rs_repo x/gitops-gate-api)" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- "
expect "ruleset gitops-backstage with promotion on keeps image-promotion" \
  "$(hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/perapp-values.yaml" -f "${tmp}/promotion-values.yaml" x/gitops-backstage | grep -v '^#' \
     | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
     | sed 's#Pipelines as Code CI / ##' | tr '\n' ' ')" "sast- provenance- sbom- values- itsm- policy-validation- image-promotion- "

# ---- per-app refusals: integrity gates, image-promotion, an unknown gate, a non-plain app, anything but required: false
refuse() { # label want-in-message values-yaml [extra helm args...]
  local label="$1" want="$2" body="$3"; shift 3
  printf '%s\n' "${body}" > "${tmp}/refuse.yaml"
  if helm template t charts/glidepath-catalog -f "${values}" -f "${tmp}/refuse.yaml" --namespace platform-catalog "$@" > /dev/null 2> "${tmp}/err"; then
    echo "FAIL: per-app ${label} rendered"; fail=1
  elif ! grep -qF -- "${want}" "${tmp}/err"; then
    echo "FAIL: per-app ${label} refusal has the wrong message: $(cat "${tmp}/err")"; fail=1
  fi
}
for locked in sast provenance; do
  refuse "${locked}" "releaseGuardrailOverridesByApp.backstage: ${locked} cannot be required: false" \
    "$(printf 'releaseGuardrailOverridesByApp:\n  backstage:\n    %s: { required: false }' "${locked}")"
done
refuse image-promotion "releaseGuardrailOverridesByApp.backstage: image-promotion cannot be required: false" \
  $'releaseGuardrailOverridesByApp:\n  backstage:\n    image-promotion: { required: false }' \
  --set promotion.enabled=true --set promotion.pinDigest=true --set registries.flight.namespace=riley-roooo
refuse image-promotion-off "releaseGuardrailOverridesByApp.backstage: image-promotion cannot be required: false" \
  $'releaseGuardrailOverridesByApp:\n  backstage:\n    image-promotion: { required: false }'
refuse unknown-gate '"image-scann" is not a gate in releaseGuardrails' \
  $'releaseGuardrailOverridesByApp:\n  backstage:\n    image-scann: { required: false }'
refuse app-name '"gitops/backstage" is not an app name' \
  $'releaseGuardrailOverridesByApp:\n  gitops/backstage:\n    image-scan: { required: false }'
refuse app-name-case '"Backstage" is not an app name' \
  $'releaseGuardrailOverridesByApp:\n  Backstage:\n    image-scan: { required: false }'
refuse required-true "releaseGuardrailOverridesByApp.backstage.qa: only { required: false } is supported" \
  $'releaseGuardrailOverridesByApp:\n  backstage:\n    qa: { required: true }'
refuse required-string "releaseGuardrailOverridesByApp.backstage.image-scan: only { required: false } is supported" \
  $'releaseGuardrailOverridesByApp:\n  backstage:\n    image-scan: { required: "false" }'
refuse not-a-map "releaseGuardrailOverridesByApp.backstage must be a map" \
  $'releaseGuardrailOverridesByApp:\n  backstage: [image-scan]'

rm -rf "${tmp}"
[[ ${fail} -eq 0 ]] && echo "OK: the registry's required field drives every gate, the bypass tooling and the ruleset"
exit ${fail}
