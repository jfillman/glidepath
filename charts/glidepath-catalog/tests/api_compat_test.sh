#!/usr/bin/env bash
# api-compat-gate.yaml's script against stub GitHub/registry/Prometheus (curl), cosign and kubectl, with the real
# oasdiff: every verdict, the owner override, usage, the JSON line Tower parses, and truncation. Needs helm, yq, jq
# and oasdiff on PATH.
# Usage: charts/glidepath-catalog/tests/api_compat_test.sh
set -u
cd "$(dirname "$0")/../../.."
command -v oasdiff >/dev/null || { echo "FAIL: oasdiff not on PATH"; exit 1; }
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --set apiCompat.usage.prod-cluster.url=http://prom.example \
  --show-only templates/tasks/api-compat-gate.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
yq '.spec.steps[0].script' "$d/task.yaml" > "$d/script.raw"
yq -o=json '.spec.steps[0].env' "$d/task.yaml" > "$d/env.json"
mkdir -p "$d/bin" "$d/lib" "$d/fx"
printf 'github_app_installation_token() { echo "tok-$2"; }\n' > "$d/lib/github-app.sh"

# curl: dispatch on the URL. Scenario files live in $FX.
cat > "$d/bin/curl" <<'EOF'
#!/usr/bin/env bash
url=""; data=""
for a in "$@"; do case "$a" in http*) url="$a" ;; query=*) data="${a#query=}" ;; esac; done
case "$url" in
  */commits/*/pulls) echo '[{"number":7}]' ;;
  */pulls/7) echo '{"base":{"sha":"basesha"},"created_at":"2026-10-01T00:00:00Z"}' ;;
  */commits/*) echo '{"commit":{"committer":{"date":"2026-10-20T00:00:00Z"}}}' ;;
  */contents/*) [[ -f "$FX/base-manifest.yaml" ]] && cat "$FX/base-manifest.yaml" || exit 22 ;;
  */issues/7/comments*) cat "$FX/comments.json" 2>/dev/null || echo '[]' ;;
  */collaborators/*/permission) u="${url%/permission}"; u="${u##*/}"; jq -nc --arg r "$(jq -r --arg u "$u" '.[$u] // "read"' "$FX/roles.json" 2>/dev/null || echo read)" '{role_name: $r}' ;;
  */token\?*) echo '{"token":"t"}' ;;
  */manifests/*) tag="${url##*/}"; echo "docker-content-digest: sha256:${tag}0000000000000000000000000000000000000000000000000000"; echo ;;
  */log/publicKey) echo key ;;
  http://prom.example*)
    [[ -f "$FX/prom-down" ]] && exit 7
    if [[ "$data" == *traces_service_graph* ]]; then cat "$FX/prom-callers.json" 2>/dev/null || echo '{"status":"success","data":{"result":[]}}'
    else cat "$FX/prom-ops.json" 2>/dev/null || echo '{"status":"success","data":{"result":[]}}'; fi ;;
  *) echo "unexpected curl $url" >&2; exit 22 ;;
esac
EOF
# cosign: download prints $FX/att-<digest-prefix>.jsonl; verify fails when $FX/verify-fails exists.
cat > "$d/bin/cosign" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
  "trusted-root create") while [[ $# -gt 0 ]]; do [[ "$1" == --out ]] && echo '{}' > "$2"; shift; done ;;
  "download attestation") ref="${!#}"; dg="${ref##*@sha256:}"; cat "$FX/att-${dg:0:4}.jsonl" 2>/dev/null; exit 0 ;;
  "verify-blob-attestation "*) for a in "$@"; do [[ "$a" == *"(app|infra)-flight-api-cicd/serviceaccounts/pipeline-runner"* ]] && idok=1; done
    [[ -z "${idok:-}" ]] && { echo "no app-scoped identity" >&2; exit 1; }
    [[ -f "$FX/verify-fails" ]] && { echo "error: invalid signature" >&2; exit 1; }; exit 0 ;;
  *) exit 0 ;;
esac
EOF
cat > "$d/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"create configmap"*) for a in "$@"; do [[ "$a" == --from-file=body=* ]] && f="${a#--from-file=body=}"; [[ "$a" == --from-literal=title=* ]] && ti="${a#--from-literal=title=}"; done
    jq -n --rawfile b "$f" --arg t "$ti" '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"x"},data:{body:$b,title:$t}}' ;;
  apply*) cat > "$FX/applied.json" ;;
  *pipelinerun*) echo uid-1 ;;
  *) echo cert ;;
esac
EOF
chmod +x "$d/bin/"*

SPEC_BASE="$(yq -o=json '.' <<'EOF'
openapi: 3.1.0
info: {title: flight-api, version: "1.0.0"}
paths:
  /api/flights/{flightNumber}:
    get: {responses: {"200": {description: ok}}}
  /api/flights/{flightNumber}/delay:
    put: {responses: {"200": {description: ok}}}
EOF
)"
SPEC_BREAK="$(jq -c 'del(.paths["/api/flights/{flightNumber}/delay"]) | .info.version = "1.1.0"' <<<"${SPEC_BASE}")"
bundle() { # <spec-json> [apiType]
  jq -nc --argjson s "$1" --arg t "${2:-openapi}" '{predicateType: "https://hangar.io/attestations/api-spec/v1",
    predicate: {apiType: $t, path: "openapi.yaml", sha256: ("x" * 64), specVersion: "3.1.0", title: $s.info.title, version: $s.info.version, spec: $s}}' \
    | base64 | tr -d '\n' | jq -Rc '{dsseEnvelope: {payload: ., payloadType: "application/vnd.in-toto+json", signatures: [{sig: "s"}]}, verificationMaterial: {}}'
}

new_case() { FX="$d/fx/$1"; mkdir -p "$FX"; export FX
  printf 'release:\n  image: {repository: ghcr.io/o/flight-api, tag: base}\n' > "$FX/base-manifest.yaml"
  bundle "${SPEC_BASE}" > "$FX/att-base.jsonl"; bundle "${SPEC_BASE}" > "$FX/att-cand.jsonl"; }
run() { # <expect-verdict> <expect-exit> [ENV=V ...]
  local want="$1" want_rc="$2"; shift 2
  sed -e "s|\$(results.outcome.path)|$FX/outcome|g" -e "s|\$(results.findings-summary.path)|$FX/summary|g" "$d/script.raw" > "$FX/script.sh"
  local envs=(); while IFS= read -r kv; do envs+=("$kv"); done < <(jq -r '.[] | select(.value != null and (.value | test("^\\$\\(") | not)) | "\(.name)=\(.value)"' "$d/env.json")
  env "${envs[@]}" PATH="$d/bin:$PATH" PLATFORM_LIB="$d/lib" FX="$FX" DOCKER_CONFIG="$FX" \
    REPO_URL=https://github.com/o/gitops-flight-api REVISION=headsha PR_NUMBER=7 IMAGE_REF=ghcr.io/o/flight-api:cand \
    MANIFEST=prod-cluster/prod/release.yaml ENFORCE=false PIPELINE_RUN=api-compat-x NAMESPACE=app-flight-api-cicd "$@" \
    bash "$FX/script.sh" > "$FX/log" 2>&1
  local rc=$? got; got="$(cat "$FX/outcome" 2>/dev/null)"
  [[ "$got" == "$want" && "$rc" == "$want_rc" ]] || { echo "FAIL: $(basename "$FX"): verdict '$got' exit $rc, want '$want' exit $want_rc"; tail -15 "$FX/log"; fail=1; }
  # The contract line: present once, valid JSON, no "--" inside the HTML comment.
  line="$(jq -r '.data.body' "$FX/applied.json" 2>/dev/null | grep -o '<!-- glidepath:api-compat .* -->')"
  [[ "$(grep -c . <<<"$line")" == 1 ]] || { echo "FAIL: $(basename "$FX"): contract line missing"; fail=1; return; }
  j="${line#<!-- glidepath:api-compat }"; j="${j% -->}"
  [[ "$j" != *"--"* ]] || { echo "FAIL: $(basename "$FX"): '--' inside the HTML comment"; fail=1; }
  printf '%s' "$j" > "$FX/report.json"
  jq -e --arg v "$want" '.version == 1 and .verdict == $v and (has("env") and has("candidate") and has("baseline") and has("breaking")
     and has("nonBreakingCount") and has("usage") and has("override")) and (.usage | has("source") and has("window") and has("callers"))' "$FX/report.json" >/dev/null \
    || { echo "FAIL: $(basename "$FX"): contract fields: $(head -c 300 "$FX/report.json")"; fail=1; }
}
jqt() { jq -e "$1" "$FX/report.json" >/dev/null || { echo "FAIL: $(basename "$FX"): $2"; fail=1; }; }

new_case pass;            run pass 0
jqt '.breaking == [] and .candidate.digest == "sha256:cand0000000000000000000000000000000000000000000000000000" and .baseline.title == "flight-api"' "pass: refs"

new_case warn;            bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; run warn 0 USAGE_CONFIG='{}'
jqt '.breaking[0].id == "api-path-removed-without-deprecation" and .breaking[0].level == "error" and .breaking[0].operation == {method: "PUT", path: "/api/flights/{flightNumber}/delay"} and .breaking[0].inUse == null and .usage.source == "unknown"' "warn: breaking entry / unknown usage"
jqt '.notes | any(test("no apiCompat.usage endpoint"))' "warn: unknown-usage note"

OPS='{"status":"success","data":{"result":[{"metric":{"span_name":"PUT /api/flights/{flightNumber}/delay"},"value":[0,"0.5"]},{"metric":{"span_name":"GET /api/flights/{flightNumber}"},"value":[0,"3"]}]}}'
CALLERS='{"status":"success","data":{"result":[{"metric":{"client":"boarding-api-prod"},"value":[0,"12"]},{"metric":{"client":"user"},"value":[0,"0"]}]}}'
new_case block;           bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; echo "$OPS" > "$FX/prom-ops.json"; echo "$CALLERS" > "$FX/prom-callers.json"
run block 1 ENFORCE=true BLOCK_WHEN_IN_USE=true
jqt '.breaking[0].inUse == true and .breaking[0].rate == 0.5 and .usage.source == "prometheus" and .usage.callers == ["boarding-api-prod"]' "block: usage"

new_case block-advisory;  bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; echo "$OPS" > "$FX/prom-ops.json"
run warn 0 ENFORCE=false BLOCK_WHEN_IN_USE=true
new_case block-off;       bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; echo "$OPS" > "$FX/prom-ops.json"
run warn 0 ENFORCE=true BLOCK_WHEN_IN_USE=false

new_case not-in-use;      bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"
echo '{"status":"success","data":{"result":[{"metric":{"span_name":"GET /api/flights/{flightNumber}"},"value":[0,"3"]}]}}' > "$FX/prom-ops.json"
run warn 0 ENFORCE=true BLOCK_WHEN_IN_USE=true
jqt '.breaking[0].inUse == false and .breaking[0].rate == 0' "not-in-use: inUse false"

new_case wildcard;        bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"
echo '{"status":"success","data":{"result":[{"metric":{"span_name":"PUT /**"},"value":[0,"1"]}]}}' > "$FX/prom-ops.json"
run warn 0 ENFORCE=true BLOCK_WHEN_IN_USE=true
jqt '.breaking[0].inUse == null' "wildcard route: usage unknown, never blocks"

new_case prom-down;       bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; touch "$FX/prom-down"
run warn 0 ENFORCE=true BLOCK_WHEN_IN_USE=true
jqt '.usage.source == "unknown" and (.notes | any(test("usage query")))' "prom-down: unknown"

# Override: an owner's comment after the promoted commit wins; a non-owner's is ignored; an owner's from before it is stale.
new_case overridden;      bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; echo "$OPS" > "$FX/prom-ops.json"
echo '{"alice":"maintain","mallory":"write","olga":"admin"}' > "$FX/roles.json"
jq -n '[{user:{login:"olga"},created_at:"2026-10-19T00:00:00Z",body:"/api-break-ok stale"},
        {user:{login:"alice"},created_at:"2026-10-21T00:00:00Z",body:"/api-break-ok gate is gone, boarding-api updated in #12\nthanks"},
        {user:{login:"mallory"},created_at:"2026-10-22T00:00:00Z",body:"/api-break-ok trust me"},
        {user:{login:"bob"},created_at:"2026-10-22T01:00:00Z",body:"looks fine"}]' > "$FX/comments.json"
run overridden 0 ENFORCE=true BLOCK_WHEN_IN_USE=true
jqt '.override == {by: "alice", at: "2026-10-21T00:00:00Z", reason: "gate is gone, boarding-api updated in #12"}' "override: owner, reason first line"
jqt '.notes | any(test("ignored /api-break-ok from mallory: role on o/flight-api is write"))' "override: non-owner ignored and said"

new_case only-non-owner;  bundle "${SPEC_BREAK}" > "$FX/att-cand.jsonl"; echo "$OPS" > "$FX/prom-ops.json"
echo '{"mallory":"write"}' > "$FX/roles.json"
jq -n '[{user:{login:"mallory"},created_at:"2026-10-22T00:00:00Z",body:"/api-break-ok trust me"}]' > "$FX/comments.json"
run block 1 ENFORCE=true BLOCK_WHEN_IN_USE=true

new_case no-spec;         rm "$FX/att-cand.jsonl"; run no-spec 0
jqt '.candidate == null' "no-spec: candidate null"
new_case no-baseline;     rm "$FX/base-manifest.yaml"; run no-baseline 0
jqt '.baseline == null' "no-baseline: baseline null"
new_case baseline-nospec; rm "$FX/att-base.jsonl"; run no-baseline 0
new_case unsupported;     bundle "${SPEC_BASE}" asyncapi > "$FX/att-cand.jsonl"; run unsupported 0
new_case unverified;      touch "$FX/verify-fails"; run no-spec 0
jqt '.notes | any(test("failed verification and was ignored"))' "unverified: said so"

# Truncation: 600 removed paths stay under the size budget, with the total kept.
new_case truncate
BIG="$(jq -c '.paths = ([range(600)] | map({key: "/api/r\(.)/with/a/rather/long/path/segment", value: {get: {responses: {"200": {description: "ok"}}}}}) | from_entries)' <<<"${SPEC_BASE}")"
bundle "${BIG}" > "$FX/att-base.jsonl"; bundle "${SPEC_BASE}" > "$FX/att-cand.jsonl"
run warn 0
jqt '.breakingTotal == 600 and (.breaking | length) < 600 and (.breaking | length) > 0' "truncate: total kept, list cut"
[[ "$(wc -c < "$FX/report.json")" -le 40000 ]] || { echo "FAIL: truncate: JSON over budget"; fail=1; }
[[ "$(jq -r '.data.body' "$FX/applied.json" | wc -c)" -lt 65536 ]] || { echo "FAIL: truncate: comment over GitHub's limit"; fail=1; }
jq -r '.data.body' "$FX/applied.json" | grep -q '| … | 550 more' || { echo "FAIL: truncate: table not cut at 50"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
