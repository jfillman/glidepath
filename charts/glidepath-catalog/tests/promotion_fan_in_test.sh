#!/usr/bin/env bash
# comment-pr-check-result.yaml's promotion-ready step (ADR-0029 phase 3), as rendered with promotion on. Runs the step's
# bash against a fake curl (GitHub + the broker) and kubectl (the release record) and checks when it asks for a
# promotion: only when every required gate is green on the PR's head commit, counting this gate's own verdict and the
# gate ledger (two gates finishing together), never twice for one attempt, never for a stale commit, a red gate, a
# merged PR or a PR without a release file. With releaseGuardrailOverridesByApp (2026-10-28) a gate advisory for one app
# (backstage: image-scan) does not hold that app's promotion, red or last, and still holds every other app's.
# Needs helm, yq (mikefarah v4) and jq.
# Usage: charts/glidepath-catalog/tests/promotion_fan_in_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
SHA=1111111111111111111111111111111111111111
URL=https://github.com/jfillman/gitops-gate-api/pull/53

render_step() { # <out> [extra helm args...]
  local out="$1"; shift
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
    --set promotion.enabled=true --set promotion.pinDigest=true --set registries.flight.namespace=riley-roooo "$@" \
    --show-only templates/tasks/comment-pr-check-result.yaml > "$work/task.yaml" || exit 1
  yq '.spec.steps[] | select(.name == "promotion-ready") | .script' "$work/task.yaml" \
    | sed "s#/var/run/secrets/platform/broker-token#$work/broker-token#" > "$work/$out"
  [[ -s "$work/$out" ]] || { echo "FAIL: no promotion-ready step rendered ($out)"; exit 1; }
}
# Every scenario runs against the per-app render (backstage: image-scan advisory); the gate-api ones are untouched by it.
render_step step.sh --set-json 'releaseGuardrailOverridesByApp={"backstage":{"image-scan":{"required":false}}}'
render_step step-noapp.sh
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/comment-pr-check-result.yaml | yq -e '.spec.steps[] | select(.name == "promotion-ready")' >/dev/null 2>&1 \
  && { echo "FAIL: promotion-ready renders with promotion off"; fail=1; }
echo t > "$work/broker-token"

mkdir -p "$work/bin" "$work/lib"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
method=GET url="" data=""
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -d) data="$2"; shift ;;
    -H|--max-time|--retry) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
case "$method $url" in
  "POST http://el-cdevents-broker"*) printf '%s\n' "$data" >> "$FX/events"; exit 0 ;;
  "GET "*/pulls/53/files*) cat "$FX/files.json" ;;
  "GET "*/pulls/53) cat "$FX/pr.json" ;;
  "GET "*/check-runs*) cat "$FX/checks.json" ;;
  *) echo "unexpected $method $url" >&2; exit 22 ;;
esac
EOF
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "get configmap"*) cat "$FX/records.json" ;;
  "patch configmap"*)
    for a in "$@"; do [[ "$prev" == -p ]] && p="$a"; prev="$a"; done
    echo "$p" >> "$FX/patches"
    if [[ -f "$FX/conflict-once" ]]; then rm "$FX/conflict-once"; echo "Operation cannot be fulfilled: the object has been modified" >&2; exit 1; fi ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
EOF
printf '#!/usr/bin/env bash\nshasum -a 256 "$@"\n' > "$work/bin/sha256sum"
command -v sha256sum >/dev/null && rm "$work/bin/sha256sum"
chmod +x "$work/bin/"*

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
scenario() { # <name> [gate]: a promotable release PR whose required gates are green except this gate, still running
  FX="$work/fx-$1"; mkdir -p "$FX/gate"; export FX
  GATE="${2:-sbom}"
  echo success > "$FX/gate/verdict"; echo 53 > "$FX/gate/pr"
  jq -n --arg sha "$SHA" --arg u "$URL" '{state: "open", merged: false, html_url: $u, head: {sha: $sha, ref: "release-prod-f54c3652"}}' > "$FX/pr.json"
  echo '[{"filename":"hangar-prod/prod/release.yaml"}]' > "$FX/files.json"
  jq -n --arg g "$GATE" '{check_runs: [ "sast","image-scan","provenance","sbom","values","itsm","policy-validation","qa"
          | {name: ("Pipelines as Code CI / " + . + "-"), status: (if . == $g then "in_progress" else "completed" end),
             conclusion: (if . == $g then null else "success" end), started_at: "2026-10-28T10:00:00Z"} ]}' > "$FX/checks.json"
  jq -n --arg u "$URL" '{items: [{metadata: {name: "release-tracking-other", resourceVersion: "7"}, data: {prUrl: "https://github.com/jfillman/gitops-gate-api/pull/1"}},
          {metadata: {name: "release-tracking-abc", resourceVersion: "41"}, data: {prUrl: $u}}]}' > "$FX/records.json"
}
run() {
  ( cd "$FX" && PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GATE_WORK="$FX/gate" GITHUB_TOKEN_BROKER_URL=x \
      CDEVENTS_BROKER_URL=http://el-cdevents-broker.platform-system.svc.cluster.local:8080 \
      REPO_URL="https://github.com/jfillman/gitops-${APP:-gate-api}" REVISION="${REV-$SHA}" GATE_NAME="$GATE" \
      PIPELINE_RUN_NAME="$GATE-x1" NAMESPACE="app-${APP:-gate-api}-cicd" bash "$work/${STEP:-step.sh}" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}
events() { [[ -f "$FX/events" ]] && wc -l < "$FX/events" | tr -d ' ' || echo 0; }

# last gate green: asks once, with the PR, head and app; records its verdict with the record's resourceVersion.
scenario last; run
expect "last: exit 0" "$(cat "$FX/rc")" 0
expect "last: one event" "$(events)" 1
expect "last: event type" "$(jq -r .context.type "$FX/events")" dev.hangar.release.promotion-ready.0.1.0
expect "last: event content" "$(jq -c .subject.content "$FX/events")" \
  "{\"appName\":\"gate-api\",\"repoUrl\":\"https://github.com/jfillman/gitops-gate-api\",\"prNumber\":\"53\",\"headSha\":\"$SHA\",\"lastGate\":\"sbom\"}"
expect "last: event id is 20 hex" "$(jq -r .context.id "$FX/events" | grep -cE '^[0-9a-f]{20}$')" 1
expect "last: ledger patch" "$(cat "$FX/patches")" '{"metadata":{"resourceVersion":"41"},"data":{"gate.111111111111.sbom":"success"}}'
id_first="$(jq -r .context.id "$FX/events")"

# another gate still running and no ledger entry for it: waits for it.
scenario peer-running
jq '.check_runs |= map(if .name == "Pipelines as Code CI / values-" then .status = "in_progress" | .conclusion = null else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run
expect "peer-running: no event" "$(events)" 0
expect "peer-running: says why" "$(grep -c 'values (in_progress)' "$FX/out")" 1

# two gates finishing together: the other one's check run is still in progress but its verdict is in the ledger.
scenario together
jq '.check_runs |= map(if .name == "Pipelines as Code CI / values-" then .status = "in_progress" | .conclusion = null else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
jq '.items[1].data["gate.111111111111.values"] = "success"' "$FX/records.json" > "$FX/x" && mv "$FX/x" "$FX/records.json"
run
expect "together: one event" "$(events)" 1

# the ledger outranks a green check run: a /retest that went red is red.
scenario ledger-red
jq '.items[1].data["gate.111111111111.itsm"] = "failure"' "$FX/records.json" > "$FX/x" && mv "$FX/x" "$FX/records.json"
run
expect "ledger-red: no event" "$(events)" 0
expect "ledger-red: says itsm" "$(grep -c 'itsm (failure)' "$FX/out")" 1

# this gate red: recorded, nothing asked.
scenario own-red; echo failure > "$FX/gate/verdict"; run
expect "own-red: no event" "$(events)" 0
expect "own-red: verdict recorded" "$(jq -r '.data["gate.111111111111.sbom"]' "$FX/patches")" failure

# an advisory gate (qa) finishing last still asks; a missing required gate does not.
scenario advisory-last qa; run
expect "advisory-last: one event" "$(events)" 1
scenario missing; jq '.check_runs |= map(select(.name != "Pipelines as Code CI / policy-validation-"))' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run
expect "missing: no event" "$(events)" 0

# already promoted, or a promotion running: nothing; after a refused promotion the next attempt gets a new id.
scenario promoted
jq '.check_runs += [{name: "Pipelines as Code CI / image-promotion-", status: "completed", conclusion: "success", started_at: "2026-10-28T10:05:00Z"}]' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run; expect "promoted: no event" "$(events)" 0
scenario promoting
jq '.check_runs += [{name: "Pipelines as Code CI / image-promotion-", status: "in_progress", conclusion: null, started_at: "2026-10-28T10:05:00Z"}]' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run; expect "promoting: no event" "$(events)" 0
scenario retry
jq '.check_runs += [{name: "Pipelines as Code CI / image-promotion-", status: "completed", conclusion: "failure", started_at: "2026-10-28T10:05:00Z"}]' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"
run
expect "retry: one event" "$(events)" 1
[[ "$(jq -r .context.id "$FX/events")" != "${id_first}" ]] && echo "ok   retry: a new attempt id" || { echo "FAIL retry: same id as the first attempt"; fail=1; }

# the record changed underneath: the patch is retried with the new resourceVersion read again.
scenario conflict; touch "$FX/conflict-once"; run
expect "conflict: two patches" "$(wc -l < "$FX/patches" | tr -d ' ')" 2
expect "conflict: one event" "$(events)" 1

# no release record (a PR made by hand): the check runs alone decide.
scenario no-record; echo '{"items":[]}' > "$FX/records.json"; run
expect "no-record: one event" "$(events)" 1
expect "no-record: no patch" "$([[ -f "$FX/patches" ]] && echo yes || echo no)" no

# stale commit, merged PR, not a release PR, no release file: nothing recorded, nothing asked.
scenario stale; REV=2222222222222222222222222222222222222222 run
expect "stale: no event" "$(events)" 0
expect "stale: no patch" "$([[ -f "$FX/patches" ]] && echo yes || echo no)" no
scenario merged; jq '.merged = true | .state = "closed"' "$FX/pr.json" > "$FX/x" && mv "$FX/x" "$FX/pr.json"; run
expect "merged: no event" "$(events)" 0
scenario not-release; jq '.head.ref = "fix-values"' "$FX/pr.json" > "$FX/x" && mv "$FX/x" "$FX/pr.json"; run
expect "not-release: no event" "$(events)" 0
scenario no-release-file; echo '[{"filename":"hangar-prod/prod/values.yaml"}]' > "$FX/files.json"; run
expect "no-release-file: no event" "$(events)" 0
expect "no-release-file: exit 0" "$(cat "$FX/rc")" 0

# releaseGuardrailOverridesByApp: image-scan is red (the trixie base CVEs) on every scenario below.
scan_red() { jq '.check_runs |= map(if .name == "Pipelines as Code CI / image-scan-" then .conclusion = "failure" else . end)' "$FX/checks.json" > "$FX/x" && mv "$FX/x" "$FX/checks.json"; }
# backstage, sbom last and green, image-scan red: image-scan is advisory for backstage, so it asks.
scenario app-advisory; scan_red; APP=backstage run
expect "app-advisory: one event" "$(events)" 1
expect "app-advisory: app is backstage" "$(jq -r .subject.content.appName "$FX/events" 2>/dev/null)" backstage
expect "app-advisory: says why" "$(grep -c 'advisory for backstage (catalog releaseGuardrailOverridesByApp), not waited for: image-scan' "$FX/out")" 1
# another app with the same red image-scan: still held.
scenario app-other; scan_red; APP=gate-api run
expect "app-other: no event" "$(events)" 0
expect "app-other: says image-scan" "$(grep -c 'image-scan (failure)' "$FX/out")" 1
# image-scan itself finishing last, red: for backstage it still asks (else an advisory gate that finished last would
# leave the release unpromoted), and its red verdict is recorded in the ledger as it is.
scenario app-advisory-last image-scan; echo failure > "$FX/gate/verdict"; APP=backstage run
expect "app-advisory-last: one event" "$(events)" 1
expect "app-advisory-last: verdict recorded" "$(jq -r '.data["gate.111111111111.image-scan"]' "$FX/patches")" failure
scenario app-other-last image-scan; echo failure > "$FX/gate/verdict"; APP=gate-api run
expect "app-other-last: no event" "$(events)" 0
# a red required gate still holds backstage: only the listed gate is advisory.
scenario app-advisory-sbom-red; scan_red
jq '.items[1].data["gate.111111111111.values"] = "failure"' "$FX/records.json" > "$FX/x" && mv "$FX/x" "$FX/records.json"
APP=backstage run
expect "app-advisory-sbom-red: no event" "$(events)" 0
expect "app-advisory-sbom-red: says values" "$(grep -c 'values (failure)' "$FX/out")" 1
# without the override (chart default render) backstage is held like any app: the lookup comes from the values.
scenario app-no-override; scan_red; APP=backstage STEP=step-noapp.sh run
expect "app-no-override: no event" "$(events)" 0

[[ "${fail}" == 0 ]] && echo "PASS promotion_fan_in_test" || exit 1
