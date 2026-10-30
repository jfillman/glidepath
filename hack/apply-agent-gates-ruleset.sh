#!/usr/bin/env bash
# The "Glidepath Agent Gates" GitHub ruleset for an app repo: one required status check, `Pipelines as Code CI /
# agent-gates-` (docs/admin/agent-gates.md), on the default branch. Gitops repos carry the same check in their
# guardrail ruleset instead (hack/apply-guardrail-ruleset.sh builds it from releaseGuardrails), so this script is for
# app repos only and refuses a gitops-* name.
#
#   hack/apply-agent-gates-ruleset.sh --dry-run OWNER/REPO...   print the JSON that would be sent
#   hack/apply-agent-gates-ruleset.sh OWNER/REPO...             create the ruleset (or update it by name)
#
# Nothing else is required: no reviews, no other checks, so a person's workflow on the app repo is unchanged (their
# PR passes the gate as not-agent in seconds). The repo must already have .tekton/pull-request-agent-gates.yaml
# (onboarding delivers it), or no PR could ever satisfy the check.
# Options: --integration-id ID (default 4461953, the Pipelines as Code app). Needs gh (admin on the repo) and jq.
set -euo pipefail
integration=4461953; dry=0; repos=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry=1;;
    --integration-id) integration="$2"; shift;;
    *) repos+=("$1");;
  esac; shift
done
[[ ${#repos[@]} -gt 0 ]] || { echo "usage: $0 [--dry-run] [--integration-id ID] OWNER/REPO..." >&2; exit 2; }
NAME="Glidepath Agent Gates"

body="$(jq -n --arg name "${NAME}" --argjson id "${integration}" '{
  name: $name, target: "branch", enforcement: "active",
  conditions: {ref_name: {include: ["~DEFAULT_BRANCH"], exclude: []}},
  bypass_actors: [],
  rules: [{type: "required_status_checks", parameters: {strict_required_status_checks_policy: false,
    do_not_enforce_on_create: true,
    required_status_checks: [{context: "Pipelines as Code CI / agent-gates-", integration_id: $id}]}}]}')"

for repo in "${repos[@]}"; do
  [[ "${repo#*/}" != gitops-* ]] || { echo "${repo}: a gitops repo gets agent-gates- from its guardrail ruleset (hack/apply-guardrail-ruleset.sh)" >&2; exit 2; }
  if [[ ${dry} -eq 1 ]]; then echo "# ${repo}"; echo "${body}" | jq .; continue; fi
  gh api "repos/${repo}/contents/.tekton/pull-request-agent-gates.yaml" --silent 2>/dev/null \
    || { echo "${repo}: no .tekton/pull-request-agent-gates.yaml (run its onboarding resync first)" >&2; exit 1; }
  id="$(gh api "repos/${repo}/rulesets" --jq ".[]|select(.name==\"${NAME}\")|.id" 2>/dev/null || true)"
  if [[ -n "${id}" ]]; then gh api -X PUT "repos/${repo}/rulesets/${id}" --input - <<<"${body}" >/dev/null && echo "updated ${repo} (#${id})"
  else gh api -X POST "repos/${repo}/rulesets" --input - <<<"${body}" >/dev/null && echo "created ${repo}"; fi
done
