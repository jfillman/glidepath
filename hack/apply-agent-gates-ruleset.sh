#!/usr/bin/env bash
# The "Glidepath App Checks" GitHub ruleset for an app repo: the required status checks on its default branch,
# `Glidepath / agent-gates-` (docs/admin/agent-gates.md) and `Glidepath / cicd-validate-`
# (docs/admin/contract.md). Gitops repos carry agent-gates- in their guardrail ruleset instead
# (hack/apply-guardrail-ruleset.sh builds it from releaseGuardrails), so this script is for app repos only and refuses
# a gitops-* name.
#
#   hack/apply-agent-gates-ruleset.sh --dry-run OWNER/REPO...   print the JSON that would be sent
#   hack/apply-agent-gates-ruleset.sh OWNER/REPO...             create the ruleset (or update it by name)
#
# Nothing else is required: no reviews, no other checks, so a person's workflow on the app repo is unchanged (their
# PR passes both checks in seconds: not an agent, cicd.yaml unchanged). A check is required only when the repo has
# its .tekton/pull-request-<check>.yaml (onboarding delivers them): a required check that nothing ever reports would
# block every PR. agent-gates- is required; without cicd-validate's file the script says so and leaves that check out,
# so re-run it after the repo's next onboarding resync.
# Until 2026-10-31 the ruleset was named "Glidepath Agent Gates" (one check); that name is updated in place.
# Options: --integration-id ID (default 4461953, the Pipelines as Code app), --app-name NAME (default Glidepath: the
# cluster's PaC application-name, catalog pipelinesAsCode.applicationName). Needs gh (admin on the repo) and jq.
set -euo pipefail
integration=4461953; app_name=Glidepath; dry=0; repos=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry=1;;
    --integration-id) integration="$2"; shift;;
    --app-name) app_name="$2"; shift;;
    *) repos+=("$1");;
  esac; shift
done
[[ ${#repos[@]} -gt 0 ]] || { echo "usage: $0 [--dry-run] [--integration-id ID] [--app-name NAME] OWNER/REPO..." >&2; exit 2; }
NAME="Glidepath App Checks"
OLD_NAME="Glidepath Agent Gates"
# ruleset_body <check>... : the ruleset JSON with these PaC checks required.
ruleset_body() {
  jq -n --arg name "${NAME}" --arg p "${app_name} / " --argjson id "${integration}" --args '{
    name: $name, target: "branch", enforcement: "active",
    conditions: {ref_name: {include: ["~DEFAULT_BRANCH"], exclude: []}},
    bypass_actors: [],
    rules: [{type: "required_status_checks", parameters: {strict_required_status_checks_policy: false,
      do_not_enforce_on_create: true,
      required_status_checks: [$ARGS.positional[] | {context: ($p + . + "-"), integration_id: $id}]}}]}' "$@"
}
for repo in "${repos[@]}"; do
  [[ "${repo#*/}" != gitops-* ]] || { echo "${repo}: a gitops repo gets agent-gates- from its guardrail ruleset (hack/apply-guardrail-ruleset.sh)" >&2; exit 2; }
  checks=(agent-gates)
  if [[ ${dry} -eq 1 ]]; then
    checks+=(cicd-validate)
    echo "# ${repo}"; ruleset_body "${checks[@]}" | jq .; continue
  fi
  gh api "repos/${repo}/contents/.tekton/pull-request-agent-gates.yaml" --silent 2>/dev/null \
    || { echo "${repo}: no .tekton/pull-request-agent-gates.yaml (run its onboarding resync first)" >&2; exit 1; }
  if gh api "repos/${repo}/contents/.tekton/pull-request-cicd-validate.yaml" --silent 2>/dev/null; then checks+=(cicd-validate)
  else echo "${repo}: no .tekton/pull-request-cicd-validate.yaml yet, so cicd-validate- is not required; re-run after its onboarding resync" >&2; fi
  body="$(ruleset_body "${checks[@]}")"
  id="$(gh api "repos/${repo}/rulesets" --jq ".[]|select(.name==\"${NAME}\" or .name==\"${OLD_NAME}\")|.id" 2>/dev/null | head -1 || true)"
  if [[ -n "${id}" ]]; then gh api -X PUT "repos/${repo}/rulesets/${id}" --input - <<<"${body}" >/dev/null && echo "updated ${repo} (#${id}): ${checks[*]}"
  else gh api -X POST "repos/${repo}/rulesets" --input - <<<"${body}" >/dev/null && echo "created ${repo}: ${checks[*]}"; fi
done
