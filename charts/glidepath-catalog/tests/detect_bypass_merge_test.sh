#!/usr/bin/env bash
# detect-bypass-merge.yaml's check step, as rendered, against a fake curl (GitHub) - the break-glass detector's view of
# a merged gitops PR. 2026-10-28: with releaseGuardrailOverridesByApp a gate advisory for one app (backstage:
# image-scan) is not a bypass when that app's PR merges past it red, and still is for every other app; a red gate
# that is not advisory for the app is still reported, and a non-gitops repo gets no per-app exemption.
# Needs helm, yq (mikefarah v4) and jq.
# Usage: charts/glidepath-catalog/tests/detect_bypass_merge_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
SHA=1111111111111111111111111111111111111111

render() { # <out> [extra helm args...]
  local out="$1"; shift
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml "$@" \
    --show-only templates/tasks/detect-bypass-merge.yaml | yq '.spec.steps[] | select(.name == "check") | .script' \
    | sed "s#/var/run/secrets/slack#$work/slack#" > "$work/$out"
  [[ -s "$work/$out" ]] || { echo "FAIL: no check step rendered ($out)"; exit 1; }
}
render step.sh --set-json 'releaseGuardrailOverridesByApp={"backstage":{"image-scan":{"required":false}}}'
render step-default.sh

mkdir -p "$work/bin" "$work/lib"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
url=""
for a in "$@"; do [[ "$a" == http* ]] && url="$a"; done
case "$url" in
  */pulls/9) cat "$FX/pr.json" ;;
  */check-runs*) cat "$FX/checks.json" ;;
  *) echo "unexpected $url" >&2; exit 22 ;;
esac
EOF
chmod +x "$work/bin/"*

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
scenario() { # <name> <red gate,...>: a merged PR whose head has every registry gate green except the listed ones
  FX="$work/fx-$1"; mkdir -p "$FX"; export FX
  jq -n --arg sha "$SHA" '{head: {sha: $sha}, merged_by: {login: "riley"}, title: "Release", html_url: "https://github.com/x/pull/9"}' > "$FX/pr.json"
  jq -n --arg red ",$2," '{check_runs: [ "sast","image-scan","provenance","sbom","values","itsm","policy-validation","qa","api-compat"
          | . as $g | {name: ("Pipelines as Code CI / " + $g + "-"), status: "completed",
             conclusion: (if ($red | contains("," + $g + ",")) then "failure" else "success" end)} ]}' > "$FX/checks.json"
}
run() { # <repo> [step]
  ( cd "$FX" && PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GITHUB_TOKEN_BROKER_URL=x \
      REPO_URL="https://github.com/jfillman/$1" PR_NUMBER=9 MERGED=true bash "$work/${2:-step.sh}" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}
bypass() { grep -o 'BYPASS DETECTED.*' "$FX/out" | sed 's/.*despite: //'; }

scenario backstage-scan image-scan; run gitops-backstage
expect "backstage, image-scan red: exit 0" "$(cat "$FX/rc")" 0
expect "backstage, image-scan red: no bypass" "$(bypass)" ""
expect "backstage, image-scan red: normal merge" "$(grep -c 'normal merge, no bypass detected' "$FX/out")" 1

scenario other-scan image-scan; run gitops-gate-api
expect "gate-api, image-scan red: bypass" "$(bypass)" "image-scan (failure)"

scenario backstage-sbom image-scan,sbom; run gitops-backstage
expect "backstage, image-scan + sbom red: sbom only" "$(bypass)" "sbom (failure)"

scenario not-gitops image-scan; run backstage
expect "repo backstage (not gitops-backstage): bypass" "$(bypass)" "image-scan (failure)"

scenario default-render image-scan; run gitops-backstage step-default.sh
expect "chart default, backstage, image-scan red: bypass" "$(bypass)" "image-scan (failure)"

[[ "${fail}" == 0 ]] && echo "PASS detect_bypass_merge_test" || exit 1
