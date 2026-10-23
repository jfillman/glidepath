#!/usr/bin/env bash
# attest-api-spec.yaml's script, run against stub cosign/kubectl/curl: every outcome (none, invalid, attested,
# already attested, failed), the predicate's shape, and that the Task never fails the build. Needs helm, yq, jq.
# Usage: charts/glidepath-catalog/tests/attest_api_spec_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/attest-api-spec.yaml > "$d/task.yaml" || { echo "FAIL: render"; exit 1; }
yq '.spec.steps[0].script' "$d/task.yaml" > "$d/script.raw"
mkdir -p "$d/bin" "$d/lib"
printf 'otel_child_span() { shift 4; shift; "$@"; }\n' > "$d/lib/otel.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$d/bin/kubectl"
printf '#!/usr/bin/env bash\nexit 0\n' > "$d/bin/curl"
# cosign: `download` prints $COSIGN_EXISTING (one envelope per line) when set; `attest` copies the predicate and
# exits $COSIGN_ATTEST_EXIT.
cat > "$d/bin/cosign" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  download) [[ -n "${COSIGN_EXISTING:-}" ]] && { printf '%s\n' "${COSIGN_EXISTING}"; exit 0; }; exit 1 ;;
  attest) while [[ $# -gt 0 ]]; do [[ "$1" == --predicate ]] && cp "$2" "${PRED_OUT}"; shift; done; exit "${COSIGN_ATTEST_EXIT:-0}" ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$d/bin/"*

run() { # name spec-file-content(or -) api-type expect-outcome [env...]
  local name="$1" content="$2" type="$3" want="$4"; shift 4
  local ws="$d/ws-$RANDOM"; mkdir -p "$ws"
  [[ "$content" != "-" ]] && printf '%s' "$content" > "$ws/openapi.yaml"
  sed -e "s|\$(results.outcome.path)|$d/outcome|g" -e "s|\$(workspaces.source.path)|$ws|g" "$d/script.raw" > "$d/script.sh"
  rm -f "$d/outcome" "$d/pred.json"
  env PATH="$d/bin:$PATH" PLATFORM_LIB="$d/lib" IMAGE_REF=reg/app@sha256:abc SPEC_PATH=openapi.yaml API_TYPE="$type" \
    FLOW_TRACEPARENT="" STAGE_SPAN_ID="" PRED_OUT="$d/pred.json" "$@" bash "$d/script.sh" > "$d/log" 2>&1
  local rc=$?
  [[ $rc -eq 0 ]] || { echo "FAIL: $name: exit $rc (the Task must never fail the build)"; cat "$d/log"; fail=1; }
  local got; got="$(cat "$d/outcome" 2>/dev/null)"
  [[ "$got" == "$want" ]] || { echo "FAIL: $name: outcome '$got', want '$want'"; cat "$d/log"; fail=1; }
}

SPEC=$'openapi: 3.1.0\ninfo:\n  title: Flight API\n  version: 2.0.1\npaths:\n  /flights/{id}:\n    get: {responses: {"200": {description: ok}}}\n'
run "no file"                 -                                   openapi none
run "not YAML"                $'openapi: [unclosed\n'             openapi invalid
run "no openapi field"        $'swagger: "2.0"\ninfo: {}\n'       openapi invalid
run "asyncapi type mismatch"  "$SPEC"                             asyncapi invalid
run "attested"                "$SPEC"                             openapi attested
jq -e '.apiType == "openapi" and .path == "openapi.yaml" and .specVersion == "3.1.0" and .title == "Flight API"
       and .version == "2.0.1" and (.sha256 | length) == 64 and .spec.paths["/flights/{id}"].get != null' "$d/pred.json" >/dev/null \
  || { echo "FAIL: predicate shape: $(cat "$d/pred.json" 2>/dev/null | head -c 400)"; fail=1; }
run "attest fails"            "$SPEC"                             openapi failed   COSIGN_ATTEST_EXIT=1
# Already attached to this digest with the same file: no second attestation.
SHA="$(printf '%s' "$SPEC" | sha256sum | cut -d' ' -f1)"
ENV_SAME="$(jq -cn --arg p "$(jq -cn --arg h "$SHA" '{predicate: {sha256: $h}}' | base64 | tr -d '\n')" '{payload: $p}')"
ENV_OTHER="$(jq -cn --arg p "$(jq -cn '{predicate: {sha256: "other"}}' | base64 | tr -d '\n')" '{payload: $p}')"
BUNDLE_SAME="$(jq -cn --arg p "$(jq -cn --arg h "$SHA" '{predicate: {sha256: $h}}' | base64 | tr -d '\n')" '{dsseEnvelope: {payload: $p}}')"
run "already attested"        "$SPEC"                             openapi attested COSIGN_EXISTING="$(printf '%s\n%s' "$ENV_OTHER" "$ENV_SAME")"
[[ ! -f "$d/pred.json" ]] || { echo "FAIL: already attested: attested a duplicate"; fail=1; }
run "already attested (bundle)" "$SPEC"                            openapi attested COSIGN_EXISTING="$BUNDLE_SAME"
[[ ! -f "$d/pred.json" ]] || { echo "FAIL: already attested (cosign 3.x bundle): attested a duplicate"; fail=1; }
run "a different spec exists" "$SPEC"                             openapi attested COSIGN_EXISTING="$ENV_OTHER"
[[ -f "$d/pred.json" ]] || { echo "FAIL: a different spec on the digest must not suppress this one"; fail=1; }
[ $fail -eq 0 ] && echo ok || exit 1
