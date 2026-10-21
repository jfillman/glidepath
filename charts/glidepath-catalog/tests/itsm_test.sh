#!/usr/bin/env bash
# Change management (docs/admin/change-management.md) against hack/lab/itsm_mock_server.py: the provider client
# (files/lib/itsm.sh) for both providers, then the rendered itsm-gate and itsm-change Task scripts with a stub
# kubectl standing in for the release records, through the whole lifecycle: create -> gate fails naming the
# change and its state -> approve -> gate passes -> implement -> close successful; and a rollback: emergency
# change linked to the original, original closed unsuccessful with the rollback change related. Also: provider
# none renders itsm-check as the loud stub and no lifecycle tasks; an unauthenticated call against a mock that
# wants a token fails loudly.
# Needs helm, python3 (pyyaml), jq, curl. Usage: charts/glidepath-catalog/tests/itsm_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
tmp="$(mktemp -d)"
values=charts/glidepath-catalog/tests/values.example.yaml
expect() { [[ "$2" == "$3" ]] || { echo "FAIL $1:"; echo "  got:  $2"; echo "  want: $3"; fail=1; }; }
contains() { [[ "$2" == *"$3"* ]] || { echo "FAIL $1: '$3' not in:"; echo "  $2"; fail=1; }; }

# ---- the mock
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1])')"
python3 hack/lab/itsm_mock_server.py "${port}" 2>"${tmp}/mock.log" &
mock_pid=$!
trap 'kill ${mock_pid} 2>/dev/null; rm -rf "${tmp}"' EXIT
for i in $(seq 1 50); do curl -fs "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fs "http://127.0.0.1:${port}/healthz" >/dev/null || { echo "FAIL: mock did not start"; cat "${tmp}/mock.log"; exit 1; }
base="http://127.0.0.1:${port}"

# ---- the client, servicenow provider
export ITSM_PROVIDER=servicenow ITSM_SERVICENOW_URL="${base}" ITSM_WEBHOOK_URL="${base}/webhook" ITSM_CREDENTIALS_DIR="${tmp}/no-creds"
source charts/glidepath-catalog/files/lib/itsm.sh
c="$(itsm_create promote boarding-api prod ghcr.io/o/boarding-api:1.0.0 chain-1:hangar-prod/prod "" 2>"${tmp}/err")"
id="$(jq -r .id <<<"${c}")"; number="$(jq -r .number <<<"${c}")"
expect "sn create number" "${number}" "CHG0010001"
expect "sn create approved" "$(jq -r .approved <<<"${c}")" "false"
contains "sn create warns about missing credentials" "$(cat "${tmp}/err")" "no change-tool credentials"
expect "sn create type" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[0].type')" "normal"
expect "sn create correlation" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[0].correlation_id')" "chain-1:hangar-prod/prod"
itsm_approved "${c}" && { echo "FAIL: a new change must not be approved"; fail=1; }
expect "sn get by number" "$(itsm_get "${number}" | jq -r .id)" "${id}"
itsm_link_pr "${id}" "https://github.com/o/gitops-boarding-api/pull/9" >/dev/null
contains "sn link_pr work note" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[0].work_notes')" "Release PR: https://github.com/o/gitops-boarding-api/pull/9"
curl -fs -X POST "${base}/_mock/approve/${id}" >/dev/null
c="$(itsm_get "${id}")"
expect "sn approved after approval" "$(jq -r '[.state,.approval,.approved]|join(" ")' <<<"${c}")" "-2 approved true"
itsm_approved "${c}" || { echo "FAIL: an approved change must be approved"; fail=1; }
# approvedStates: a change in Implement counts without the approval field; Assess does not
ITSM_SERVICENOW_APPROVED_STATES="-1" ; c2="$(_sn_normalize <<<'{"result":{"sys_id":"x","number":"CHG1","state":"-1","approval":"requested"}}')"
expect "approved by state" "$(jq -r .approved <<<"${c2}")" "true"
c2="$(_sn_normalize <<<'{"result":{"sys_id":"x","number":"CHG1","state":"-4","approval":"requested"}}')"
expect "not approved by state" "$(jq -r .approved <<<"${c2}")" "false"
ITSM_SERVICENOW_APPROVED_STATES="-2,-1"
expect "sn implement" "$(itsm_implement "${id}" "merged" | jq -r .state)" "-1"
expect "sn close" "$(itsm_close "${id}" successful "healthy" | jq -r .state)" "3"
expect "sn close code" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[0].close_code')" "successful"
# a rollback: emergency change with the original as parent; original closed unsuccessful with the rollback related
r="$(itsm_create rollback boarding-api prod ghcr.io/o/boarding-api:0.9.0 chain-2:hangar-prod/prod "" "${id}")"
expect "sn rollback type" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[1].type')" "emergency"
expect "sn rollback parent" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[1].parent')" "${id}"
itsm_close "${id}" unsuccessful "rolled back" "$(jq -r .number <<<"${r}")" >/dev/null
contains "sn close related" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[0].work_notes')" "related change: CHG0010002"

# ---- the client, webhook provider (same store, the adapter's own shape)
export ITSM_PROVIDER=webhook
w="$(itsm_create promote gate-api prod ghcr.io/o/gate-api:2.0.0 chain-3:hangar-prod/prod https://github.com/o/gitops-gate-api/pull/3)"
wid="$(jq -r .id <<<"${w}")"
expect "wh create" "$(jq -r '[.number,.approved]|join(" ")' <<<"${w}")" "CHG0010003 false"
contains "wh create pr note" "$(curl -fs "${base}/_mock/changes" | jq -r '.result[2].work_notes')" "pull/3"
curl -fs -X POST "${base}/_mock/approve/${wid}" >/dev/null
expect "wh get approved" "$(itsm_get "${wid}" | jq -r .approved)" "true"
expect "wh implement" "$(itsm_implement "${wid}" "merged" | jq -r .state)" "-1"
expect "wh note keeps state" "$(itsm_note "${wid}" "degraded" | jq -r .state)" "-1"
expect "wh close" "$(itsm_close "${wid}" successful "healthy" | jq -r .state)" "3"
export ITSM_PROVIDER=none
expect "none is a no-op" "$(itsm_create promote a b c d e)" ""
itsm_enabled && { echo "FAIL: none must not be enabled"; fail=1; }

# ---- credentials: a bearer token from the mounted Secret, and a loud failure without one
MOCK_TOKEN=s3cret python3 hack/lab/itsm_mock_server.py "$((port+1))" 2>/dev/null &
mock2=$!; trap 'kill ${mock_pid} ${mock2} 2>/dev/null; rm -rf "${tmp}"' EXIT
for i in $(seq 1 50); do curl -fs "http://127.0.0.1:$((port+1))/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
export ITSM_PROVIDER=servicenow ITSM_SERVICENOW_URL="http://127.0.0.1:$((port+1))"
if itsm_create promote a prod img rid "" >/dev/null 2>"${tmp}/err"; then echo "FAIL: unauthenticated call must fail"; fail=1; fi
contains "401 is loud" "$(cat "${tmp}/err")" "HTTP 401"
mkdir -p "${tmp}/creds" && printf 's3cret' > "${tmp}/creds/token"
expect "bearer token from the Secret" "$(ITSM_CREDENTIALS_DIR="${tmp}/creds" itsm_create promote a prod img rid "" | jq -r .number)" "CHG0010001"
export ITSM_SERVICENOW_URL="${base}" ITSM_CREDENTIALS_DIR="${tmp}/no-creds"

# ---- the rendered Task scripts, through the lifecycle, with a stub kubectl holding the release records
render() { helm template t charts/glidepath-catalog -f "${values}" --namespace platform-catalog "$@"; }
script_of() { python3 -c 'import sys,yaml
for d in yaml.safe_load_all(sys.stdin.read()):
    if d and d.get("kind")=="Task" and d["metadata"]["name"]==sys.argv[1]: print(d["spec"]["steps"][0]["script"])' "$1"; }
render --set itsm.provider=servicenow --set itsm.servicenow.instanceUrl="${base}" > "${tmp}/sn.yaml"
script_of itsm-gate < "${tmp}/sn.yaml" > "${tmp}/itsm-gate.sh"
script_of itsm-change < "${tmp}/sn.yaml" > "${tmp}/itsm-change.sh"
[[ -s "${tmp}/itsm-gate.sh" && -s "${tmp}/itsm-change.sh" ]] || { echo "FAIL: itsm Tasks not rendered with a provider"; fail=1; }
bash -n "${tmp}/itsm-gate.sh" && bash -n "${tmp}/itsm-change.sh" || { echo "FAIL: bash -n"; fail=1; }
# the Tekton results paths and the lib's own PLATFORM_LIB source
mkdir -p "${tmp}/results" "${tmp}/lib" "${tmp}/bin"
sed -i.bak -e "s#\$(results\.\([a-z-]*\)\.path)#${tmp}/results/\1#g" "${tmp}/itsm-gate.sh" "${tmp}/itsm-change.sh"
printf 'github_app_installation_token() { echo tok; }\n' > "${tmp}/lib/github-app.sh"
# stub kubectl: the records live in $RECORDS (a JSON list of ConfigMaps); patches are appended to $PATCHLOG
cat > "${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "get configmap")
    if [[ "$*" == *"-l hangar.io/subcomponent=release-tracking"* ]]; then jq -c '{items: .}' "${RECORDS}"
    elif [[ "$*" == *"-o name"* ]]; then jq -r --arg n "$3" '.[] | select(.metadata.name == $n) | "configmap/" + .metadata.name' "${RECORDS}"
    else path="$(sed -nE 's/.*\{\.data\.([A-Za-z]+)\}.*/\1/p' <<<"$*")"; jq -r --arg n "$3" --arg k "${path}" '.[] | select(.metadata.name == $n) | .data[$k] // ""' "${RECORDS}" | tr -d '\n'; fi ;;
  "patch configmap")
    printf '%s %s\n' "$3" "$(jq -c . <<<"${@: -1}")" >> "${PATCHLOG}"
    # apply the patch to the records so a later read sees it
    jq --arg n "$3" --argjson p "${@: -1}" 'map(if .metadata.name == $n then .data += $p.data else . end)' "${RECORDS}" > "${RECORDS}.new" && mv "${RECORDS}.new" "${RECORDS}" ;;
  *) echo "stub kubectl: unexpected $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
# curl to GitHub must not happen in the gate when the record has the change (PR body is the fallback only)
cat > "${tmp}/bin/curl" <<CURL
#!/usr/bin/env bash
case "\$*" in *api.github.com*) echo "stub curl: GitHub must not be called: \$*" >&2; exit 1;; esac
exec /usr/bin/curl "\$@"
CURL
chmod +x "${tmp}/bin/curl"
[[ -x /usr/bin/curl ]] || sed -i.bak "s#/usr/bin/curl#$(command -v curl)#" "${tmp}/bin/curl"

export RECORDS="${tmp}/records.json" PATCHLOG="${tmp}/patches.log"
: > "${PATCHLOG}"
c="$(itsm_create promote boarding-api prod ghcr.io/o/boarding-api:3.0.0 chain-9:hangar-prod/prod "")"
cid="$(jq -r .id <<<"${c}")"; cnum="$(jq -r .number <<<"${c}")"
jq -n --arg id "${cid}" --arg num "${cnum}" '[{metadata: {name: "release-tracking-chain-9"}, data: {prUrl: "https://github.com/o/gitops-boarding-api/pull/12", releaseId: "chain-9:hangar-prod/prod", image: "ghcr.io/o/boarding-api:3.0.0", cluster: "hangar-prod", env: "prod", changeRequestId: $id, changeRequestNumber: $num, state: "proposed"}}]' > "${RECORDS}"
gate() { PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x REPO_URL="https://github.com/o/gitops-boarding-api.git" PR_NUMBER="$1" REVISION=abc NAMESPACE=app-boarding-api-cicd \
  ITSM_PROVIDER=servicenow ITSM_SERVICENOW_URL="${base}" ITSM_SERVICENOW_APPROVED_STATES="-2,-1" ITSM_WEBHOOK_URL="" ITSM_CREDENTIALS_DIR="${tmp}/no-creds" bash "${tmp}/itsm-gate.sh"; }
change() { PATH="${tmp}/bin:${PATH}" NAMESPACE=app-boarding-api-cicd ACTION="$1" REPO_URL="${2:-}" PR_NUMBER="${3:-}" CHAIN_ID="${4:-}" STATUS="${5:-}" \
  ITSM_PROVIDER=servicenow ITSM_SERVICENOW_URL="${base}" ITSM_SERVICENOW_APPROVED_STATES="-2,-1" ITSM_WEBHOOK_URL="" ITSM_CREDENTIALS_DIR="${tmp}/no-creds" bash "${tmp}/itsm-change.sh"; }

if gate 12 >"${tmp}/out" 2>"${tmp}/err"; then echo "FAIL: the gate must fail on an unapproved change"; fail=1; fi
contains "gate names the change and its state" "$(cat "${tmp}/err")" "change ${cnum} for https://github.com/o/gitops-boarding-api/pull/12 is in state '-5' with approval 'not requested'"
contains "gate summary on failure" "$(cat "${tmp}/results/findings-summary")" "Change ${cnum} is not approved"
contains "gate records the state" "$(cat "${PATCHLOG}")" '"changeRequestState":"-5"'
curl -fs -X POST "${base}/_mock/approve/${cid}" >/dev/null
gate 12 >"${tmp}/out" 2>"${tmp}/err" || { echo "FAIL: the gate must pass on an approved change: $(cat "${tmp}/err")"; fail=1; }
contains "gate summary on pass" "$(cat "${tmp}/results/findings-summary")" "Change ${cnum} is approved (state -2, approval approved)"
expect "gate change number result" "$(cat "${tmp}/results/change-request-number")" "${cnum}"
# no record and no marker: the gate says so (the PR body fallback is reached through GitHub, stubbed to fail loudly here)
echo '[]' > "${tmp}/empty.json"
if RECORDS="${tmp}/empty.json" gate 12 >/dev/null 2>"${tmp}/err"; then echo "FAIL: no record must fail"; fail=1; fi
contains "no record fails loudly" "$(cat "${tmp}/err")" "stub curl: GitHub must not be called"
# implement, then close successful
change implement https://github.com/o/gitops-boarding-api.git 12 > "${tmp}/out" || { echo "FAIL: implement"; fail=1; }
contains "implement log" "$(cat "${tmp}/out")" "change ${cnum} -> implementing"
expect "implement state in the tool" "$(itsm_get "${cid}" | jq -r .state)" "-1"
contains "implement on the record" "$(tail -1 "${PATCHLOG}")" '"changeRequestState":"implementing"'
change close-outcome "" "" chain-9 Failed > "${tmp}/out" || { echo "FAIL: close-outcome Failed"; fail=1; }
expect "failed outcome leaves the change open" "$(itsm_get "${cid}" | jq -r .state)" "-1"
contains "failed outcome on the record" "$(tail -1 "${PATCHLOG}")" '"changeRequestState":"implementing-failed"'
change close-outcome "" "" chain-9 Succeeded > "${tmp}/out" || { echo "FAIL: close-outcome Succeeded"; fail=1; }
expect "healthy closes the change" "$(itsm_get "${cid}" | jq -r .state)" "3"
expect "healthy close code" "$(curl -fs "${base}/_mock/changes" | jq -r --arg id "${cid}" '.result[] | select(.sys_id == $id) | .close_code')" "successful"
contains "closed on the record" "$(tail -1 "${PATCHLOG}")" '"changeRequestState":"closed-successful"'
# a rollback of chain-9 by chain-10: the original's change closes unsuccessful, related to the emergency change
r="$(itsm_create rollback boarding-api prod ghcr.io/o/boarding-api:2.0.0 chain-10:hangar-prod/prod "" "${cid}")"
rid="$(jq -r .id <<<"${r}")"; rnum="$(jq -r .number <<<"${r}")"
jq --arg id "${rid}" --arg num "${rnum}" '. + [{metadata: {name: "release-tracking-chain-10"}, data: {prUrl: "https://github.com/o/gitops-boarding-api/pull/13", releaseId: "chain-10:hangar-prod/prod", image: "ghcr.io/o/boarding-api:2.0.0", cluster: "hangar-prod", env: "prod", kind: "rollback", rollbackOf: "chain-9:hangar-prod/prod", rollbackReason: "p99 doubled", changeRequestId: $id, changeRequestNumber: $num}}]' "${RECORDS}" > "${RECORDS}.new" && mv "${RECORDS}.new" "${RECORDS}"
change close-rolled-back "" "" chain-10 > "${tmp}/out" || { echo "FAIL: close-rolled-back"; fail=1; }
contains "rolled-back log" "$(cat "${tmp}/out")" "change ${cnum} closed unsuccessful (rolled back by ${rnum})"
expect "rolled-back close code" "$(curl -fs "${base}/_mock/changes" | jq -r --arg id "${cid}" '.result[] | select(.sys_id == $id) | .close_code')" "unsuccessful"
contains "rolled-back close note names the reason and the emergency change" "$(curl -fs "${base}/_mock/changes" | jq -r --arg id "${cid}" '.result[] | select(.sys_id == $id) | .work_notes')" "by emergency change ${rnum}. Reason: p99 doubled (related change: ${rnum})"
contains "rolled-back on the original record" "$(tail -1 "${PATCHLOG}")" "\"changeRequestState\":\"closed-unsuccessful\",\"rolledBackByChange\":\"${rnum}\""
# no change on the record: every action is a successful no-op
jq 'map(.data.changeRequestId = "")' "${RECORDS}" > "${RECORDS}.new" && mv "${RECORDS}.new" "${RECORDS}"
change implement https://github.com/o/gitops-boarding-api.git 12 > "${tmp}/out" || { echo "FAIL: implement without a change must succeed"; fail=1; }
contains "implement without a change" "$(cat "${tmp}/out")" "has no change request"

# ---- the Pipelines: provider none is the loud stub with no lifecycle tasks; a provider renders the gate (hard, or advisory by the registry)
tasks() { python3 -c 'import sys,yaml
for d in yaml.safe_load_all(sys.stdin.read()):
    if d and d.get("kind")=="Pipeline" and d["metadata"]["name"]==sys.argv[1]:
        print(" ".join(t["name"] + ("+continue" if t.get("onError")=="continue" else "") for t in d["spec"]["tasks"]))' "$1"; }
render > "${tmp}/none.yaml"
expect "none: itsm-check is the stub" "$(tasks itsm-check < "${tmp}/none.yaml")" "check"
expect "none: no itsm-implement" "$(tasks bypass-merge-check < "${tmp}/none.yaml")" "resolve-merged-pr detect-bypass-merge mark-release-merged"
expect "none: no itsm-close" "$(tasks release-rollback-notify < "${tmp}/none.yaml")" "notify-backstage"
grep -q 'name: itsm-gate' "${tmp}/none.yaml" && { echo "FAIL: itsm-gate rendered without a provider"; fail=1; }
grep -q '| `itsm` | [^|]* | not yet real' <(script_of open-release-pr < "${tmp}/none.yaml") || { echo "FAIL: none: PR body must call itsm a stub"; fail=1; }
expect "servicenow: itsm-check verifies" "$(tasks itsm-check < "${tmp}/sn.yaml")" "verify-change"
expect "servicenow: implement after merge" "$(tasks bypass-merge-check < "${tmp}/sn.yaml")" "resolve-merged-pr detect-bypass-merge mark-release-merged itsm-implement+continue"
expect "servicenow: close on rollback" "$(tasks release-rollback-notify < "${tmp}/sn.yaml")" "notify-backstage itsm-close-rolled-back+continue"
grep -q '| `itsm` | [^|]* | real |' <(script_of open-release-pr < "${tmp}/sn.yaml") || { echo "FAIL: servicenow: PR body must call itsm real"; fail=1; }
render --set itsm.provider=webhook --set itsm.webhook.url="${base}/webhook" --set releaseGuardrailOverrides.itsm.required=false > "${tmp}/wh.yaml"
expect "webhook, not required: advisory" "$(tasks itsm-check < "${tmp}/wh.yaml")" "verify-change-advisory+continue"
grep -q 'ITSM_WEBHOOK_URL' "${tmp}/wh.yaml" || { echo "FAIL: webhook url not rendered"; fail=1; }
grep -q 'resolve-release-tracking' "${tmp}/sn.yaml" && grep -q 'name: change-request-number' "${tmp}/sn.yaml" || { echo "FAIL: change-request results missing"; fail=1; }

[[ ${fail} -eq 0 ]] && echo "OK: the change-management lifecycle holds against the mock for both providers"
exit ${fail}
