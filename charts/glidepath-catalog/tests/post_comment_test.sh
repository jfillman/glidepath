#!/usr/bin/env bash
# comment-pr-check-result.yaml's comment step with post-comment (2026-10-30): "false" still records the gate's verdict
# for the promotion fan-in and posts nothing; the default posts. agent-gates-check sets it from its agent-pr result, so
# a person's or a release PR gets no agent-gates comment while its verdict counts. Also checks that agent-gates-check
# always runs the finally task on a PR, and passes post-comment from agent-pr.
# Runs the rendered step against a fake curl (GitHub) and kubectl. Needs helm, yq (mikefarah v4) and jq.
# Usage: charts/glidepath-catalog/tests/post_comment_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/comment-pr-check-result.yaml > "$work/task.yaml" || exit 1
yq '.spec.steps[] | select(.name == "comment") | .script' "$work/task.yaml" > "$work/step.sh"
[[ -s "$work/step.sh" ]] || { echo "FAIL: no comment step rendered"; exit 1; }
expect "post-comment defaults to true" "$(yq '.spec.params[] | select(.name == "post-comment") | .default' "$work/task.yaml")" "true"

mkdir -p "$work/bin" "$work/lib"
echo 'github_app_installation_token() { echo gh-token; }' > "$work/lib/github-app.sh"
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
method=GET url=""
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -d|-H|--max-time|--retry|-o) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
echo "$method $url" >> "$FX/calls"
case "$method $url" in
  "GET "*/comments*) echo '[]' ;;
  "POST "*/comments) echo '{"html_url": "https://github.com/x/y/pull/7#c1"}' ;;
  *) echo "unexpected $method $url" >&2; exit 22 ;;
esac
EOF
cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "get taskrun"*|"get configmap"*) echo '{"items": []}' ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/"*

run() { # <name> <post-comment>
  FX="$work/$1"; mkdir -p "$FX/gate"; export FX; : > "$FX/calls"
  ( PATH="$work/bin:$PATH" PLATFORM_LIB="$work/lib" GATE_WORK="$FX/gate" REPO_URL=https://github.com/x/y PR_NUMBER=7 \
      REVISION=abc GATE_NAME=agent-gates PIPELINE_RUN_NAME=agent-gates-x1 STATUS=Succeeded ADVISORY_REASON=rollback \
      NAMESPACE=app-y-cicd POST_COMMENT="$2" bash "$work/step.sh" ) > "$FX/out" 2>&1
  echo $? > "$FX/rc"
}

run quiet false
expect "quiet: exit 0" "$(cat "$work/quiet/rc")" "0"
expect "quiet: verdict recorded" "$(cat "$work/quiet/gate/verdict" 2>/dev/null)" "success"
expect "quiet: PR recorded" "$(cat "$work/quiet/gate/pr" 2>/dev/null)" "7"
expect "quiet: no GitHub call" "$(wc -l < "$work/quiet/calls" | tr -d ' ')" "0"

run loud true
expect "loud: exit 0" "$(cat "$work/loud/rc")" "0"
expect "loud: verdict recorded" "$(cat "$work/loud/gate/verdict" 2>/dev/null)" "success"
expect "loud: comment posted" "$(grep -c '^POST .*/issues/7/comments$' "$work/loud/calls")" "1"

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/pipelines/agent-gates-check.yaml > "$work/pipeline.yaml" || exit 1
expect "agent-gates-check: the finally task runs on every PR" \
  "$(yq -o=json '.spec.finally[0].when' "$work/pipeline.yaml" | jq -c '[.[].input]')" '["$(params.pr-number)"]'
expect "agent-gates-check: post-comment is agent-pr" \
  "$(yq '.spec.finally[0].params[] | select(.name == "post-comment") | .value' "$work/pipeline.yaml")" \
  '$(tasks.agent-gates.results.agent-pr)'

[[ ${fail} -eq 0 ]] && echo "PASS post_comment_test" || { echo "FAIL post_comment_test"; exit 1; }
