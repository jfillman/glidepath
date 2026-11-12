#!/usr/bin/env bash
# Generates the "Glidepath Guardrail Checks" GitHub ruleset from releaseGuardrails (the single source of truth for
# which gates exist) and applies it to one or more gitops repos. See docs/admin/github-ruleset.md.
#
#   hack/apply-guardrail-ruleset.sh --dry-run OWNER/REPO...           print the JSON that would be sent
#   hack/apply-guardrail-ruleset.sh OWNER/REPO...                     create the ruleset (or update it by name)
#
# Only gates with `required: true` (the default) become required status checks; a gate with required: false is
# advisory (its check is green by construction) and is left out, exactly as detect-bypass-merge leaves it out.
# -f FILE (repeatable) layers a cluster's catalog values on the chart default - the same releaseGuardrailOverrides
# map the catalog Application passes - so the ruleset matches what that cluster renders. For OWNER/gitops-<app>, a
# gate that file's releaseGuardrailOverridesByApp makes advisory for <app> (required: false) is left out of that
# repo's ruleset too: its check still runs and can be red, and the chart does not let it hold that app's release.
# sast, provenance and image-promotion are never dropped (the chart refuses them there).
# With promotion.enabled in the merged values, image-promotion is required too, as in the chart's effective registry
# (_helpers.tpl glidepath-catalog.guardrails): it has no releaseGuardrails entry because the fan-in starts it, not a
# .tekton file (ADR-0029, docs/admin/registries.md).
#
# C-7: with agentGates.clearanceUrl set and agentGates.clearanceChecks on, the agent gate is the Clearance GitHub
# App's two check runs (`Clearance / agent-identity`, `Clearance / agent-scope`), not Glidepath's `agent-gates-`: an
# entry with `check` is required under that exact name, with `agentGates.clearanceAppId` (or --clearance-app-id) as its
# integration_id. The script refuses to build such a ruleset without the id: a required check no App reports blocks
# every PR. Apply this ruleset BEFORE the onboarding resync removes .tekton/pull-request-agent-gates.yaml
# (docs/admin/agent-gates.md, Migration).
#
# Options: --approvals N (default 2; 0 for a lab), --bypass-app-id ID (repeatable: a GitHub App allowed to bypass, e.g.
# the one Crossplane's provider-github writes with), --integration-id ID (default 4461953, the Pipelines as Code app).
# Needs gh (admin on the repo), jq, yq. Rulesets on private repos need a paid GitHub plan; on the free plan the API
# answers 403 and nothing can be enforced (the detective `bypass-check` gate is then the only control).
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
values="${here}/charts/glidepath-catalog/values.yaml"
approvals=2; integration=4461953; clearance_app_id=""; dry=0; bypass=(); repos=(); extra_values=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry=1;;
    -f|--values) extra_values+=("$2"); shift;;
    --approvals) approvals="$2"; shift;;
    --integration-id) integration="$2"; shift;;
    --clearance-app-id) clearance_app_id="$2"; shift;;
    --bypass-app-id) bypass+=("$2"); shift;;
    *) repos+=("$1");;
  esac; shift
done
[[ ${#repos[@]} -gt 0 ]] || { echo "usage: $0 [--dry-run] [-f VALUES]... [--approvals N] [--clearance-app-id ID] [--bypass-app-id ID]... OWNER/REPO..." >&2; exit 2; }

# Check names are "<pipelinesAsCode.applicationName> / <gate>-": PaC names the check-run after its application-name
# (the merged values' pipelinesAsCode.applicationName must equal the cluster's PaC ConfigMap) and the PipelineRun
# generateName, and every gate's generateName is "<gate>-" (see wait-for-release-guardrails.yaml). The required set is the chart's
# own rule (_helpers.tpl glidepath-catalog.guardrails): entry.required, default true, overridden by
# releaseGuardrailOverrides.<name>.required; a stub is always required (its check is always green).
# yq's `*` merge deep-merges maps and replaces lists, the same as Helm's values merge.
merged="$(yq -o=json ea '. as $item ireduce ({}; . * $item)' "${values}" ${extra_values[@]+"${extra_values[@]}"})"
cc_app_id="${clearance_app_id:-$(jq -r '.agentGates.clearanceAppId // ""' <<<"${merged}")}"
cc_on="$(jq -r '((.agentGates.clearanceUrl // "") != "") and ((.agentGates.clearanceChecks // false) == true)' <<<"${merged}")"
checks="$(jq --argjson id "${integration}" --arg ccid "${cc_app_id}" --argjson cc "${cc_on}" '
  ((.pipelinesAsCode.applicationName // "Glidepath") + " / ") as $p
  | (.releaseGuardrailOverrides // {}) as $o
  | [.releaseGuardrails[]
      | . as $g
      | select(($g | has("clearanceChecks") | not) or $g.clearanceChecks == $cc)
      | (if ($g.status // "real") == "stub" then true
         elif (($o[$g.name] | type) == "object" and ($o[$g.name] | has("required"))) then $o[$g.name].required
         elif ($g | has("required")) then $g.required
         else true end) as $required
      | select($required)
      | if ($g.check // "") != "" then {context: $g.check, integration_id: ($ccid | tonumber? // null), gate: $g.name}
        else {context: ($p + $g.name + "-"), integration_id: $id, gate: $g.name} end]
  + (if .promotion.enabled == true
     then [{context: ($p + "image-promotion-"), integration_id: $id, gate: "image-promotion"}] else [] end)' <<<"${merged}")"
if jq -e 'any(.[]; .integration_id == null)' <<<"${checks}" >/dev/null; then
  echo "error: the Clearance App's checks are required (agentGates.clearanceChecks) but its GitHub App id is not set: give agentGates.clearanceAppId in the values (-f) or --clearance-app-id" >&2; exit 2
fi
bypass_json="$(printf '%s\n' "${bypass[@]:-}" | jq -R 'select(length>0) | {actor_id: (.|tonumber), actor_type: "Integration", bypass_mode: "always"}' | jq -s '.')"

body_for() { # OWNER/REPO -> the ruleset JSON, without that app's per-app advisory gates
  local app="${1#*/}" repo_checks
  if [[ "${app}" == gitops-* ]]; then app="${app#gitops-}"; else app=""; fi
  repo_checks="$(jq --arg app "${app}" --argjson merged "${merged}" '
    (($merged.releaseGuardrailOverridesByApp // {})[$app] // {}) as $a
    | [.[] | .gate as $g
        | select(($g | IN("sast", "provenance", "image-promotion"))
                 or (($a[$g] | type) != "object") or ($a[$g].required != false))]' <<<"${checks}")"
  jq -n --argjson checks "${repo_checks}" --argjson approvals "${approvals}" --argjson bypass "${bypass_json}" '{
  name: "Glidepath Guardrail Checks", target: "branch", enforcement: "active",
  conditions: {ref_name: {include: ["~DEFAULT_BRANCH"], exclude: []}},
  bypass_actors: $bypass,
  rules: [
    {type: "deletion"}, {type: "non_fast_forward"},
    {type: "pull_request", parameters: {required_approving_review_count: $approvals, dismiss_stale_reviews_on_push: false,
      required_reviewers: [], require_last_push_approval: ($approvals > 0), required_review_thread_resolution: false,
      allowed_merge_methods: ["merge","squash","rebase"]}},
    {type: "required_status_checks", parameters: {strict_required_status_checks_policy: false,
      do_not_enforce_on_create: true, required_status_checks: ($checks | map(del(.gate)))}}
  ]}'
}

for repo in "${repos[@]}"; do
  body="$(body_for "${repo}")"
  if [[ ${dry} -eq 1 ]]; then echo "# ${repo}"; echo "${body}" | jq .; continue; fi
  id="$(gh api "repos/${repo}/rulesets" --jq '.[]|select(.name=="Glidepath Guardrail Checks")|.id' 2>/dev/null || true)"
  if [[ -n "${id}" ]]; then gh api -X PUT "repos/${repo}/rulesets/${id}" --input - <<<"${body}" >/dev/null && echo "updated ${repo} (#${id})"
  else gh api -X POST "repos/${repo}/rulesets" --input - <<<"${body}" >/dev/null && echo "created ${repo}"; fi
done
