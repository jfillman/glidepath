#!/usr/bin/env bash
# The registry's per-gate `required` (docs/admin/release-guardrails.md): renders the catalog with the chart default
# and with releaseGuardrailOverrides, and checks what each gate Pipeline, detect-bypass-merge, release-log-emit,
# wait-for-release-guardrails, open-release-pr and hack/apply-guardrail-ruleset.sh make of it; bash -n on every
# rendered script that ranges over the registry; and that sast/provenance refuse required: false. 2026-10-28: the
# per-app map (releaseGuardrailOverridesByApp) - the bypass set, the PR body table and the ruleset for one app, the
# gate Pipelines left as they are, and its render refusals. CG-2: the class x kind matrix in the release-guardrails
# ConfigMap, with the cluster overrides composed, and its render locks.
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
script_of() { # TASK RENDERED -> the first step's script (the first after a CG-3 classify step, which has none)
  python3 - "$1" "$2" <<'PY'
import sys, yaml
want, path = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(path)):
    if d and d.get("kind") == "Task" and d["metadata"]["name"] == want:
        print([st for st in d["spec"]["steps"] if "script" in st][0]["script"])
PY
}
expect() { # label got want
  [[ "$2" == "$3" ]] || { echo "FAIL $1:"; echo "  got:  $2"; echo "  want: $3"; fail=1; }
}

# ---- chart default: qa advisory, every other real gate required
render default
expect "qa-check default" "$(tasks qa-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image verify-check-ride-advisory+continue+when not-applicable+when advisory-reason=not-required "
expect "image-scan-check default" "$(tasks image-scan-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image image-scan+when image-scan-advisory+continue+when not-applicable+when advisory-reason=rollback "
expect "sast-check default" "$(tasks sast-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image verify-sast-attestation+when verify-sast-attestation-advisory+continue+when not-applicable+when advisory-reason=rollback "
expect "sbom-check default" "$(tasks sbom-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image generate-sbom+when generate-sbom-advisory+continue+when not-applicable+when advisory-reason=rollback "
expect "api-compat-check default (advisory)" "$(tasks api-compat-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "extract-promoted-image compare-api-advisory+continue+when not-applicable+when advisory-reason=not-required "
expect "values-check default" "$(tasks values-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "fetch render+when validate+when not-applicable+when advisory-reason=rollback "
expect "policy-validation-check default (required, no rollback copy)" "$(tasks policy-validation-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "fetch render+when evaluate+when not-applicable+when advisory-reason=rollback "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr comment-pr-check-result; do
  script_of "${task}" "${tmp}/default.yaml" > "${tmp}/${task}.sh"
  [[ -s "${tmp}/${task}.sh" ]] || { echo "FAIL: no script for ${task}"; fail=1; }
  bash -n "${tmp}/${task}.sh" || { echo "FAIL: bash -n ${task}"; fail=1; }
done
expect "detect-bypass-merge gate loop (no qa)" "$(grep -o 'for gate in .*; do' "${tmp}/detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation agent-gates ; do"
expect "release-log-emit records every gate" "$(grep -o 'for gate in .*; do' "${tmp}/release-log-emit.sh")" \
  "for gate in sast image-scan provenance sbom values itsm api-compat qa policy-validation agent-gates clearance-gate ; do"
expect "release-log-emit bypass set (no qa)" "$(grep -o 'required_gates=".*"' "${tmp}/release-log-emit.sh")" \
  'required_gates=" sast image-scan provenance sbom values itsm policy-validation agent-gates "'
expect "wait-for-release-guardrails siblings (no qa)" "$(grep -o 'for g in .*; do' "${tmp}/wait-for-release-guardrails.sh")" \
  'for g in "sast" "image-scan" "provenance" "sbom" "values" "itsm" "policy-validation" "agent-gates" ; do'
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
  "extract-promoted-image verify-check-ride+when verify-check-ride-advisory+continue+when not-applicable+when advisory-reason=rollback "
expect "api-compat-check required" "$(tasks api-compat-check "${tmp}/override.yaml" | tr "\n" " ")" \
  "extract-promoted-image compare-api+when compare-api-advisory+continue+when not-applicable+when advisory-reason=rollback "
expect "sbom-check advisory" "$(tasks sbom-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "extract-promoted-image generate-sbom-advisory+continue+when not-applicable+when advisory-reason=not-required "
expect "values-check advisory" "$(tasks values-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "fetch+continue render+continue+when validate-advisory+continue+when not-applicable+when advisory-reason=not-required "
expect "policy-validation-check advisory" "$(tasks policy-validation-check "${tmp}/override.yaml" | tr '\n' ' ')" \
  "fetch+continue render+continue+when evaluate-advisory+continue+when not-applicable+when advisory-reason=not-required "
script_of detect-bypass-merge "${tmp}/override.yaml" > "${tmp}/detect-override.sh"
expect "detect-bypass-merge with overrides (stub itsm stays; sbom/values out; qa in)" \
  "$(grep -o 'for gate in .*; do' "${tmp}/detect-override.sh")" \
  "for gate in sast image-scan provenance itsm api-compat qa agent-gates ; do"

# ---- the ruleset script applies the same rule
rs() { hack/apply-guardrail-ruleset.sh --dry-run "$@" x/y | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
  | sed 's#Glidepath / ##' | tr '\n' ' '; }
expect "ruleset default" "$(rs)" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- agent-gates- "
expect "ruleset with overrides" "$(rs -f "${tmp}/override-values.yaml")" "sast- image-scan- provenance- itsm- api-compat- qa- agent-gates- "
printf 'promotion:\n  enabled: true\n  pinDigest: true\n' > "${tmp}/promotion-values.yaml"
expect "ruleset with promotion on (image-promotion required, as the chart's effective registry)" \
  "$(rs -f "${tmp}/promotion-values.yaml")" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- agent-gates- image-promotion- "

# ---- G-ARGO-8: the rollout flip. clearance-gate is advisory by default; releaseGuardrailOverrides makes it required, and
# then the one hard register task replaces the advisory pair, the fan-in's wait, the bypass detectors, the log's
# required set and the ruleset all name it. The check name comes from pipelinesAsCode (applicationName + legacy names),
# never from a literal in the scripts.
printf 'releaseGuardrailOverrides:\n  clearance-gate: { required: true }\n' > "${tmp}/cg-required.yaml"
render cgreq -f "${tmp}/cg-required.yaml"
expect "clearance-gate-check required: fetch, render, one hard register task, no render-check" "$(tasks clearance-gate-check "${tmp}/cgreq.yaml" | tr '\n' ' ')" \
  "fetch render+when register+when not-applicable+when advisory-reason=rollback "
expect "clearance-gate-check default: fetch, render, advisory register plus a hard render-check" "$(tasks clearance-gate-check "${tmp}/default.yaml" | tr '\n' ' ')" \
  "fetch render+when register-advisory+continue+when render-check+when not-applicable+when advisory-reason=not-required "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails; do
  script_of "${task}" "${tmp}/cgreq.yaml" > "${tmp}/cgreq-${task}.sh"
  bash -n "${tmp}/cgreq-${task}.sh" || { echo "FAIL: bash -n ${task} (clearance-gate required)"; fail=1; }
done
expect "required clearance-gate: detect-bypass-merge counts it" "$(grep -o 'for gate in .*; do' "${tmp}/cgreq-detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation agent-gates clearance-gate ; do"
expect "required clearance-gate: the fan-in waits on it" "$(grep -o 'for g in .*; do' "${tmp}/cgreq-wait-for-release-guardrails.sh")" \
  'for g in "sast" "image-scan" "provenance" "sbom" "values" "itsm" "policy-validation" "agent-gates" "clearance-gate" ;'" do"
expect "required clearance-gate: release-log-emit's bypass set" "$(grep -o 'required_gates=".*"' "${tmp}/cgreq-release-log-emit.sh")" \
  'required_gates=" sast image-scan provenance sbom values itsm policy-validation agent-gates clearance-gate "'
expect "required clearance-gate: the ruleset requires it (by the configured application name)" \
  "$(rs -f "${tmp}/cg-required.yaml")" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- agent-gates- clearance-gate- "
# A check is found under every name PaC has posted it, so a renamed application keeps finding it.
printf 'pipelinesAsCode:\n  applicationName: Acme\n  legacyApplicationNames: ["Pipelines as Code CI", "Glidepath"]\n' > "${tmp}/rename.yaml"
render cgrename -f "${tmp}/cg-required.yaml" -f "${tmp}/rename.yaml"
script_of wait-for-release-guardrails "${tmp}/cgrename.yaml" > "${tmp}/cgrename-wait.sh"
grep -q 'Acme' "${tmp}/cgrename-wait.sh" && grep -q 'Pipelines as Code CI' "${tmp}/cgrename-wait.sh" \
  || { echo "FAIL: the fan-in's wait does not read pipelinesAsCode.applicationName and legacyApplicationNames"; fail=1; }
if script_of clearance-gate "${tmp}/cgrename.yaml" | grep -q 'Glidepath / '; then echo "FAIL: clearance-gate task hardcodes the check prefix"; fail=1; fi

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
  "extract-promoted-image image-scan+when image-scan-advisory+continue+when not-applicable+when advisory-reason=rollback "
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr; do
  script_of "${task}" "${tmp}/perapp.yaml" > "${tmp}/perapp-${task}.sh"
  bash -n "${tmp}/perapp-${task}.sh" || { echo "FAIL: bash -n ${task} (per-app)"; fail=1; }
  grep -q '^    backstage) echo "image-scan" ;;$' "${tmp}/perapp-${task}.sh" || { echo "FAIL: ${task} has no per-app lookup"; fail=1; }
done
expect "detect-bypass-merge gate loop unchanged by the per-app map" "$(grep -o 'for gate in .*; do' "${tmp}/perapp-detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation agent-gates ; do"
helpers() { sed -n '/^advisory_gates_for_app() {$/,/^gate_advisory_for_app() /p' "$1"; }
# release-log-emit's bypass set for backstage and another app: the helper and the two lines that build the set.
log_set() { # app
  { helpers "${tmp}/perapp-release-log-emit.sh"; grep -E '^ *(required_gates=" |log_app=|for g in \$\(advisory_gates_for_app)' "${tmp}/perapp-release-log-emit.sh"
    echo 'printf "%s" "${required_gates}"'; } > "${tmp}/log-set.sh"
  APP_NAME=ignored repo="gitops-$1" bash "${tmp}/log-set.sh"
}
expect "release-log-emit bypass set, backstage" "$(log_set backstage)" " sast provenance sbom values itsm policy-validation agent-gates "
expect "release-log-emit bypass set, gate-api" "$(log_set gate-api)" " sast image-scan provenance sbom values itsm policy-validation agent-gates "
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
  | sed 's#Glidepath / ##' | tr '\n' ' '; }
expect "ruleset gitops-backstage" "$(rs_repo x/gitops-backstage)" "sast- provenance- sbom- values- itsm- policy-validation- agent-gates- "
expect "ruleset gitops-gate-api" "$(rs_repo x/gitops-gate-api)" "sast- image-scan- provenance- sbom- values- itsm- policy-validation- agent-gates- "
expect "ruleset gitops-backstage with promotion on keeps image-promotion" \
  "$(hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/perapp-values.yaml" -f "${tmp}/promotion-values.yaml" x/gitops-backstage | grep -v '^#' \
     | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' \
     | sed 's#Glidepath / ##' | tr '\n' ' ')" "sast- provenance- sbom- values- itsm- policy-validation- agent-gates- image-promotion- "

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

# ---- C-7: Clearance's GitHub App reports the agent checks (agentGates.clearanceUrl + clearanceChecks)
cat > "${tmp}/cc-values.yaml" <<'YAML'
agentGates:
  clearanceUrl: http://clearance.example
  clearanceChecks: true
  clearanceAppId: "5561694"
YAML
render cc -f "${tmp}/cc-values.yaml"
for task in detect-bypass-merge release-log-emit wait-for-release-guardrails open-release-pr promote comment-pr-check-result deliver-onboarding-files; do
  script_of "${task}" "${tmp}/cc.yaml" > "${tmp}/cc-${task}.sh"
  [[ -s "${tmp}/cc-${task}.sh" ]] && bash -n "${tmp}/cc-${task}.sh" || { echo "FAIL: bash -n ${task} (clearance)"; fail=1; }
done
expect "clearance: detect-bypass-merge gate loop" "$(grep -o 'for gate in .*; do' "${tmp}/cc-detect-bypass-merge.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation agent-identity agent-scope ; do"
expect "clearance: the exact check names are the readers' external map" \
  "$(grep -o "^ *ext_checks='.*'" "${tmp}/cc-detect-bypass-merge.sh" | sed "s/^ *ext_checks=//")" \
  "'{\"agent-identity\":\"Clearance / agent-identity\",\"agent-scope\":\"Clearance / agent-scope\"}'"
expect "default: no external checks" "$(grep -o "^ *ext_checks='.*'" "${tmp}/perapp-detect-bypass-merge.sh" | sed "s/^ *ext_checks=//")" "'{}'"
expect "clearance: wait-for-release-guardrails gates" "$(grep -o 'for g in .*; do' "${tmp}/cc-wait-for-release-guardrails.sh")" \
  'for g in "sast" "image-scan" "provenance" "sbom" "values" "itsm" "policy-validation" "agent-identity" "agent-scope" ; do'
expect "clearance: promote required gates" "$(grep -o 'required_gates=".*"' "${tmp}/cc-promote.sh")" \
  'required_gates="sast image-scan provenance sbom values itsm policy-validation agent-identity agent-scope "'
grep -q 'agent-gates' "${tmp}/cc-open-release-pr.sh" && { echo "FAIL: clearance PR body table still lists agent-gates"; fail=1; }
grep -q '| `agent-identity` |' "${tmp}/cc-open-release-pr.sh" && grep -q '| `agent-scope` |' "${tmp}/cc-open-release-pr.sh" \
  || { echo "FAIL: clearance PR body table lacks agent-identity/agent-scope"; fail=1; }
expect "clearance: release-log bypass set" "$(grep -o '^ *required_gates=" .*"' "${tmp}/cc-release-log-emit.sh" | sed 's/^ *//')" \
  'required_gates=" sast image-scan provenance sbom values itsm policy-validation agent-identity agent-scope "'
expect "clearance: onboarding is told" "$(grep -c 'CLEARANCE_CHECKS' "${tmp}/cc-deliver-onboarding-files.sh")" "$(grep -c 'CLEARANCE_CHECKS' "${tmp}/cc-deliver-onboarding-files.sh")"
python3 - "${tmp}/cc.yaml" "${tmp}/default.yaml" <<'PY' || fail=1
import sys, yaml
for path, want in ((sys.argv[1], "true"), (sys.argv[2], "false")):
    for d in yaml.safe_load_all(open(path)):
        if d and d.get("kind") == "Task" and d["metadata"]["name"] == "deliver-onboarding-files":
            for st in d["spec"]["steps"]:
                env = {e["name"]: e.get("value") for e in st.get("env", [])}
                if "CLEARANCE_CHECKS" in env and env["CLEARANCE_CHECKS"] != want:
                    print("FAIL: %s CLEARANCE_CHECKS=%s want %s in %s" % (st["name"], env["CLEARANCE_CHECKS"], want, path)); sys.exit(1)
                if "RELEASE_GUARDRAILS_JSON" in env:
                    names = [g["name"] for g in __import__("json").loads(env["RELEASE_GUARDRAILS_JSON"])]
                    if (("agent-gates" in names) == (want == "true")):
                        print("FAIL: onboarding registry %s in %s" % (names, path)); sys.exit(1)
PY
# the ruleset: the App's exact names under its id; the PaC gates under theirs
expect "clearance: gitops ruleset" "$(hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/cc-values.yaml" x/gitops-a | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[]|"\(.context)=\(.integration_id)"' | tr '\n' ' ')" \
  "Glidepath / sast-=4461953 Glidepath / image-scan-=4461953 Glidepath / provenance-=4461953 Glidepath / sbom-=4461953 Glidepath / values-=4461953 Glidepath / itsm-=4461953 Glidepath / policy-validation-=4461953 Clearance / agent-identity=5561694 Clearance / agent-scope=5561694 "
expect "clearance: per-cluster advisory agent-scope leaves it out of the ruleset" "$(hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/cc-values.yaml" \
  -f <(printf 'releaseGuardrailOverrides:\n  agent-scope: { required: false }\n') x/gitops-a | grep -v '^#' \
  | jq -r '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context' | grep -c 'agent-scope')" 0
printf 'agentGates:\n  clearanceUrl: http://c\n  clearanceChecks: true\n' > "${tmp}/cc-noid.yaml"
hack/apply-guardrail-ruleset.sh --dry-run -f "${tmp}/cc-noid.yaml" x/gitops-a >/dev/null 2>"${tmp}/err" && { echo "FAIL: ruleset built without the Clearance App id"; fail=1; }
grep -q 'GitHub App id is not set' "${tmp}/err" || { echo "FAIL: missing-id message: $(cat "${tmp}/err")"; fail=1; }
expect "clearance: app-repo ruleset" "$(hack/apply-agent-gates-ruleset.sh --dry-run -f "${tmp}/cc-values.yaml" x/app | grep -v '^#' \
  | jq -r '.rules[0].parameters.required_status_checks[]|"\(.context)=\(.integration_id)"' | tr '\n' ' ')" \
  "Clearance / agent-identity=5561694 Clearance / agent-scope=5561694 Glidepath / cicd-validate-=4461953 "
expect "default: app-repo ruleset keeps agent-gates-" "$(hack/apply-agent-gates-ruleset.sh --dry-run x/app | grep -v '^#' \
  | jq -r '.rules[0].parameters.required_status_checks[].context' | tr '\n' ' ')" "Glidepath / agent-gates- Glidepath / cicd-validate- "
# the fallback: clearanceChecks without a clearanceUrl keeps agent-gates (no Clearance on the cluster)
render ccnourl --set agentGates.clearanceChecks=true
script_of detect-bypass-merge "${tmp}/ccnourl.yaml" > "${tmp}/ccnourl.sh"
expect "no Clearance: clearanceChecks alone keeps agent-gates" "$(grep -o 'for gate in .*; do' "${tmp}/ccnourl.sh")" \
  "for gate in sast image-scan provenance sbom values itsm policy-validation agent-gates ; do"
# a per-cluster override of the old name still renders (it just has nothing to act on once Clearance reports the checks)
render ccold -f "${tmp}/cc-values.yaml" --set-json 'releaseGuardrailOverrides={"agent-gates":{"required":false}}'

# ---- CG-2: change kinds (docs/admin/release-guardrails.md "Change kinds"). The class x kind matrix is published as the
# release-guardrails ConfigMap; `effective` composes required on top (a gate that runs but is required: false advises).
matrix() { # RENDERED -> "gate image-release/rollback/config/no-op" per gate, from `effective`; one line per gate
  python3 - "$1" <<'PY'
import sys, yaml, json
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "release-guardrails":
        m = json.loads(d["data"]["matrix.json"])
        assert m["v"] == 1 and m["kinds"] == ["image-release", "rollback", "config", "no-op"], m["kinds"]
        assert m["values"] == ["run", "advisory", "n/a"], m["values"]
        for g in m["gates"]:
            print(g["name"] + " " + "/".join(g["effective"][k] for k in m["kinds"]))
PY
}
expect "release-guardrails matrix, chart default" "$(matrix "${tmp}/default.yaml" | tr '\n' ' ')" \
  "sast run/advisory/n/a/n/a image-scan run/advisory/n/a/n/a provenance run/run/run/n/a sbom run/advisory/n/a/n/a values run/run/run/n/a itsm run/run/run/n/a api-compat advisory/advisory/n/a/n/a qa advisory/advisory/n/a/n/a policy-validation run/run/run/n/a agent-gates run/run/run/run clearance-gate advisory/advisory/advisory/n/a "
expect "release-guardrails matrix, cluster overrides compose (qa, api-compat required; sbom, values advisory)" "$(matrix "${tmp}/override.yaml" | tr '\n' ' ')" \
  "sast run/advisory/n/a/n/a image-scan run/advisory/n/a/n/a provenance run/run/run/n/a sbom advisory/advisory/n/a/n/a values advisory/advisory/advisory/n/a itsm run/run/run/n/a api-compat run/advisory/n/a/n/a qa run/advisory/n/a/n/a policy-validation advisory/advisory/advisory/n/a agent-gates run/run/run/run clearance-gate advisory/advisory/advisory/n/a "
expect "release-guardrails matrix, clearance-gate required" "$(matrix "${tmp}/cgreq.yaml" | grep '^clearance-gate ')" "clearance-gate run/run/run/n/a"
expect "release-guardrails matrix, Clearance App checks run on a no-op PR" "$(matrix "${tmp}/cc.yaml" | grep '^agent-' | tr '\n' ' ')" \
  "agent-identity run/run/run/run agent-scope run/run/run/run "
render promo -f "${tmp}/promotion-values.yaml" --set registries.flight.namespace=riley-roooo
expect "release-guardrails matrix, image-promotion with promotion on" "$(matrix "${tmp}/promo.yaml" | grep '^image-promotion ')" "image-promotion run/run/n/a/n/a"
expect "release-guardrails matrix, per-app advisory is listed, not composed" \
  "$(python3 -c 'import sys,yaml,json; [print(json.dumps(json.loads(d["data"]["matrix.json"])["advisoryByApp"])) for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind")=="ConfigMap" and d["metadata"]["name"]=="release-guardrails"]' "${tmp}/perapp.yaml")" \
  '{"backstage": ["image-scan"]}'
expect "release-guardrails matrix, per-app image-scan still runs in the table" "$(matrix "${tmp}/perapp.yaml" | grep '^image-scan ')" "image-scan run/advisory/n/a/n/a"
python3 - "${tmp}/default.yaml" <<'PY' || fail=1
import sys, yaml
docs = {(d["kind"], d["metadata"]["name"]): d for d in yaml.safe_load_all(open(sys.argv[1])) if d}
role = docs.get(("Role", "release-guardrails-read")); rb = docs.get(("RoleBinding", "release-guardrails-read"))
ok = role and role["rules"] == [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["release-guardrails"], "verbs": ["get"]}] \
    and rb and rb["roleRef"]["name"] == "release-guardrails-read" and rb["subjects"][0]["kind"] == "Group"
if not ok: print("FAIL: release-guardrails read-only Role/RoleBinding"); sys.exit(1)
PY

# the generated contract (tools/gen_contract.py, its own copy of the class defaults) states the same rows the chart renders
python3 - "${tmp}/default.yaml" "${tmp}/cc.yaml" contract/glidepath-contract.json <<'PY' || fail=1
import sys, yaml, json
contract = {g["name"]: g["kinds"] for g in json.load(open(sys.argv[3]))["gates"]}
seen = set()
for path in sys.argv[1:3]:
    for d in yaml.safe_load_all(open(path)):
        if d and d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "release-guardrails":
            for g in json.loads(d["data"]["matrix.json"])["gates"]:
                seen.add(g["name"])
                if contract.get(g["name"]) != g["kinds"]:
                    print("FAIL: contract kinds for %s %s, chart renders %s" % (g["name"], contract.get(g["name"]), g["kinds"])); sys.exit(1)
if seen != set(contract):
    print("FAIL: contract gates %s, chart gates %s" % (sorted(contract), sorted(seen))); sys.exit(1)
PY

# ---- CG-2 locks and refusals: a registry row that weakens an image release, or names an unknown kind, value or class,
# fails the render. The registry is replaced whole here (yq on the chart default), the only way to set an entry's kinds.
kinds_refuse() { # label want-in-message yq-expression-on-releaseGuardrails
  yq -o yaml "{\"releaseGuardrails\": (.releaseGuardrails | $3)}" charts/glidepath-catalog/values.yaml > "${tmp}/kinds.yaml"
  if helm template t charts/glidepath-catalog -f "${values}" -f "${tmp}/kinds.yaml" --namespace platform-catalog > /dev/null 2> "${tmp}/err"; then
    echo "FAIL: kinds ${1} rendered"; fail=1
  elif ! grep -qF -- "$2" "${tmp}/err"; then
    echo "FAIL: kinds ${1} refusal has the wrong message: $(cat "${tmp}/err")"; fail=1
  fi
}
kinds_refuse sast-advisory "sast must be run for image-release" '(.[] | select(.name == "sast") | .kinds) = {"image-release": "advisory"}'
kinds_refuse provenance-advisory "provenance must be run for image-release" '(.[] | select(.name == "provenance") | .kinds) = {"image-release": "advisory"}'
kinds_refuse provenance-rollback "provenance cannot be n/a for rollback" '(.[] | select(.name == "provenance") | .kinds) = {"rollback": "n/a"}'
kinds_refuse provenance-config "provenance cannot be n/a for config" '(.[] | select(.name == "provenance") | .kinds) = {"config": "n/a"}'
kinds_refuse any-gate-na "values cannot be n/a for image-release" '(.[] | select(.name == "values") | .kinds) = {"image-release": "n/a"}'
kinds_refuse advisory-gate-na "qa cannot be n/a for image-release" '(.[] | select(.name == "qa") | .kinds) = {"image-release": "n/a"}'
kinds_refuse unknown-kind '"hotfix" is not a change kind' '(.[] | select(.name == "sbom") | .kinds) = {"hotfix": "run"}'
kinds_refuse unknown-value '"skip" is not one of run, advisory, n/a' '(.[] | select(.name == "sbom") | .kinds) = {"config": "skip"}'
kinds_refuse bool-value '"true" is not one of run, advisory, n/a' '(.[] | select(.name == "sbom") | .kinds) = {"config": true}'
kinds_refuse not-a-map "sbom.kinds must be a map" '(.[] | select(.name == "sbom") | .kinds) = ["config"]'
kinds_refuse unknown-class 'sbom has class "contents"' '(.[] | select(.name == "sbom") | .class) = "contents"'
refuse cluster-kinds "releaseGuardrailOverrides.sbom.kinds: a gate's change kinds are set in the registry entry" \
  $'releaseGuardrailOverrides:\n  sbom: { kinds: { config: run } }'
# entries and override keys this cluster leaves out are checked too: agent-identity is not in the effective registry
# without Clearance's App, and image-promotion is not a registry entry at all (promotion off here)
kinds_refuse excluded-entry 'agent-identity.kinds: "hotfix" is not a change kind' '(.[] | select(.name == "agent-identity") | .kinds) = {"hotfix": "run"}'
kinds_refuse excluded-entry-lock "agent-scope cannot be n/a for image-release" '(.[] | select(.name == "agent-scope") | .kinds) = {"image-release": "n/a"}'
refuse cluster-kinds-image-promotion "releaseGuardrailOverrides.image-promotion.kinds: a gate's change kinds are set in the registry entry" \
  $'releaseGuardrailOverrides:\n  image-promotion: { kinds: { config: run } }'
refuse cluster-kinds-excluded "releaseGuardrailOverrides.agent-identity.kinds: a gate's change kinds are set in the registry entry" \
  $'releaseGuardrailOverrides:\n  agent-identity: { kinds: { no-op: n/a } }'
# an allowed row change renders and shows: image-scan run on a config change
yq -o yaml '{"releaseGuardrails": (.releaseGuardrails | (.[] | select(.name == "image-scan") | .kinds) = {"config": "run"})}' \
  charts/glidepath-catalog/values.yaml > "${tmp}/kinds-ok.yaml"
render kindsok -f "${tmp}/kinds-ok.yaml"
expect "an entry's kinds override its class default" "$(matrix "${tmp}/kindsok.yaml" | grep '^image-scan ')" "image-scan run/advisory/run/n/a"
# behaviour unchanged until CG-3: no gate Pipeline reads the matrix yet
expect "image-scan-check unchanged by its kinds" "$(tasks image-scan-check "${tmp}/kindsok.yaml" | tr '\n' ' ')" \
  "extract-promoted-image image-scan+when image-scan-advisory+continue+when not-applicable+when advisory-reason=rollback "

rm -rf "${tmp}"
[[ ${fail} -eq 0 ]] && echo "OK: the registry's required field drives every gate, the bypass tooling and the ruleset"
exit ${fail}
