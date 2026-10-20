#!/usr/bin/env bash
# charts/glidepath-catalog/files/lib/itsm.sh - the change-management provider contract (docs/admin/change-management.md).
#
# One small shell/curl client behind one interface, with a provider switch: `servicenow` (the Table API on
# change_request, the few calls below) or `webhook` (one HTTP endpoint that adapts to whatever change tool a
# company runs). `none` means no change tool: every function is a no-op that prints nothing, and the itsm gate is
# the loud stub. Embedded into each Task that talks to the change tool (open-release-pr, itsm-gate, itsm-change)
# by the chart (Files.Get: a Pod cannot mount a ConfigMap from the catalog's namespace, see the toolbox
# Dockerfile) and sourced directly by tests/itsm_test.sh against tests/itsm_mock_server.py.
#
# Environment (rendered from .Values.itsm by the chart, or set by a test):
#   ITSM_PROVIDER                    none | servicenow | webhook
#   ITSM_SERVICENOW_URL              https://<instance>.service-now.com (no trailing slash)
#   ITSM_SERVICENOW_APPROVED_STATES  change_request.state values that count as approved or scheduled, comma
#                                    separated (default "-2,-1": Scheduled, Implement). approval == "approved"
#                                    always counts.
#   ITSM_WEBHOOK_URL                 the adapter endpoint, POSTed a JSON body per call (contract below)
#   ITSM_CREDENTIALS_DIR             the mounted platform Secret (default /var/run/secrets/itsm): `token` (sent as
#                                    a bearer) or `username` + `password` (basic). Neither: unauthenticated, with a
#                                    warning. Never a value in values.yaml.
#
# Every call prints ONE JSON object for the change request on stdout and nothing else:
#   {"id": "<provider id>", "number": "<human id, CHG...>", "url": "<link>", "state": "<raw state>",
#    "approval": "<raw approval>", "approved": true|false}
# `approved` is the only field a gate decides on. A failed HTTP call exits non-zero with the response on stderr.
#
# Webhook contract: POST ${ITSM_WEBHOOK_URL} with {"action": "create"|"get"|"link"|"implement"|"close",
# "change_id": "...", "release": {kind, app, env, image, release_id, pr_url, rollback_of_change}, "note": "...",
# "close_code": "successful"|"unsuccessful", "related_change_id": "..."} and the adapter answers the JSON object
# above (its own judgement of `approved`). The same Authorization header as ServiceNow.

: "${ITSM_PROVIDER:=none}"
# The chart leaves an unset URL out of the step's env (an empty value does not survive the API server); under
# `set -u` they must still be defined.
: "${ITSM_SERVICENOW_URL:=}"
: "${ITSM_WEBHOOK_URL:=}"
: "${ITSM_CREDENTIALS_DIR:=/var/run/secrets/itsm}"
: "${ITSM_SERVICENOW_APPROVED_STATES:=-2,-1}"
_ITSM_AUTH_WARNED=""

itsm_enabled() {
  [[ "${ITSM_PROVIDER}" == "servicenow" || "${ITSM_PROVIDER}" == "webhook" ]]
}

# _itsm_curl METHOD URL [JSON-BODY] -> response body on stdout; exits 1 with the body on stderr on a non-2xx.
_itsm_curl() {
  local method="$1" url="$2" body="${3:-}"
  local -a auth=()
  if [[ -s "${ITSM_CREDENTIALS_DIR}/token" ]]; then
    auth=(-H "Authorization: Bearer $(cat "${ITSM_CREDENTIALS_DIR}/token")")
  elif [[ -s "${ITSM_CREDENTIALS_DIR}/username" && -s "${ITSM_CREDENTIALS_DIR}/password" ]]; then
    auth=(-u "$(cat "${ITSM_CREDENTIALS_DIR}/username"):$(cat "${ITSM_CREDENTIALS_DIR}/password")")
  elif [[ -z "${_ITSM_AUTH_WARNED}" ]]; then
    echo "warn: no change-tool credentials at ${ITSM_CREDENTIALS_DIR} (Secret itsm-credentials: token, or username+password) - calling ${ITSM_PROVIDER} unauthenticated" >&2
    _ITSM_AUTH_WARNED=1
  fi
  local -a data=()
  [[ -n "${body}" ]] && data=(-H "Content-Type: application/json" -d "${body}")
  local raw http_code
  raw="$(curl --silent --show-error --max-time 20 --retry 3 --retry-connrefused -w '\n%{http_code}' \
    -X "${method}" ${auth[@]+"${auth[@]}"} -H "Accept: application/json" ${data[@]+"${data[@]}"} "${url}")"
  http_code="${raw##*$'\n'}"
  if [[ "${http_code}" != 2* ]]; then
    echo "error: ${ITSM_PROVIDER} ${method} ${url}: HTTP ${http_code}: ${raw%$'\n'*}" >&2
    return 1
  fi
  printf '%s' "${raw%$'\n'*}"
}

# ---- ServiceNow (Table API, change_request) ----------------------------------------------------------------

_sn_fields="sys_id,number,state,approval,type,short_description,close_code,parent,correlation_id"

# _sn_normalize <table API response with .result as an object> -> the contract object
_sn_normalize() {
  jq -c --arg base "${ITSM_SERVICENOW_URL:-}" --arg states "${ITSM_SERVICENOW_APPROVED_STATES}" '
    .result | (if type == "array" then .[0] // {} else . end) | (.state // "") as $st |
    {id: (.sys_id // ""), number: (.number // ""),
     url: (if .sys_id then ($base + "/nav_to.do?uri=change_request.do?sys_id=" + .sys_id) else "" end),
     state: $st, approval: (.approval // ""),
     approved: ((.approval == "approved") or any(($states | split(","))[]; . == $st))}'
}

_sn_create() {
  local kind="$1" app="$2" env="$3" image="$4" release_id="$5" rollback_of_change="${6:-}"
  local type="normal" short="Release ${app} to ${env}: ${image}" parent=""
  if [[ "${kind}" == "rollback" ]]; then
    type="emergency"; short="Rollback ${app} ${env} to ${image}"
    # `parent` links the emergency change to the change it rolls back (a standard Task field).
    [[ -n "${rollback_of_change}" ]] && parent="${rollback_of_change}"
  fi
  local body
  body="$(jq -n --arg type "${type}" --arg short "${short}" --arg parent "${parent}" \
    --arg desc "$(printf 'Glidepath release\napp: %s\nenvironment: %s\nimage: %s\nrelease id: %s\nkind: %s' "${app}" "${env}" "${image}" "${release_id}" "${kind}")" \
    --arg rid "${release_id}" '{type: $type, short_description: $short, description: $desc, category: "Software",
      correlation_id: $rid, correlation_display: "glidepath"} + (if $parent != "" then {parent: $parent} else {} end)')"
  local resp
  resp="$(_itsm_curl POST "${ITSM_SERVICENOW_URL}/api/now/table/change_request?sysparm_fields=${_sn_fields}" "${body}")" || return 1
  _sn_normalize <<<"${resp}"
}

# _sn_get <sys_id or CHG number>
_sn_get() {
  local id="$1" resp
  if [[ "${id}" == CHG* ]]; then
    resp="$(_itsm_curl GET "${ITSM_SERVICENOW_URL}/api/now/table/change_request?sysparm_query=number=${id}&sysparm_fields=${_sn_fields}&sysparm_limit=1")" || return 1
  else
    resp="$(_itsm_curl GET "${ITSM_SERVICENOW_URL}/api/now/table/change_request/${id}?sysparm_fields=${_sn_fields}")" || return 1
  fi
  _sn_normalize <<<"${resp}"
}

# _sn_patch <sys_id> <json fields>
_sn_patch() {
  local resp
  resp="$(_itsm_curl PATCH "${ITSM_SERVICENOW_URL}/api/now/table/change_request/$1?sysparm_fields=${_sn_fields}" "$2")" || return 1
  _sn_normalize <<<"${resp}"
}

# ---- Webhook adapter ---------------------------------------------------------------------------------------

# _wh_call <json body>
_wh_call() {
  local resp
  resp="$(_itsm_curl POST "${ITSM_WEBHOOK_URL}" "$1")" || return 1
  jq -c '{id: (.id // ""), number: (.number // .id // ""), url: (.url // ""),
    state: (.state // ""), approval: (.approval // ""), approved: (.approved == true)}' <<<"${resp}"
}

# ---- The interface -----------------------------------------------------------------------------------------

# itsm_create <promote|rollback> <app> <env> <image> <release-id> <pr-url or ""> [rollback-of change id]
itsm_create() {
  itsm_enabled || return 0
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_create "$1" "$2" "$3" "$4" "$5" "${7:-}" ;;
    webhook) _wh_call "$(jq -n --arg kind "$1" --arg app "$2" --arg env "$3" --arg image "$4" --arg rid "$5" --arg pr "${6:-}" --arg rb "${7:-}" \
      '{action: "create", release: {kind: $kind, app: $app, env: $env, image: $image, release_id: $rid, pr_url: $pr, rollback_of_change: $rb}}')" ;;
  esac
}

# itsm_get <id>
itsm_get() {
  itsm_enabled || return 0
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_get "$1" ;;
    webhook) _wh_call "$(jq -n --arg id "$1" '{action: "get", change_id: $id}')" ;;
  esac
}

# itsm_link_pr <id> <pr-url> - records the release PR on the change
itsm_link_pr() {
  itsm_enabled || return 0
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_patch "$1" "$(jq -n --arg n "Release PR: $2" '{work_notes: $n}')" ;;
    webhook) _wh_call "$(jq -n --arg id "$1" --arg pr "$2" '{action: "link", change_id: $id, release: {pr_url: $pr}}')" ;;
  esac
}

# itsm_implement <id> <note> - the PR merged: the change is being implemented
itsm_implement() {
  itsm_enabled || return 0
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_patch "$1" "$(jq -n --arg n "$2" '{state: "-1", work_notes: $n}')" ;;
    webhook) _wh_call "$(jq -n --arg id "$1" --arg n "$2" '{action: "implement", change_id: $id, note: $n}')" ;;
  esac
}

# itsm_note <id> <note> - a work note, state unchanged (a deploy that reported failure but may still recover)
itsm_note() {
  itsm_enabled || return 0
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_patch "$1" "$(jq -n --arg n "$2" '{work_notes: $n}')" ;;
    webhook) _wh_call "$(jq -n --arg id "$1" --arg n "$2" '{action: "note", change_id: $id, note: $n}')" ;;
  esac
}

# itsm_close <id> <successful|unsuccessful> <note> [related change id] - the release's outcome is final
itsm_close() {
  itsm_enabled || return 0
  local related="${4:-}"
  case "${ITSM_PROVIDER}" in
    servicenow) _sn_patch "$1" "$(jq -n --arg code "$2" --arg n "$3" --arg rel "${related}" \
      '{state: "3", close_code: $code, close_notes: $n, work_notes: (if $rel != "" then ($n + " (related change: " + $rel + ")") else $n end)}')" ;;
    webhook) _wh_call "$(jq -n --arg id "$1" --arg code "$2" --arg n "$3" --arg rel "${related}" \
      '{action: "close", change_id: $id, close_code: $code, note: $n, related_change_id: $rel}')" ;;
  esac
}

# itsm_approved <change json> -> 0 when the change is approved or scheduled
itsm_approved() {
  jq -e '.approved == true' <<<"$1" >/dev/null
}
