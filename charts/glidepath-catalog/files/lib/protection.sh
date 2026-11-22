# files/lib/protection.sh - which environments clearance-gate registers, and what Clearance's answer means for the PR
# comment (docs/admin/clearance-gate.md, "Which environments are protected"). It rides in the fetch and register steps'
# environment (PROTECTION_LIB), not in their scripts: see the RELEASE_SOURCE_LIB comment in tasks/clearance-gate.yaml.
#
# The platform decides protection, never the app. Clearance's core decides it for every change it is sent (clearance
# C-6): an environment is protected when CLEARANCE_PROTECTED_ENVIRONMENTS names it (rendered from the core cluster's
# cluster.yaml, which no app team or pipeline account can write; empty protects every environment), or when a cluster's
# Argo CD agent reported the app and environment as protected. A change to any other environment is cleared at tier 0,
# with the reason `unprotected`, and recorded. So with a Clearance configured, every environment a PR changes is
# registered. Until Clearance answers, an environment's protection is unknown, and unknown counts as protected: nothing is
# skipped because of what the app's own repository says. cicd.yaml's `production: true` is a declaration. It is
# compared with Clearance's answer and any disagreement is shown in the PR comment. It no longer decides what is
# registered. The one exception is a cluster with no Clearance (agentGates.clearanceUrl empty). No platform answer
# exists there and no sync-time gate can exist either, so the declaration still decides, as before: a production: true
# environment fails the check ("no Clearance") and any other environment is skipped.
#
#   protection_declared <environments json> <env>             cicd.yaml deploy.environments[name == env].production:
#                                                              true | false | missing
#   protection_register <declared> <clearance url>             whether the gate renders and registers the environment:
#                                                              true | false
#   protection_platform                                        Clearance's answer for one change (its JSON on stdin):
#                                                              protected | unprotected | unknown
#   protection_verdict <cluster/env> <declared> <platform>     {protected, warning} as one line of JSON: protected is what
#                                                              the comment reports (unknown counts as protected); warning
#                                                              is null or names a disagreement between cicd.yaml and Clearance

protection_declared() {
  local out
  out="$(jq -r --arg e "$2" '[(if type == "array" then .[] else empty end) | select(type == "object" and .name == $e) | .production] | .[0]
    | if . == true or . == "true" then "true" elif . == null then "missing" else "false" end' <<<"$1" 2>/dev/null)" || out=missing
  case "${out}" in true|false|missing) printf '%s\n' "${out}" ;; *) echo missing ;; esac
}

protection_register() {
  if [[ -n "$2" ]]; then echo true; elif [[ "$1" == true ]]; then echo true; else echo false; fi
}

# Clearance's Change carries `protected` (what the core decided, clearance internal/gate/change.go). A body without it
# falls back to the reasons: an `unprotected` reason is the core's tier-0 clearance of an environment it does not protect.
protection_platform() {
  local out
  out="$(jq -rs 'if length != 1 or (.[0] | type) != "object" then "unknown" else .[0]
    | if .protected == true then "protected" elif .protected == false then "unprotected"
      elif any(.reasons[]?; type == "object" and .rule == "unprotected") then "unprotected"
      else "unknown" end end' 2>/dev/null)" || out=unknown
  case "${out}" in protected|unprotected) printf '%s\n' "${out}" ;; *) echo unknown ;; esac
}

protection_verdict() {
  jq -nc --arg where "$1" --arg declared "$2" --arg platform "$3" '
    ($declared | if . == "true" then "says `production: true`" elif . == "false" then "says `production: false`"
                 else "does not say `production: true`" end) as $says
    | if $platform == "unprotected" then
        {protected: false, warning: (if $declared == "true" then
          "cicd.yaml says `production: true` for `\($where)`, but Clearance does not protect it, so the change was cleared at tier 0 (unprotected). An app cannot protect an environment by itself. A platform admin adds it to Clearance'"'"'s protected environments (cluster.yaml `clearance.protectedEnvironments`)."
          else null end)}
      elif $platform == "protected" then
        {protected: true, warning: (if $declared == "true" then null else
          "Clearance protects `\($where)`, but cicd.yaml \($says) for it. The change was registered and gated anyway, because the platform decides, not the app. Add `production: true` to that environment in cicd.yaml so the two agree."
          end)}
      else
        {protected: true, warning: "Clearance did not say whether `\($where)` is protected, so it is treated as protected (cicd.yaml \($says))."}
      end'
}
