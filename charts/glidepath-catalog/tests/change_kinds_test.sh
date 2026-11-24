#!/usr/bin/env bash
# Change-aware release gates, CG-3 (docs/admin/release-guardrails.md "Change kinds"; hangar
# specs/plans/change-aware-release-gates.md sections 6.1 and 6.2, CG-9's pipeline-level cases):
#   1. every gate Pipeline runs the classify step in its first task (provenance: a classify task of its own), its hard tasks
#      only `when` applicability is run, its -advisory copies only when advisory, and a `not-applicable` task when n/a; no task
#      decides on rollback-eligible any more;
#   2. the classify StepAction, run against a fake glidepath-api, answers what the matrix says for every gate and every kind,
#      narrows values to n/a on a config change with no values file (and only with the fact present), judges a config change
#      that needs a render check and any doubt as an image release, refuses two new images where a content gate runs (Q8),
#      and never asks for a repository that is not the app's gitops repo;
#   3. extract-promoted-image keeps its own rollback rule as the fallback when glidepath-api did not answer;
#   4. the registry's `narrow` refusals;
#   5. comment-pr-check-result: an n/a gate posts the shared "Change classification" comment and no comment of its own, the
#      comment is the same whoever writes it, is not rewritten when unchanged, and is left alone for an older head.
# Needs helm, python3 (pyyaml), yq (mikefarah v4) and jq. Usage: charts/glidepath-catalog/tests/change_kinds_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$work"' EXIT
values=charts/glidepath-catalog/tests/values.example.yaml

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1:"; echo "  got:  $2"; echo "  want: $3"; fail=1; fi
}
render() { # <out> [helm args...]
  local out="$1"; shift
  helm template t charts/glidepath-catalog -f "${values}" --namespace platform-catalog "$@" > "${out}" 2> "${out}.err"
}

# ---- 1. the gate Pipelines -------------------------------------------------------------------------------------------
render "$work/default.yaml" || { echo "FAIL: render: $(cat "$work/default.yaml.err")"; exit 1; }
render "$work/itsm.yaml" --set itsm.provider=servicenow || { echo "FAIL: render itsm: $(cat "$work/itsm.yaml.err")"; exit 1; }
render "$work/required.yaml" --set releaseGuardrailOverrides.qa.required=true --set releaseGuardrailOverrides.clearance-gate.required=true \
  || { echo "FAIL: render required: $(cat "$work/required.yaml.err")"; exit 1; }

# shape RENDERED PIPELINE -> "first=<task>:<classify-gate> <task>[+continue]@<applicability values>|..." one line
shape() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml
path, want = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(path)):
    if not (d and d.get("kind") == "Pipeline" and d["metadata"]["name"] == want):
        continue
    tasks = d["spec"]["tasks"]
    out = []
    first = tasks[0]
    gate = [p["value"] for p in first.get("params", []) if p["name"] == "classify-gate"]
    out.append("first=%s:%s" % (first["name"], gate[0] if gate else "-"))
    for t in tasks[1:]:
        vals = []
        for w in t.get("when", []):
            if "rollback-eligible" in w["input"]:
                vals.append("ROLLBACK-ELIGIBLE")
            if w["input"].endswith(".results.applicability)"):
                vals.append(",".join(w["values"]))
        out.append("%s%s@%s" % (t["name"], "+continue" if t.get("onError") == "continue" else "", "/".join(vals) or "-"))
    print(" ".join(out))
PY
}
expect "sast-check" "$(shape "$work/default.yaml" sast-check)" \
  "first=extract-promoted-image:sast verify-sast-attestation@run verify-sast-attestation-advisory+continue@advisory not-applicable@n/a"
expect "image-scan-check" "$(shape "$work/default.yaml" image-scan-check)" \
  "first=extract-promoted-image:image-scan image-scan@run image-scan-advisory+continue@advisory not-applicable@n/a"
expect "sbom-check" "$(shape "$work/default.yaml" sbom-check)" \
  "first=extract-promoted-image:sbom generate-sbom@run generate-sbom-advisory+continue@advisory not-applicable@n/a"
expect "qa-check (advisory by registry)" "$(shape "$work/default.yaml" qa-check)" \
  "first=extract-promoted-image:qa verify-check-ride-advisory+continue@advisory not-applicable@n/a"
expect "qa-check (required)" "$(shape "$work/required.yaml" qa-check)" \
  "first=extract-promoted-image:qa verify-check-ride@run verify-check-ride-advisory+continue@advisory not-applicable@n/a"
expect "api-compat-check (advisory by registry)" "$(shape "$work/default.yaml" api-compat-check)" \
  "first=extract-promoted-image:api-compat compare-api-advisory+continue@advisory not-applicable@n/a"
expect "provenance-check" "$(shape "$work/default.yaml" provenance-check)" \
  "first=classify:provenance verify@run verify-provenance@run not-applicable@n/a"
expect "values-check" "$(shape "$work/default.yaml" values-check)" \
  "first=fetch:values render@run,advisory validate@run,advisory not-applicable@n/a"
expect "policy-validation-check" "$(shape "$work/default.yaml" policy-validation-check)" \
  "first=fetch:policy-validation render@run,advisory evaluate@run,advisory not-applicable@n/a"
expect "clearance-gate-check (advisory by registry)" "$(shape "$work/default.yaml" clearance-gate-check)" \
  "first=fetch:clearance-gate render@run,advisory register-advisory+continue@run,advisory render-check@run,advisory not-applicable@n/a"
expect "clearance-gate-check (required)" "$(shape "$work/required.yaml" clearance-gate-check)" \
  "first=fetch:clearance-gate render@run,advisory register@run,advisory not-applicable@n/a"
expect "itsm-check (a change tool)" "$(shape "$work/itsm.yaml" itsm-check)" \
  "first=verify-change:itsm not-applicable@n/a"
expect "itsm-check (the stub is not classified)" "$(shape "$work/default.yaml" itsm-check)" "first=check:-"
expect "no task decides on rollback-eligible" "$(grep -c 'results.rollback-eligible)' "$work/default.yaml")" "0"
# The content gates ask the classify step to refuse two new images; the others do not.
expect "content gates refuse several images" "$(yq 'select(.kind == "Pipeline") | .spec.tasks[0].params[]? | select(.name == "classify-refuse-multiple-images") | .value' "$work/default.yaml" | grep -v '^---$' | sort | uniq -c | tr -s ' ')" " 5 true"
# The first tasks host the step first, and every later fetch-side step carries the n/a exit.
for t in extract-promoted-image validate-values policy-validation-gate clearance-gate itsm-gate classify-change; do
  expect "$t: classify is the first step" "$(yq "select(.kind == \"Task\" and .metadata.name == \"$t\") | .spec.steps[0].name" "$work/itsm.yaml")" "classify"
done
for ts in validate-values:clone-and-list validate-values:publish validate-values:validate policy-validation-gate:fetch policy-validation-gate:publish \
          clearance-gate:fetch clearance-gate:publish itsm-gate:verify; do
  t="${ts%%:*}" st="${ts##*:}"
  expect "$t/$st reads the applicability" \
    "$(yq "select(.kind == \"Task\" and .metadata.name == \"$t\") | .spec.steps[] | select(.name == \"$st\") | .env[] | select(.name == \"GATE_APPLICABILITY\") | .value" "$work/itsm.yaml")" \
    '$(steps.classify.results.applicability)'
  expect "$t/$st exits on n/a" \
    "$(yq "select(.kind == \"Task\" and .metadata.name == \"$t\") | .spec.steps[] | select(.name == \"$st\") | .script" "$work/itsm.yaml" | grep -c 'GATE_APPLICABILITY:-run}" != "n/a" \]\]\|GATE_APPLICABILITY:-run}" == "n/a" \]\]')" "1"
done

# ---- 2. the classify step ------------------------------------------------------------------------------------------
yq 'select(.kind == "StepAction" and .metadata.name == "classify-change") | .spec.script' "$work/default.yaml" > "$work/classify.sh"
[[ -s "$work/classify.sh" ]] || { echo "FAIL: no classify-change StepAction rendered"; exit 1; }
mkdir -p "$work/res" "$work/bin" "$work/lib" "$work/sa"
for r in change-kind applicability change-reason gate-table classification-source rollback-applicability classification-reason; do
  sed -i.bak "s#\$(step.results.${r}.path)#${work}/res/${r}#g" "$work/classify.sh"
done
grep -q 'step.results' "$work/classify.sh" && { echo "FAIL: an unreplaced result path in the classify script"; fail=1; }
echo sa-token > "$work/sa/token"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
out="" url=""
while (( $# )); do
  case "$1" in
    -o) out="$2"; shift ;;
    -w|-H|--max-time|--retry|-X|-d) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
echo "${url}" >> "$FX/calls"
case "${url}" in
  */commits/*/pulls) echo '[{"number": 7}]'; exit 0 ;;
esac
[[ "${API_CODE}" == 000 ]] && exit 7
printf '%s' "${API_BODY}" > "${out}"
printf '%s' "${API_CODE}"
EOF
chmod +x "$work/bin/curl"
H=2222222222222222222222222222222222222222
cls() { # <kind> <envs json> [renderCheck] -> a stored classification of head H
  jq -nc --arg k "$1" --argjson e "$2" --arg rc "${3:-none}" --arg h "$H" \
    '{v: 1, classifier: "glidepath-api/changekind 2", repo: "acme/gitops-shop", pr: 7, head: $h, kind: $k, renderCheck: $rc,
      reason: "prod-1/staging: what changed", envs: $e, outcome: "stored"}'
}
env1() { # <fileClasses json|none> [base images] [head images]
  jq -nc --arg fc "$1" --argjson b "${2:-[]}" --argjson h "${3:-[]}" \
    '{cluster: "prod-1", env: "staging", kind: "x", images: {base: $b, head: $h}} + (if $fc == "none" then {} else {fileClasses: ($fc | fromjson)} end)'
}
classify() { # <gate> <api code> <api body> [repo] [pr] -> "<kind> <applicability> <source> rc=<rc>"; FX/calls
  FX="$work/fx"; rm -rf "$FX"; mkdir -p "$FX"; : > "$FX/calls"; rm -f "$work/res/"*
  ( PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" FX="$FX" API_CODE="$2" API_BODY="$3" GATE="$1" \
      REPO_URL="${4:-https://github.com/acme/gitops-shop}" REVISION="$H" PR_NUMBER="${5:-7}" NAMESPACE=app-shop-cicd \
      REFUSE_MULTIPLE_IMAGES="$( [[ "$1" =~ ^(sast|image-scan|sbom|qa|api-compat)$ ]] && echo true || echo false )" \
      GLIDEPATH_API_URL=http://api TOKEN_PATH="$work/sa/token" GITHUB_TOKEN_BROKER_URL=http://broker bash "$work/classify.sh" ) > "$FX/out" 2>&1
  local rc=$?
  echo "$(cat "$work/res/change-kind" 2>/dev/null) $(cat "$work/res/applicability" 2>/dev/null) $(cat "$work/res/classification-source" 2>/dev/null) rc=${rc}"
}
answers() { # <api body> -> every gate's applicability, space separated, in a fixed order
  local g out=""
  for g in sast image-scan sbom api-compat qa provenance values policy-validation clearance-gate itsm; do
    classify "$g" 200 "$1" >/dev/null
    out+="${g}=$(cat "$work/res/applicability") "
  done
  echo "${out% }"
}
IR="$(cls image-release "[$(env1 '["values"]' '["ghcr.io/acme/shop:1.0"]' '["ghcr.io/acme/shop:1.1"]')]")"
RB="$(cls rollback "[$(env1 '["values"]' '["ghcr.io/acme/shop:1.1"]' '["ghcr.io/acme/shop:1.0"]')]")"
CFGV="$(cls config "[$(env1 '["values"]')]")"
CFGM="$(cls config "[$(env1 '["manifest"]')]")"
CFG1="$(cls config "[$(env1 none)]")"
CFGR="$(cls config "[$(env1 '["chart"]')]" required)"
NOOP="$(cls no-op "[]")"
expect "image-release" "$(answers "$IR")" \
  "sast=run image-scan=run sbom=run api-compat=advisory qa=advisory provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "rollback" "$(answers "$RB")" \
  "sast=advisory image-scan=advisory sbom=advisory api-compat=advisory qa=advisory provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "config, a values file" "$(answers "$CFGV")" \
  "sast=n/a image-scan=n/a sbom=n/a api-compat=n/a qa=n/a provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "config, manifests only: values narrows to n/a" "$(answers "$CFGM")" \
  "sast=n/a image-scan=n/a sbom=n/a api-compat=n/a qa=n/a provenance=run values=n/a policy-validation=run clearance-gate=advisory itsm=run"
expect "config stored by changekind 1 (no fileClasses): never narrows" "$(answers "$CFG1")" \
  "sast=n/a image-scan=n/a sbom=n/a api-compat=n/a qa=n/a provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "config that needs a render check: judged as an image release" "$(answers "$CFGR")" \
  "sast=run image-scan=run sbom=run api-compat=advisory qa=advisory provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "no-op" "$(answers "$NOOP")" \
  "sast=n/a image-scan=n/a sbom=n/a api-compat=n/a qa=n/a provenance=n/a values=n/a policy-validation=n/a clearance-gate=n/a itsm=n/a"
RBR="$(cls rollback "[$(env1 '["values","chart"]' '["ghcr.io/acme/shop:1.1"]' '["ghcr.io/acme/shop:1.0"]')]" required)"
expect "a rollback that needs a render check: judged as an image release" "$(answers "$RBR")" \
  "sast=run image-scan=run sbom=run api-compat=advisory qa=advisory provenance=run values=run policy-validation=run clearance-gate=advisory itsm=run"
expect "an unknown kind is judged as an image release" "$(classify sast 200 "$(cls future-kind "[]")" | cut -d' ' -f2)" "run"

classify values 200 "$CFGM" >/dev/null
expect "values n/a names its reason" "$(cat "$work/res/change-reason")" \
  "not applicable: no values, release or base file changed (config: prod-1/staging: what changed)"
expect "the asked route" "$(cat "$work/fx/calls")" "http://api/v1/apps/shop/pulls/7/classification?head=${H}"
expect "the gate table (agent-gates runs on every kind; no Clearance App checks, no image-promotion)" "$(cat "$work/res/gate-table")" \
  '{"sast":"n/a:kind:config","image-scan":"n/a:kind:config","provenance":"run","sbom":"n/a:kind:config","values":"n/a:no-values-file","itsm":"run","api-compat":"n/a:kind:config","qa":"n/a:kind:config","policy-validation":"run","agent-gates":"run","clearance-gate":"advisory"}'
expect "the classification's own reason" "$(cat "$work/res/classification-reason")" "prod-1/staging: what changed"

# Q8: two distinct new images - refused where a content gate runs, not where it advises.
TWO="$(cls image-release "[$(env1 '["values"]' '[]' '["ghcr.io/acme/shop:1.1"]'),$(env1 '["manifest"]' '[]' '["ghcr.io/acme/worker:2"]')]")"
expect "Q8: sast refuses two new images" "$(classify sast 200 "$TWO" | cut -d' ' -f4) $(grep -c "more than one distinct new image" "$work/fx/out")" "rc=1 1"
classify qa 200 "$TWO" >/dev/null
expect "Q8: qa (advisory) does not refuse" "$(grep -c "more than one distinct new image" "$work/fx/out")" "0"
SAME="$(cls image-release "[$(env1 '["values"]' '[]' '["ghcr.io/acme/shop:1.1"]'),$(env1 '["values"]' '[]' '["ghcr.io/acme/shop:1.1"]')]")"
classify sast 200 "$SAME" >/dev/null
expect "Q8: one image in two environments is one image" "$(grep -c "more than one distinct new image" "$work/fx/out")" "0"

# Fail closed: glidepath-api says no, cannot be reached, or answers for another head.
expect "403: fallback, image release" "$(classify sast 403 '{"error": "no"}')" "image-release run fallback rc=0"
expect "unreachable: fallback" "$(classify values 000 '')" "image-release run fallback rc=0"
expect "fallback says why" "$(grep -c 'FALLBACK - glidepath-api could not be reached' "$work/fx/out")" "1"
expect "an answer for another head: fallback" "$(classify sast 200 "$(cls config '[]' | jq -c '.head = "3333333333333333333333333333333333333333"')")" "image-release run fallback rc=0"
# A release pin PR on the app's source repository (ADR-0020): nothing is asked, every gate runs.
expect "a source-repo pin PR is not classified" "$(classify sast 200 "$CFGM" https://github.com/acme/shop)" "image-release run fallback rc=0"
expect "a source-repo pin PR asks nothing" "$(wc -l < "$work/fx/calls" | tr -d ' ')" "0"
# A retest hands over PaC's template text: the PR is found from the commit.
classify sast 200 "$CFGM" https://github.com/acme/gitops-shop '{{ pull_request_number }}' >/dev/null
expect "an unsubstituted PR number is resolved from the commit" "$(tail -1 "$work/fx/calls")" "http://api/v1/apps/shop/pulls/7/classification?head=${H}"
# No gate named: nothing asked, run.
classify "" 200 "$CFGM" >/dev/null
expect "no gate: run, nothing asked" "$(cat "$work/res/applicability") $(wc -l < "$work/fx/calls" | tr -d ' ')" "run 0"

# ---- 3. extract-promoted-image's rollback step ------------------------------------------------------------------------
yq 'select(.kind == "Task" and .metadata.name == "extract-promoted-image") | .spec.steps[] | select(.name == "rollback-eligibility") | .script' "$work/default.yaml" > "$work/rb.sh"
for r in eligible note change-kind applicability change-reason; do sed -i.bak "s#\$(step.results.${r}.path)#${work}/res/rb-${r}#g" "$work/rb.sh"; done
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo kubectl >> "$FX/calls"
jq -nc '{items: [{data: {releaseId: "r3", image: "ghcr.io/acme/shop:1.0", healthyAt: "2026-11-01T00:00:00Z"}}]}'
EOF
chmod +x "$work/bin/kubectl"
rb() { # <source> <kind> <applicability> -> "<eligible> <kind> <applicability>"
  FX="$work/fx"; : > "$FX/calls"
  ( PATH="$work/bin:$PATH" FX="$FX" CLASSIFICATION_SOURCE="$1" CHANGE_KIND="$2" GATE_APPLICABILITY="$3" CHANGE_REASON="why" \
      ROLLBACK_APPLICABILITY=advisory IMAGE_REF=ghcr.io/acme/shop:1.0 MANIFEST=prod-1/staging/release.yaml \
      REPO_URL=https://github.com/acme/gitops-shop NAMESPACE=app-shop-cicd bash "$work/rb.sh" ) > /dev/null 2>&1
  echo "$(cat "$work/res/rb-eligible") $(cat "$work/res/rb-change-kind") $(cat "$work/res/rb-applicability")"
}
expect "fallback + an eligible rollback here: the gate's rollback value" "$(rb fallback image-release run)" "true rollback advisory"
expect "api rollback: eligible, as classified, no records read" "$(rb api rollback advisory) $(wc -l < "$work/fx/calls" | tr -d ' ')" "true rollback advisory 0"
expect "api image release: not eligible, no records read" "$(rb api image-release run) $(wc -l < "$work/fx/calls" | tr -d ' ')" "false image-release run 0"
expect "n/a: nothing read" "$(rb api config n/a) $(wc -l < "$work/fx/calls" | tr -d ' ')" "false config n/a 0"

# ---- 4. the registry's narrow refusals -------------------------------------------------------------------------------
refused() { # <name> <want in error> <yaml for the values file>
  printf '%s\n' "$3" > "$work/bad.yaml"
  if helm template t charts/glidepath-catalog -f "${values}" -f "$work/bad.yaml" > /dev/null 2> "$work/bad.err"; then
    echo "FAIL $1: rendered"; fail=1
  else
    grep -q -- "$2" "$work/bad.err" && echo "ok   $1" || { echo "FAIL $1: $(cat "$work/bad.err")"; fail=1; }
  fi
}
# A registry entry is replaced whole by a values file's list, so these rewrite one entry through yq on the chart default.
entry() { # <gate> <yq update> -> a releaseGuardrails list
  yq -o=yaml "{\"releaseGuardrails\": (.releaseGuardrails | map(select(.name == \"$1\") |= ($2)))}" charts/glidepath-catalog/values.yaml
}
refused "narrow on sast" "sast cannot narrow" "$(entry sast '.narrow = {"config": ["no-values-file"]}')"
refused "narrow on an image release" "no gate is n/a for an image release" "$(entry values '.narrow = {"image-release": ["no-values-file"]}')"
refused "narrow from n/a" "narrowing turns run into n/a" "$(entry image-scan '.narrow = {"config": ["no-values-file"]}')"
refused "an unknown reason" "is not a narrowing reason" "$(entry values '.narrow = {"config": ["unprotected"]}')"
refused "an empty list" "non-empty list" "$(entry values '.narrow = {"config": []}')"
refused "narrow per cluster" "releaseGuardrailOverrides.values.narrow" "releaseGuardrailOverrides: { values: { narrow: { config: [no-values-file] } } }"
refused "a single-path gate that runs and advises" "one path for its check" "$(entry values '.kinds = {"rollback": "advisory"}')"
refused "a required single-path gate whose row only advises" "is required but its kinds row advises" \
  "$(entry clearance-gate '.required = true | .kinds = {"image-release": "advisory", "rollback": "advisory", "config": "advisory"}')"
expect "the matrix publishes narrow" "$(yq 'select(.metadata.name == "release-guardrails") | .data["matrix.json"]' "$work/default.yaml" | jq -c '[.gates[] | select(.narrow != {}) | {(.name): .narrow}], .narrowReasons')" \
  '[{"values":{"config":["no-values-file"]}}]
{"no-values-file":"no values, release or base file changed"}'

# ---- 5. the shared Change classification comment ---------------------------------------------------------------------
taskruns() { # <gate> <classification body> <api code> -> the first TaskRun's results, from a classify run
  classify "$1" "${3:-200}" "$2" >/dev/null
  jq -nc --arg k "$(cat "$work/res/change-kind")" --arg a "$(cat "$work/res/applicability")" --arg r "$(cat "$work/res/change-reason")" \
    --arg t "$(cat "$work/res/gate-table")" --arg s "$(cat "$work/res/classification-source")" --arg cr "$(cat "$work/res/classification-reason")" \
    '{items: [{metadata: {name: "tr-1", labels: {"tekton.dev/pipelineTask": "extract-promoted-image"}}, status: {conditions: [{status: "True"}],
       results: [{name: "change-kind", value: $k}, {name: "applicability", value: $a}, {name: "change-reason", value: $r},
                 {name: "gate-table", value: $t}, {name: "classification-source", value: $s}, {name: "classification-reason", value: $cr}]}}]}'
}
# The first TaskRun of each case, from the classify step against the fake glidepath-api (before its curl is replaced below).
TR_NA_SAST="$(taskruns sast "$CFGM")"; TR_NA_VALUES="$(taskruns values "$CFGM")"; TR_RUN_PROV="$(taskruns provenance "$CFGM")"
TR_IR_SAST="$(taskruns sast "$IR")"; TR_FB_SAST="$(taskruns sast "" 403)"
yq 'select(.kind == "Task" and .metadata.name == "comment-pr-check-result") | .spec.steps[] | select(.name == "comment") | .script' "$work/default.yaml" > "$work/cpr.sh"
narrow_reasons="$(yq 'select(.kind == "Task" and .metadata.name == "comment-pr-check-result") | .spec.steps[] | select(.name == "comment") | .env[] | select(.name == "NARROW_REASONS") | .value' "$work/default.yaml")"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
method=GET url=""
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -d) printf '%s\n' "$2" >> "$FX/bodies"; shift ;;
    -H|--max-time|--retry|-o) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
echo "$method $url" >> "$FX/calls"
case "$method $url" in
  "GET "*/issues/7/comments*) cat "$FX/comments" ;;
  "GET "*/pulls/7) jq -nc --arg h "$PR_HEAD" '{head: {sha: $h}}' ;;
  "POST "*/comments|"PATCH "*/comments/*) echo '{"html_url": "https://github.com/acme/gitops-shop/pull/7#c"}' ;;
  "DELETE "*) echo '{}' ;;
  *) echo "unexpected $method $url" >&2; exit 22 ;;
esac
EOF
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "get taskrun"*) cat "$FX/taskruns" ;;
  "get configmap"*) echo '{"items": []}' ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/"*
comment() { # <name> <gate> <taskruns json> <comments json> [pr head]
  FX="$work/c-$1"; mkdir -p "$FX/gate"; : > "$FX/calls"; : > "$FX/bodies"
  printf '%s' "$3" > "$FX/taskruns"; printf '%s' "$4" > "$FX/comments"
  ( PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GATE_WORK="$FX/gate" FX="$FX" PR_HEAD="${5:-$H}" REPO_URL=https://github.com/acme/gitops-shop \
      PR_NUMBER=7 REVISION="$H" GATE_NAME="$2" PIPELINE_RUN_NAME="$2-x1" STATUS=Succeeded ADVISORY_REASON=rollback NAMESPACE=app-shop-cicd \
      POST_COMMENT=true NARROW_REASONS="${narrow_reasons}" bash "$work/cpr.sh" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}
comment na sast "${TR_NA_SAST}" '[]'
expect "n/a: exit 0" "$(cat "$work/c-na/rc")" "0"
expect "n/a: verdict success (n/a in the ledger is CG-4)" "$(cat "$work/c-na/gate/verdict" 2>/dev/null)" "success"
expect "n/a: one comment posted, the shared one" "$(grep -c '^POST ' "$work/c-na/calls")" "1"
bodies() { jq -s -r "$2" "$work/c-$1/bodies"; } # the JSON bodies a run sent, as one array
expect "n/a: the shared comment's marker" "$(bodies na '.[0].body' | head -1)" "<!-- glidepath:change-classification -->"
shared="$(bodies na '.[0].body')"
expect "n/a: names the n/a gates" "$(grep -c '^\*\*Not applicable\*\* (config): sast, image-scan, sbom, values, api-compat, qa\.' <<<"${shared}")" "1"
expect "n/a: one line per Glidepath gate" "$(grep -c '^| `' <<<"${shared}")" "11"
expect "n/a: values says why" "$(grep -c '^| `values` | \*\*n/a\*\*: no values, release or base file changed |$' <<<"${shared}")" "1"
# Another gate of the same head writes the same text; an unchanged comment is not rewritten.
comment same values "${TR_NA_VALUES}" "$(jq -nc --arg b "${shared}" '[{id: 5, body: $b}]')"
expect "the same text from another gate: no write" "$(grep -c '^\(POST\|PATCH\) ' "$work/c-same/calls")" "0"
# A gate that runs, on a head with n/a gates: it keeps its own comment and writes the same shared text.
comment run provenance "${TR_RUN_PROV}" '[]'
expect "a running gate: its own comment and the shared one" "$(grep -c '^POST ' "$work/c-run/calls")" "2"
expect "a running gate writes the same shared text" "$(bodies run '[.[] | select(.body | startswith("<!-- glidepath:change-classification -->"))] | length')" "1"
expect "the shared text does not depend on which gate wrote it" "$(bodies run '.[] | select(.body | startswith("<!-- glidepath:change-classification -->")) | .body')" "${shared}"
# An older head finishing late leaves the newer answer alone.
comment old sast "${TR_NA_SAST}" '[]' 4444444444444444444444444444444444444444
expect "an older head: no shared comment" "$(grep -c '^POST ' "$work/c-old/calls")" "0"
# An image release has no n/a gate: no shared comment, unless one exists from an earlier head (then it is brought up to date).
comment ir sast "${TR_IR_SAST}" '[]'
expect "an image release: no shared comment" "$(grep -c 'change-classification' "$work/c-ir/bodies")" "0"
comment irold sast "${TR_IR_SAST}" '[{"id": 9, "body": "<!-- glidepath:change-classification -->\nold"}]'
expect "an image release after a config head: the shared comment is updated" "$(grep -c '^PATCH .*/issues/comments/9$' "$work/c-irold/calls")" "1"
# A gate n/a with its own comment from an earlier head: that comment is edited to say n/a.
comment naown sast "${TR_NA_SAST}" '[{"id": 3, "body": "<!-- glidepath:gate:sast -->\n### ❌ `sast` failed"}]'
expect "n/a with an earlier comment of its own: edited" "$(grep -c '^PATCH .*/issues/comments/3$' "$work/c-naown/calls")" "1"
expect "n/a with an earlier comment of its own: says n/a" "$(grep -c 'not applicable at' "$work/c-naown/bodies")" "1"
# A decide-phase TaskRun of the same Task (classify-gate "", source none, applicability run) listed first does not hide the
# fetch TaskRun's answer.
decide='{"metadata": {"name": "tr-0", "labels": {"tekton.dev/pipelineTask": "validate"}}, "status": {"conditions": [{"status": "True"}], "results": [{"name": "change-kind", "value": ""}, {"name": "applicability", "value": "run"}, {"name": "classification-source", "value": "none"}]}}'
comment decide values "$(jq -c --argjson d "${decide}" '.items = [$d] + .items' <<<"${TR_NA_VALUES}")" '[]'
expect "a decide TaskRun first: still n/a, no own comment" "$(grep -c 'no comment of its own' "$work/c-decide/out")" "1"
# The fallback never writes the shared comment (it is not glidepath-api's answer).
comment fb sast "${TR_FB_SAST}" '[]'
expect "fallback: no shared comment" "$(grep -c 'change-classification' "$work/c-fb/bodies")" "0"

[[ ${fail} -eq 0 ]] && echo "PASS change_kinds_test" || { echo "FAIL change_kinds_test"; exit 1; }
