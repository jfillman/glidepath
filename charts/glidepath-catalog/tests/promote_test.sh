#!/usr/bin/env bash
# The promote Task and promote-image StepAction (ADR-0029 phase 2), as rendered. Runs each step's bash against a fake
# curl/oras/cosign that serve fixtures, and checks the refusals the rules require (PROM-01 Ground pin, PROM-03 red
# gate, PROM-06 foreign source, PROM-08 merged PR, no digest), the happy path's hand-off file, the workflow's failing step as the reason, an
# attestation from another request, and the check run's conclusion. With releaseGuardrailOverridesByApp (2026-10-28),
# PROM-03 skips a gate advisory for the promoted app only (backstage: image-scan) and still refuses it for any other.
# Needs helm, yq (mikefarah v4), jq and python3 (pyyaml).
# Usage: charts/glidepath-catalog/tests/promote_test.sh
set -u
cd "$(dirname "$0")/../../.."
export GATE_POLL_SECONDS=0
fail=0
work="$(mktemp -d)"
EX=charts/glidepath-catalog/tests/values.example.yaml
D=sha256:53de992b7d3b3f5965a10695b5b04a02cc0558807c0ad3a8f1c10971c3bd762d
SHA=1111111111111111111111111111111111111111

helm template t charts/glidepath-catalog -f "$EX" --show-only templates/tasks/promote.yaml > "$work/task.yaml" || exit 1
helm template t charts/glidepath-catalog -f "$EX" --show-only templates/stepactions/promote-image.yaml \
  --set promotion.promoter.repo=riley-roooo/flight-promoter --set registries.flight.namespace=riley-roooo > "$work/sa.yaml" || exit 1
python3 - "$work" <<'PY'
import sys, yaml
w = sys.argv[1]
for st in yaml.safe_load(open(f"{w}/task.yaml"))["spec"]["steps"]:
    if "script" in st:
        s = st["script"].replace("$(results.flight-ref.path)", f"{w}/res-flight-ref").replace("$(results.outcome.path)", f"{w}/res-outcome")
        open(f"{w}/{st['name']}.sh", "w").write(s)
sa = yaml.safe_load(open(f"{w}/sa.yaml"))["spec"]
open(f"{w}/copy.sh", "w").write(sa["script"])
open(f"{w}/copy.env", "w").write("".join(f"export {e['name']}='{e['value']}'\n" for e in sa["env"] if "$(" not in e["value"]))
PY
# The release step again, rendered with a per-app advisory gate (backstage: image-scan).
helm template t charts/glidepath-catalog -f "$EX" --show-only templates/tasks/promote.yaml \
  --set-json 'releaseGuardrailOverridesByApp={"backstage":{"image-scan":{"required":false}}}' \
  | yq '.spec.steps[] | select(.name == "release") | .script' > "$work/release-perapp.sh" || exit 1
[[ -s "$work/release-perapp.sh" ]] || { echo "FAIL: no release step in the per-app render"; exit 1; }

# --- fakes ------------------------------------------------------------------------------------------------------
mkdir -p "$work/bin"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
# Serves $FX/<route> for the URL; logs "METHOD URL [body]" to $FX/calls. -w '\n%{http_code}' gets "\n200".
method=GET url="" data="" wcode=0
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -d) data="$2"; shift ;;
    -w) wcode=1; shift ;;
    -H|--max-time|--retry) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
[[ -n "$data" && "$method" == GET ]] && method=POST
echo "$method $url $( [[ -n "$data" ]] && { jq -c . <<<"$data" 2>/dev/null || echo "$data"; })" >> "$FX/calls"
route() {
  case "$method $url" in
    "POST "*promoter-token) echo broker ;;
    "GET "*/issues/*/comments*) echo comments.json ;;
    "POST "*/issues/*/comments|"PATCH "*/issues/comments/*) echo empty.json ;;
    "GET "*/pulls/*/files*) echo files.json ;;
    "GET "*/pulls/*) echo pr.json ;;
    "GET "*/contents/*) echo release.yaml ;;
    "GET "*/check-runs*) if [[ -f "$FX/checks.first.json" ]]; then echo checks.first.json; else echo checks.json; fi ;;
    "POST "*/check-runs) echo check-created.json ;;
    "PATCH "*/check-runs/*) echo empty.json ;;
    "POST "*/dispatches) echo empty.json ;;
    "GET "*/runs\?*) echo runs.json ;;
    "GET "*/jobs*) echo jobs.json ;;
    *) echo none ;;
  esac
}
r="$(route)"
if [[ "$r" == broker ]]; then printf '{"token":"t"}'; else cat "$FX/$r" 2>/dev/null || { echo "{}"; exit 22; }; fi
[[ "$r" == checks.first.json ]] && rm -f "$FX/checks.first.json"   # served once: the next read sees checks.json
(( wcode )) && printf '\n200'
exit 0
EOF
cat > "$work/bin/oras" <<'EOF'
#!/usr/bin/env bash
# oras resolve <ref>: $FX/oras-<sanitized ref> holds the digest, else "not found".
ref="${@: -1}"; f="$FX/oras-$(tr '/:@' '___' <<<"$ref")"
[[ -f "$f" ]] && { cat "$f"; exit 0; }; echo "not found: $ref" >&2; exit 1
EOF
cat > "$work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
[[ -f "$FX/cosign.out" ]] && { cat "$FX/cosign.out"; exit 0; }; echo "no matching attestations" >&2; exit 1
EOF
chmod +x "$work/bin/"*
echo t > "$work/sa-token"

scenario() { # name -> fresh fixture dir in $FX, defaults for a promotable release
  FX="$work/fx-$1"; mkdir -p "$FX/w"; export FX
  echo '{"id":42}' > "$FX/check-created.json"; echo '{}' > "$FX/empty.json"; echo '[]' > "$FX/comments.json"
  jq -n --arg sha "$SHA" '{state: "open", merged: false, head: {sha: $sha}}' > "$FX/pr.json"
  echo '[{"filename":"hangar-prod/prod/release.yaml"}]' > "$FX/files.json"
  printf 'release:\n  image:\n    repository: ghcr.io/riley-roooo/boarding-api\n    tag: 2.0.5-832e475\n    digest: %s\n    source: ghcr.io/jfillman/boarding-api\nreleaseTracking:\n  releaseId: r1:hangar-prod/prod\n' "$D" > "$FX/release.yaml"
  jq -n '{check_runs: [ "sast","image-scan","provenance","sbom","values","itsm","policy-validation"
          | {name: ("Pipelines as Code CI / " + . + "-"), status: "completed", conclusion: "success", started_at: "2026-10-27T10:00:00Z"} ]}' > "$FX/checks.json"
}
run_step() { # step-file -> runs it with the fakes; exit code in $rc
  ( export PATH="$work/bin:$PATH" PROMOTE_WORK="$FX/w" PROMOTER_TOKEN_FILE="$work/sa-token" \
      PROMOTER_TOKEN_URL=http://broker/promoter-token REPO_URL="${REPO_URL:-https://github.com/jfillman/gitops-boarding-api}" \
      PR_NUMBER=7 RUN_NAME=promote-run-abc GROUND_HOST=ghcr.io GROUND_NAMESPACE= FLIGHT_HOST=ghcr.io FLIGHT_NAMESPACE="${FLIGHT_NAMESPACE-riley-roooo}" REQUEST_ID=promote-run-abc
    # shellcheck disable=SC1091
    [[ "$1" == copy.sh ]] && source "$work/copy.env"
    bash "$work/$1" ) > "$FX/out" 2>&1
  rc=$?
}
expect_refusal() { # name pattern
  run_step release.sh
  if (( rc == 0 )) || ! grep -q -- "$2" "$FX/w/reason" 2>/dev/null; then
    echo "FAIL: $1: want a refusal matching '$2', got rc=$rc reason='$(cat "$FX/w/reason" 2>/dev/null)'"; tail -3 "$FX/out"; fail=1
  fi
  [[ ! -f "$FX/w/release.env" ]] || { echo "FAIL: $1 still wrote release.env"; fail=1; }
}

# --- release step -------------------------------------------------------------------------------------------------
scenario happy; run_step release.sh
if (( rc != 0 )); then echo "FAIL: happy release refused: $(cat "$FX/w/reason" 2>/dev/null)"; cat "$FX/out"; fail=1
else
  ( source "$FX/w/release.env"
    [[ "$SOURCE" == "ghcr.io/jfillman/boarding-api@$D" && "$PACKAGE" == boarding-api && "$TAG" == 2.0.5-832e475 \
       && "$RELEASE_ID" == r1:hangar-prod/prod && "$PULL_REQUEST" == "jfillman/gitops-boarding-api#7@$SHA" ]] \
      || { echo "FAIL: happy release.env: $(cat "$FX/w/release.env")"; exit 1; }
    [[ "$GATES" == *itsm=success* && "$GATES" != *image-promotion* ]] || { echo "FAIL: gates snapshot: $GATES"; exit 1; } ) || fail=1
  [[ "$(cat "$FX/w/check-id")" == 42 ]] || { echo "FAIL: the check run was not opened"; fail=1; }
  grep 'POST .*/check-runs' "$FX/calls" | grep -qF '"name":"Pipelines as Code CI / image-promotion-"' \
    || { echo "FAIL: check run not named like a PaC gate: $(grep check-runs "$FX/calls")"; fail=1; }
fi

scenario merged; jq '.merged = true | .state = "closed"' "$FX/pr.json" > "$FX/x" && mv "$FX/x" "$FX/pr.json"
expect_refusal merged "PROM-08"
scenario closed; jq '.state = "closed"' "$FX/pr.json" > "$FX/x" && mv "$FX/x" "$FX/pr.json"
expect_refusal closed "is closed"
scenario nodigest; yq -i 'del(.release.image.digest)' "$FX/release.yaml"
expect_refusal no-digest "pins no digest"
scenario prsource; yq -i '.release.image.source = "ghcr.io/jfillman/boarding-api-pr"' "$FX/release.yaml"
expect_refusal pr-source "PROM-06"
scenario groundpin; yq -i '.release.image.repository = "ghcr.io/jfillman/boarding-api"' "$FX/release.yaml"
expect_refusal ground-pin "pins ghcr.io/jfillman/boarding-api, not the Flight repository ghcr.io/riley-roooo/boarding-api.*PROM-01"
scenario noflightns; FLIGHT_NAMESPACE="" expect_refusal no-flight-namespace "registries.flight.namespace is not set"
scenario otherapp; yq -i '.release.image.source = "ghcr.io/jfillman/gate-api"' "$FX/release.yaml"
expect_refusal other-app "PROM-06"
scenario redgate
jq '.check_runs |= map(if .name == "Pipelines as Code CI / itsm-" then .conclusion = "failure" else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
expect_refusal red-gate "itsm (failure).*PROM-03"
scenario missinggate
jq '.check_runs |= map(select(.name != "Pipelines as Code CI / sbom-"))' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
expect_refusal missing-gate "sbom (missing)"
scenario retest   # an older red sast and a newer green one: the newest counts
jq '.check_runs += [{name: "Pipelines as Code CI / sast-", status: "completed", conclusion: "failure", started_at: "2026-10-27T09:00:00Z"}]' \
  "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run_step release.sh; (( rc == 0 )) || { echo "FAIL: a retested-green gate still refused: $(cat "$FX/w/reason")"; fail=1; }
scenario retest-red   # a newer red sast after an older green one: still red
jq '.check_runs += [{name: "Pipelines as Code CI / sast-", status: "completed", conclusion: "failure", started_at: "2026-10-27T11:00:00Z"}]' \
  "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
expect_refusal retest-red "sast (failure)"
scenario running   # a gate still running is not green
jq '.check_runs |= map(if .name == "Pipelines as Code CI / qa-" or .name == "Pipelines as Code CI / values-" then .status = "in_progress" | .conclusion = null else . end)' \
  "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
GATE_WAIT_SECONDS=0 expect_refusal running "still running on 1111111 after 0s: values (in_progress)"
scenario running-then-green   # the fan-in's own gate is still in progress at first, then green: waited for, not refused
jq '.check_runs |= map(if .name == "Pipelines as Code CI / sbom-" then .status = "in_progress" | .conclusion = null else . end)' \
  "$FX/checks.json" > "$FX/checks.first.json"
run_step release.sh
if (( rc != 0 )) || ! grep -q 'waiting for sbom (in_progress)' "$FX/out" || [[ ! -f "$FX/w/release.env" ]]; then
  echo "FAIL: running-then-green: rc=$rc reason='$(cat "$FX/w/reason" 2>/dev/null)'"; tail -3 "$FX/out"; fail=1
fi
scenario notgitops; REPO_URL=https://github.com/jfillman/boarding-api expect_refusal not-gitops "not a gitops-<app>"
scenario notrelease; echo '[{"filename":"README.md"}]' > "$FX/files.json"
expect_refusal not-release "not a release PR"

# --- releaseGuardrailOverridesByApp: image-scan red on every scenario here ------------------------------------------
app_scenario() { # name app: a promotable release of <app> with image-scan red
  scenario "$1"
  yq -i ".release.image.repository = \"ghcr.io/riley-roooo/$2\" | .release.image.source = \"ghcr.io/jfillman/$2\"" "$FX/release.yaml"
  jq '.check_runs |= map(if .name == "Pipelines as Code CI / image-scan-" then .conclusion = "failure" else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
}
app_scenario perapp-backstage backstage
REPO_URL=https://github.com/jfillman/gitops-backstage run_step release-perapp.sh
if (( rc != 0 )) || [[ ! -f "$FX/w/release.env" ]]; then
  echo "FAIL: perapp-backstage: image-scan is advisory for backstage but PROM-03 refused: $(cat "$FX/w/reason" 2>/dev/null)"; tail -3 "$FX/out"; fail=1
else
  ( source "$FX/w/release.env"; [[ "$PACKAGE" == backstage && "$GATES" == *sbom=success* && "$GATES" != *image-scan* ]] \
      || { echo "FAIL: perapp-backstage release.env: $(cat "$FX/w/release.env")"; exit 1; } ) || fail=1
  grep -q 'advisory for backstage (catalog releaseGuardrailOverridesByApp), not required: image-scan' "$FX/out" \
    || { echo "FAIL: perapp-backstage does not say image-scan is advisory"; fail=1; }
fi
app_scenario perapp-other boarding-api
run_step release-perapp.sh
if (( rc == 0 )) || ! grep -q 'image-scan (failure).*PROM-03' "$FX/w/reason" 2>/dev/null; then
  echo "FAIL: perapp-other: image-scan red must still refuse boarding-api: rc=$rc reason='$(cat "$FX/w/reason" 2>/dev/null)'"; fail=1
fi
app_scenario perapp-default backstage   # the chart default render: backstage is held like any app
REPO_URL=https://github.com/jfillman/gitops-backstage expect_refusal perapp-default "image-scan (failure).*PROM-03"
app_scenario perapp-sbom-red backstage  # only the listed gate is advisory for backstage
jq '.check_runs |= map(if .name == "Pipelines as Code CI / sbom-" then .conclusion = "failure" else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
REPO_URL=https://github.com/jfillman/gitops-backstage run_step release-perapp.sh
if (( rc == 0 )) || ! grep -q 'sbom (failure)' "$FX/w/reason" 2>/dev/null || grep -q 'image-scan' "$FX/w/reason"; then
  echo "FAIL: perapp-sbom-red: want a refusal for sbom only: rc=$rc reason='$(cat "$FX/w/reason" 2>/dev/null)'"; fail=1
fi

# --- copy (promote-image) -----------------------------------------------------------------------------------------
copy_fixture() { # scenario name + conclusion [failed step]
  scenario "$1"
  printf 'SOURCE=%q\nPACKAGE=boarding-api\nTAG=2.0.5-832e475\nDIGEST=%q\nRELEASE_ID=r1\nPULL_REQUEST=x\nGATES=sast=success\n' \
    "ghcr.io/jfillman/boarding-api@$D" "$D" > "$FX/w/release.env"
  jq -n --arg c "$2" '{workflow_runs: [{display_title: "promote boarding-api someone-else", status: "completed", conclusion: "success", html_url: "u0", jobs_url: "https://api.github.com/j0"},
                                       {display_title: "promote boarding-api promote-run-abc", status: "completed", conclusion: $c, html_url: "https://run/1", jobs_url: "https://api.github.com/jobs/1"}]}' > "$FX/runs.json"
  jq -n --arg s "${3:-}" '{jobs: [{steps: [{name: "Install oras", conclusion: "success"}, {name: $s, conclusion: "failure"}]}]}' > "$FX/jobs.json"
  mkdir -p "$FX/dc"; echo '{}' > "$FX/dc/config.json"
  b="sha256-${D#sha256:}"
  echo "$D" > "$FX/oras-ghcr.io_riley-roooo_boarding-api_2.0.5-832e475"
  echo x > "$FX/oras-ghcr.io_riley-roooo_boarding-api_${b}.sig"; echo x > "$FX/oras-ghcr.io_riley-roooo_boarding-api_${b}.att"
  att() { jq -nc --arg r "$1" '{payload: ({predicate: {requestId: $r}} | tojson | @base64)}'; }
  { att older-request; att "${4:-promote-run-abc}"; } > "$FX/cosign.out"
}
run_copy() {
  sed -e "s#/var/run/secrets/flight-read#$FX/dc#g" "$work/copy.sh" > "$work/copy-run.sh"
  ( export PATH="$work/bin:$PATH" PROMOTE_WORK="$FX/w" PROMOTER_TOKEN_FILE="$work/sa-token" REQUEST_ID=promote-run-abc
    source "$work/copy.env"; bash "$work/copy-run.sh" ) > "$FX/out" 2>&1
  rc=$?
}
copy_fixture copy-ok success; run_copy
if (( rc != 0 )) || [[ "$(cat "$FX/w/flight-ref" 2>/dev/null)" != "ghcr.io/riley-roooo/boarding-api@$D" ]]; then
  echo "FAIL: copy happy path: rc=$rc reason=$(cat "$FX/w/reason" 2>/dev/null)"; tail -5 "$FX/out"; fail=1
fi
grep dispatches "$FX/calls" | grep -qF '"request-id":"promote-run-abc"' || { echo "FAIL: dispatch inputs: $(grep dispatches "$FX/calls")"; fail=1; }
copy_fixture copy-red failure "Check the source is signed and attested"; run_copy
(( rc != 0 )) && grep -q 'failure at "Check the source is signed and attested": https://run/1' "$FX/w/reason" \
  || { echo "FAIL: red workflow reason: rc=$rc $(cat "$FX/w/reason" 2>/dev/null)"; fail=1; }
copy_fixture copy-foreign-att success "" another-request; run_copy
(( rc != 0 )) && grep -q "none is this request's" "$FX/w/reason" || { echo "FAIL: another request's attestation accepted: rc=$rc $(cat "$FX/w/reason" 2>/dev/null)"; fail=1; }
copy_fixture copy-wrong-digest success; echo "sha256:$(printf '0%.0s' {1..64})" > "$FX/oras-ghcr.io_riley-roooo_boarding-api_2.0.5-832e475"; run_copy
(( rc != 0 )) && grep -q "not $D" "$FX/w/reason" || { echo "FAIL: a wrong Flight digest accepted: rc=$rc $(cat "$FX/w/reason" 2>/dev/null)"; fail=1; }
copy_fixture copy-skip success; echo "refused earlier" > "$FX/w/reason"; run_copy
(( rc == 0 )) && ! grep -q dispatches "$FX/calls" 2>/dev/null || { echo "FAIL: copy ran after a refusal"; fail=1; }

# --- report -------------------------------------------------------------------------------------------------------
scenario report-ok; printf 'OWNER=jfillman\nREPO=gitops-boarding-api\nHEAD_SHA=%s\n' "$SHA" > "$FX/w/repo.env"; echo 42 > "$FX/w/check-id"
echo "ghcr.io/riley-roooo/boarding-api@$D" > "$FX/w/flight-ref"; printf 'GATES=sast=success\n' > "$FX/w/release.env"
run_step report.sh
(( rc == 0 )) && grep 'PATCH .*/check-runs/42' "$FX/calls" | grep -q '"conclusion":"success"' || { echo "FAIL: report success: rc=$rc $(grep PATCH "$FX/calls")"; fail=1; }
[[ "$(cat "$work/res-outcome")" == success ]] || { echo "FAIL: outcome result"; fail=1; }
grep 'POST .*/issues/7/comments' "$FX/calls" | grep -q '<!-- glidepath:gate:image-promotion -->\\n### ✅ `image-promotion` passed' \
  || { echo "FAIL: report success: no new PR comment with the gate marker: $(grep comments "$FX/calls")"; fail=1; }
scenario report-red; printf 'OWNER=jfillman\nREPO=gitops-boarding-api\nHEAD_SHA=%s\n' "$SHA" > "$FX/w/repo.env"
echo "required gates are not green" > "$FX/w/reason"
echo '[{"id":5,"body":"<!-- glidepath:gate:sbom -->\nx"},{"id":9,"body":"<!-- glidepath:gate:image-promotion -->\nold"}]' > "$FX/comments.json"
run_step report.sh   # no check-id yet (refused before opening): report opens one, then concludes it
(( rc != 0 )) && grep 'PATCH .*/check-runs/42' "$FX/calls" | grep -q '"conclusion":"failure"' \
  && grep -q 'POST .*/check-runs' "$FX/calls" || { echo "FAIL: report failure: rc=$rc $(cat "$FX/calls")"; fail=1; }
grep 'PATCH .*/issues/comments/9' "$FX/calls" | grep -q '### ❌ `image-promotion` failed.*Why:\*\* required gates are not green' \
  && ! grep -q 'POST .*/issues/7/comments' "$FX/calls" \
  || { echo "FAIL: report failure: the earlier image-promotion comment was not edited in place: $(grep comments "$FX/calls")"; fail=1; }

[[ $fail == 0 ]] && echo ok
exit $fail
