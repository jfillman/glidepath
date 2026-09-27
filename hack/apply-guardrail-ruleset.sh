#!/usr/bin/env bash
# Generates the "Glidepath Guardrail Checks" GitHub ruleset from releaseGuardrails (the single source of truth for
# which gates exist) and applies it to one or more gitops repos. See docs/admin/github-ruleset.md.
#
#   hack/apply-guardrail-ruleset.sh --dry-run OWNER/REPO...           print the JSON that would be sent
#   hack/apply-guardrail-ruleset.sh OWNER/REPO...                     create the ruleset (or update it by name)
#
# Options: --approvals N (default 2; 0 for a lab), --bypass-app-id ID (repeatable: a GitHub App allowed to bypass, e.g.
# the one Crossplane's provider-github writes with), --integration-id ID (default 4461953, the Pipelines as Code app).
# Needs gh (admin on the repo), jq, yq. Rulesets on private repos need a paid GitHub plan; on the free plan the API
# answers 403 and nothing can be enforced (the detective `bypass-check` gate is then the only control).
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
values="${here}/charts/glidepath-catalog/values.yaml"
approvals=2; integration=4461953; dry=0; bypass=(); repos=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry=1;;
    --approvals) approvals="$2"; shift;;
    --integration-id) integration="$2"; shift;;
    --bypass-app-id) bypass+=("$2"); shift;;
    *) repos+=("$1");;
  esac; shift
done
[[ ${#repos[@]} -gt 0 ]] || { echo "usage: $0 [--dry-run] [--approvals N] [--bypass-app-id ID]... OWNER/REPO..." >&2; exit 2; }

# Check names are "Pipelines as Code CI / <gate>-": PaC names the check-run after the PipelineRun generateName, and
# every gate's generateName is "<gate>-" (see wait-for-release-guardrails.yaml).
checks="$(yq -o=json '.releaseGuardrails[].name' "${values}" | jq -s --argjson id "${integration}" \
  'map({context: ("Pipelines as Code CI / " + . + "-"), integration_id: $id})')"
bypass_json="$(printf '%s\n' "${bypass[@]:-}" | jq -R 'select(length>0) | {actor_id: (.|tonumber), actor_type: "Integration", bypass_mode: "always"}' | jq -s '.')"

body="$(jq -n --argjson checks "${checks}" --argjson approvals "${approvals}" --argjson bypass "${bypass_json}" '{
  name: "Glidepath Guardrail Checks", target: "branch", enforcement: "active",
  conditions: {ref_name: {include: ["~DEFAULT_BRANCH"], exclude: []}},
  bypass_actors: $bypass,
  rules: [
    {type: "deletion"}, {type: "non_fast_forward"},
    {type: "pull_request", parameters: {required_approving_review_count: $approvals, dismiss_stale_reviews_on_push: false,
      required_reviewers: [], require_last_push_approval: ($approvals > 0), required_review_thread_resolution: false,
      allowed_merge_methods: ["merge","squash","rebase"]}},
    {type: "required_status_checks", parameters: {strict_required_status_checks_policy: false,
      do_not_enforce_on_create: true, required_status_checks: $checks}}
  ]}')"

for repo in "${repos[@]}"; do
  if [[ ${dry} -eq 1 ]]; then echo "# ${repo}"; echo "${body}" | jq .; continue; fi
  id="$(gh api "repos/${repo}/rulesets" --jq '.[]|select(.name=="Glidepath Guardrail Checks")|.id' 2>/dev/null || true)"
  if [[ -n "${id}" ]]; then gh api -X PUT "repos/${repo}/rulesets/${id}" --input - <<<"${body}" >/dev/null && echo "updated ${repo} (#${id})"
  else gh api -X POST "repos/${repo}/rulesets" --input - <<<"${body}" >/dev/null && echo "created ${repo}"; fi
done
