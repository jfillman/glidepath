#!/usr/bin/env bash
# cicd-validate's validate step (docs/admin/contract.md): the step that reads what fetch left in /work and runs
# `glidepath validate`. Extracted from the rendered Task and run with the real validator (tools/glidepath-validate,
# needs pyyaml + jsonschema) against fixtures: an unchanged PR runs nothing; a valid cicd.yaml passes with "valid"; an
# unknown key fails with its rule; an agent's loosened gate fails GP-SCOPE-002 while a person's identical change
# passes; a new cicd.yaml in an agent's PR is compared with nothing; a PR that removes cicd.yaml fails. Also: the
# Pipeline renders and the step runs .Values.cicdValidatorImage.
# Usage: charts/glidepath-catalog/tests/cicd_validate_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
V="charts/glidepath-catalog/tests/values.example.yaml"

helm template t charts/glidepath-catalog -f "$V" --show-only templates/tasks/cicd-validate.yaml > "$d/task.yaml" || { echo "FAIL render task"; exit 1; }
helm template t charts/glidepath-catalog -f "$V" --show-only templates/pipelines/cicd-validate-check.yaml > "$d/pipeline.yaml" || { echo "FAIL render pipeline"; exit 1; }
image="$(yq '.spec.steps[] | select(.name == "validate") | .image' "$d/task.yaml")"
want="$(yq '.cicdValidatorImage' charts/glidepath-catalog/values.yaml)"
[[ "$image" == "$want" && -n "$want" ]] && echo "ok   validate runs cicdValidatorImage ($image)" || { echo "FAIL validate image: '$image' (values: '$want')"; fail=1; }
[[ "$(yq '.spec.finally[0].params[] | select(.name == "post-comment") | .value' "$d/pipeline.yaml")" == '$(tasks.cicd-validate.results.changed)' ]] \
  && echo "ok   the PR comment follows the changed result" || { echo "FAIL post-comment wiring"; fail=1; }

# The step, with /work and the result path pointed at this test's directory.
yq '.spec.steps[] | select(.name == "validate") | .script' "$d/task.yaml" \
  | sed -e "s|\"/work\"|\"$d/work\"|" -e "s|\$(results.findings-summary.path)|$d/summary|" > "$d/validate.py"
grep -q "glidepath-validate" "$d/validate.py" || { echo "FAIL validate step not found in the rendered Task"; exit 1; }
mkdir -p "$d/bin"; printf '#!/usr/bin/env bash\nexec "%s/tools/glidepath-validate" "$@"\n' "$PWD" > "$d/bin/glidepath-validate"; chmod +x "$d/bin/glidepath-validate"

# run <name> <expected rc> <meta json or ""> [head file] [base file] ; then expect <substring>...
run() {
  local name="$1" want_rc="$2" meta="$3" head="${4:-}" base="${5:-}"
  rm -rf "$d/work" "$d/summary"; mkdir -p "$d/work"
  [[ -n "$meta" ]] && printf '%s' "$meta" > "$d/work/meta.json"
  [[ -n "$head" ]] && cp "$head" "$d/work/head.yaml"
  [[ -n "$base" ]] && cp "$base" "$d/work/base.yaml"
  out="$(PATH="$d/bin:$PATH" python3 "$d/validate.py" 2>&1)"; rc=$?
  summary="$(cat "$d/summary" 2>/dev/null || true)"
  if [[ "$rc" == "$want_rc" ]]; then echo "ok   $name (exit $rc, summary: ${summary:-none})"; else echo "FAIL $name: exit $rc, wanted $want_rc"; echo "$out" | sed 's/^/    /'; fail=1; fi
}
expect() { # <name> <substring> (in the last run's output or summary)
  if [[ "$out$summary" == *"$1"* ]]; then echo "ok   says: $1"; else echo "FAIL expected '$1' in:"; echo "$out" | sed 's/^/    /'; echo "    summary: $summary"; fail=1; fi
}

ex=docs/user/examples/02-standard-ci.yaml
META='{"pr": "7", "head": "abcdef0123", "mergeBase": "0123456789", "author": "someone", "agent": AGENT, "base": BASE}'
person="${META//AGENT/false}"; person="${person//BASE/true}"
agent="${META//AGENT/true}"; agent="${agent//BASE/true}"
agent_new="${META//AGENT/true}"; agent_new="${agent_new//BASE/false}"

run "unchanged PR runs nothing" 0 ""
expect "unchanged"

run "a valid cicd.yaml passes" 0 "$person" "$ex"
expect "PASS: cicd-validate - valid"

yq '.build.cahce = {"enabled": true}' "$ex" > "$d/typo.yaml"
run "an unknown key fails with its rule" 1 "$person" "$d/typo.yaml"
expect "GP-SCHEMA-001"
expect "FAIL: cicd-validate - 1 error(s): GP-SCHEMA-001"

yq '.governance.sast = true' "$ex" > "$d/base.yaml"
yq '.governance.sast = false' "$ex" > "$d/loosened.yaml"
run "an agent loosening a gate fails GP-SCOPE-002" 1 "$agent" "$d/loosened.yaml" "$d/base.yaml"
expect "GP-SCOPE-002"
expect "agent rules against the merge base 0123456"
run "a person making the same change passes" 0 "$person" "$d/loosened.yaml" "$d/base.yaml"
expect "valid"

printf '{}\n' > "$d/empty.yaml"
run "an agent adding cicd.yaml is compared with nothing" 1 "$agent_new" "$ex" "$d/empty.yaml"
expect "no cicd.yaml there: every field counts as added"
expect "GP-SCOPE-001"

run "a PR that removes cicd.yaml fails" 1 "$person"
expect "leaves the repository without a cicd.yaml"

[[ "${fail}" == 0 ]] && echo "PASS cicd_validate_test" || exit 1
