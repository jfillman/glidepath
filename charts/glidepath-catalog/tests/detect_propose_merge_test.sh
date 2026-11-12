#!/usr/bin/env bash
# detect-propose-merge.yaml's check step, as rendered, against a fake curl (GitHub): an APP repo's PR marked for a person to
# merge (Hangar-Merge: person on the head commit, or Clearance's agent-scope check titled "needs a person") is reported when a bot,
# an agents' login or no named account merged it, quiet when a person did, and quiet for an unmarked PR.
# Needs helm, yq (mikefarah v4) and jq.
# Usage: charts/glidepath-catalog/tests/detect_propose_merge_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
SHA=2222222222222222222222222222222222222222
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
    --show-only templates/tasks/detect-propose-merge.yaml | yq '.spec.steps[] | select(.name == "check") | .script' \
  | sed "s#/var/run/secrets/slack#$work/slack#" > "$work/step.sh"
[[ -s "$work/step.sh" ]] || { echo "FAIL: no check step rendered"; exit 1; }
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
    --show-only templates/pipelines/propose-merge-check.yaml | yq '.spec.tasks[].name' | tr '\n' ' ' | grep -q "resolve-merged-pr detect-propose-merge" \
  && echo "ok   the pipeline resolves the merged PR, then detects" || { echo "FAIL pipeline tasks"; fail=1; }

mkdir -p "$work/bin" "$work/lib" "$work/slack"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOT'
#!/usr/bin/env bash
url=""; for a in "$@"; do [[ "$a" == http* ]] && url="$a"; done
case "$url" in
  */pulls/9) cat "$FX/pr.json" ;;
  */check-runs*) cat "$FX/checks.json" ;;
  */commits/*) cat "$FX/commit.json" ;;
  *hooks.slack.test*) while [[ $# -gt 0 ]]; do [[ "$1" == "-d" ]] && printf '%s' "$2" > "$FX/slack.json"; shift; done; echo '{}' ;;
  *) echo "unexpected $url" >&2; exit 22 ;;
esac
EOT
chmod +x "$work/bin/curl"
expect() { if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi; }
scenario() { # <name> <merger login> <merger type|none> <trailer yes|no> <check title or "">
  FX="$work/fx-$1"; mkdir -p "$FX"; export FX
  if [[ "$3" == none ]]; then merged='null'; else merged="{\"login\":\"$2\",\"type\":\"$3\"}"; fi
  echo "{\"head\":{\"sha\":\"$SHA\"},\"merged_by\":$merged,\"title\":\"Declare prod\",\"html_url\":\"https://github.com/x/pull/9\"}" > "$FX/pr.json"
  msg="Declare prod"; [[ "$4" == yes ]] && msg="$msg\n\nHangar-Task: t-1\nHangar-Merge: person"
  printf '{"commit":{"message":"%s"}}' "$msg" > "$FX/commit.json"
  jq -n --arg t "$5" '{check_runs: [{name: "Clearance / agent-scope", output: {title: $t}}]}' > "$FX/checks.json"
  echo "https://hooks.slack.test/x" > "$work/slack/slack-webhook-url"
  ( cd "$FX" && PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GITHUB_TOKEN_BROKER_URL=x REPO_URL="https://github.com/jfillman/gate-api" \
      PR_NUMBER=9 MERGED=true AGENT_LOGINS="hangar-autopilot-agents[bot]" bash "$work/step.sh" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}
found() { grep -c 'PROPOSE MERGE BY NON-PERSON' "$FX/out"; }
alerted() { [[ -s "$FX/slack.json" ]] && jq -r '.attachments[0].blocks[1].fields[1].text' "$FX/slack.json" | tr '\n' ' ' || echo none; }

scenario person riley User yes ""; expect "trailer, merged by a person: no finding" "$(found)" 0
scenario bot "some-app[bot]" Bot yes ""; expect "trailer, merged by a bot: finding" "$(found)" 1
expect "the bot's merge is alerted to Slack with who" "$(alerted)" "*Merged by* some-app[bot] (Bot) "
scenario agent "hangar-autopilot-agents[bot]" User yes ""; expect "trailer, merged by the agents' login: finding" "$(found)" 1
scenario unnamed x none yes ""; expect "trailer, no merger named: finding" "$(found)" 1
scenario title-only "some-app[bot]" Bot no "needs a person: cicd.yaml"; expect "no trailer but the needs-a-person check, bot merge: finding" "$(found)" 1
scenario unmarked "some-app[bot]" Bot no "Passed: in-scope"; expect "unmarked PR merged by a bot: no finding" "$(found)" 0
scenario org acme Organization yes ""; expect "an organization-type account is not a person: finding" "$(found)" 1
expect "the step never fails the run" "$(cat "$FX/rc")" 0
[[ "${fail}" == 0 ]] && echo "PASS detect_propose_merge_test" || exit 1
