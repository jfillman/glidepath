#!/usr/bin/env bash
# Pipeline trust T4 step 2: runs record-gate-verdict.yaml's and join-release.yaml's real scripts against a stub kubectl.
# Checks the ledger patch (resourceVersion CAS, retry), the ledger result (this commit's keys only, under Tekton's 4 KiB
# result limit even for a 20-gate ledger of the longest names), and the join (one patch per environment, best effort).
# Needs helm, python3 (pyyaml) and jq. Usage: charts/glidepath-catalog/tests/carve_out_tasks_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"

extract() { # task -> script path with the result paths pointed at files in $work
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only "templates/tasks/$1.yaml" \
    | python3 -c "import sys,yaml; print(yaml.safe_load(sys.stdin.read())['spec']['steps'][0]['script'])" \
    | sed -e "s#\$(results\.\([a-z-]*\)\.path)#${work}/result-\1#g" > "${work}/$1.sh"
}
extract record-gate-verdict
extract join-release
[[ -s "${work}/record-gate-verdict.sh" && -s "${work}/join-release.sh" ]] || { echo "FAIL: could not render the Tasks"; exit 1; }

stubdir="${work}/bin"; mkdir -p "${stubdir}"
cat > "${stubdir}/kubectl" <<'STUB'
#!/usr/bin/env bash
# get: print $RECORDS; patch: log "<name> <body>" and fail the first $FAIL_PATCHES times (a lost resourceVersion race)
case "$1" in
  get) printf '%s' "${RECORDS}" ;;
  patch)
    n="$(cat "${COUNTER}" 2>/dev/null || echo 0)"; echo $((n + 1)) > "${COUNTER}"
    [[ "${n}" -lt "${FAIL_PATCHES:-0}" ]] && exit 1
    printf '%s %s\n' "$3" "$(jq -c . <<<"$9")" >> "${PATCHLOG}" ;;
esac
STUB
chmod +x "${stubdir}/kubectl"
printf '#!/usr/bin/env bash\nexit 0\n' > "${stubdir}/sleep"; chmod +x "${stubdir}/sleep"
PATCHLOG="${work}/patch.log"; COUNTER="${work}/counter"; export PATCHLOG COUNTER

PR=https://github.com/o/gitops-app/pull/7
SHA=0123456789abcdef0123456789abcdef01234567
rec() { # existing ledger data (JSON object) -> RECORDS
  jq -nc --arg u "${PR}" --argjson d "$1" '{items: [{metadata: {name: "release-tracking-b", resourceVersion: "42"}, data: ({prUrl: $u, image: "reg/o/app@sha256:abc"} + $d)},
    {metadata: {name: "release-tracking-a", resourceVersion: "1"}, data: {prUrl: "https://github.com/o/gitops-app/pull/6"}}]}'
}
verdict() { # records verdict gate [fail-patches] -> runs the script; prints the patch log; the result is in ${work}/result-ledger
  : > "${PATCHLOG}"; echo 0 > "${COUNTER}"
  PATH="${stubdir}:${PATH}" PR_URL="${PR}" HEAD_SHA="${SHA}" GATE_NAME="$3" VERDICT="$2" NAMESPACE=app-x-cicd RECORDS="$1" FAIL_PATCHES="${4:-0}" \
    bash "${work}/record-gate-verdict.sh" >/dev/null 2>&1 || { echo "FAIL: script exited non-zero"; fail=1; }
  cat "${PATCHLOG}"
}

out="$(verdict "$(rec '{}')" success sast)"
[[ "${out}" == 'release-tracking-b {"metadata":{"resourceVersion":"42"},"data":{"gate.0123456789ab.sast":"success"}}' ]] || { echo "FAIL: ledger patch: ${out}"; fail=1; }
[[ "$(cat "${work}/result-ledger")" == '{"gate.0123456789ab.sast":"success"}' ]] || { echo "FAIL: ledger result: $(cat "${work}/result-ledger")"; fail=1; }

# the result carries this commit's keys only (not the record's other data, not another commit's verdicts)
out="$(verdict "$(rec '{"gate.0123456789ab.qa":"failure","gate.ffffffffffff.sast":"success","state":"proposed"}')" success sast)"
[[ "$(cat "${work}/result-ledger")" == '{"gate.0123456789ab.qa":"failure","gate.0123456789ab.sast":"success"}' ]] || { echo "FAIL: ledger result keys: $(cat "${work}/result-ledger")"; fail=1; }

# a lost race retries and still records
out="$(verdict "$(rec '{}')" failure qa 2)"
[[ "$(wc -l < "${work}/patch.log" | tr -d ' ')" == 1 && "$(cat "${COUNTER}")" == 3 ]] || { echo "FAIL: retry: patches=$(cat "${COUNTER}")"; fail=1; }
[[ "$(cat "${work}/result-ledger")" == '{"gate.0123456789ab.qa":"failure"}' ]] || { echo "FAIL: ledger after retry"; fail=1; }

# five lost races: nothing recorded, still exit 0, result {}
out="$(verdict "$(rec '{}')" success sast 99)"
[[ -z "${out}" && "$(cat "${work}/result-ledger")" == '{}' && "$(cat "${COUNTER}")" == 5 ]] || { echo "FAIL: give-up: ${out} $(cat "${work}/result-ledger") $(cat "${COUNTER}")"; fail=1; }

# no record for the PR, and bad inputs: nothing written, result {}
out="$(verdict '{"items":[]}' success sast)"
[[ -z "${out}" && "$(cat "${work}/result-ledger")" == '{}' ]] || { echo "FAIL: no record: ${out}"; fail=1; }
out="$(verdict "$(rec '{}')" maybe sast)"
[[ -z "${out}" ]] || { echo "FAIL: a verdict that is not success or failure was written: ${out}"; fail=1; }
out="$(verdict "$(rec '{}')" success 'a b')"
[[ -z "${out}" ]] || { echo "FAIL: a gate name with a space was written: ${out}"; fail=1; }

# the 4 KiB result limit: 20 gates, the longest names the pattern allows (63 chars), 12-char sha key prefix
big="$(jq -nc '[range(0; 20)] | map({key: ("gate.0123456789ab." + ("g" * 61) + (. | tostring | if length < 2 then "0" + . else . end)), value: "success"}) | from_entries')"
verdict "$(rec "${big}")" success sast >/dev/null
size="$(wc -c < "${work}/result-ledger" | tr -d ' ')"
echo "20-gate worst-case ledger result: ${size} bytes (limit 4096)"
[[ "${size}" -gt 1900 && "${size}" -lt 4096 ]] || { echo "FAIL: ledger result ${size} bytes"; fail=1; }
# the real catalog: every gate name in values.yaml, written for one commit
names="$(python3 -c "import yaml; print(' '.join(g['name'] for g in yaml.safe_load(open('charts/glidepath-catalog/values.yaml'))['releaseGuardrails']))")"
d='{}'; for g in ${names} image-promotion; do d="$(jq -c --arg k "gate.0123456789ab.${g}" '. + {($k): "success"}' <<<"${d}")"; done
verdict "$(rec "${d}")" success sast >/dev/null
echo "real catalog ledger ($(wc -w <<<"${names} image-promotion" | tr -d ' ') gates): $(wc -c < "${work}/result-ledger" | tr -d ' ') bytes"

join() { # records joins -> patch log
  : > "${PATCHLOG}"; echo 0 > "${COUNTER}"
  PATH="${stubdir}:${PATH}" REPO_URL="https://github.com/o/gitops-app.git" JOINS="$2" NAMESPACE=app-x-cicd RECORDS="$1" \
    bash "${work}/join-release.sh" >/dev/null 2>&1 || { echo "FAIL: join script exited non-zero"; fail=1; }
  cat "${PATCHLOG}"
}
jrecs="$(jq -nc --arg u "${PR}" '{items: [{metadata: {name: "release-tracking-b"}, data: {prUrl: $u}}]}')"
out="$(join "${jrecs}" '[{"cluster":"c1","env":"prod","id":"chg_1","tree":"sha256:aa","pr":"7"},{"cluster":"c2","env":"stage","id":"chg_2","tree":"sha256:bb","pr":7}]')"
[[ "$(wc -l <<<"${out}" | tr -d ' ')" == 2 && "${out}" == *'"clearanceId":"chg_1"'* && "${out}" == *'"clearanceTree":"sha256:bb"'* ]] || { echo "FAIL: join, two environments: ${out}"; fail=1; }
[[ "$(cat "${work}/result-joined")" == "release-tracking-b,release-tracking-b" ]] || { echo "FAIL: joined result: $(cat "${work}/result-joined")"; fail=1; }
out="$(join "${jrecs}" '[{"cluster":"c1","env":"prod","id":"","tree":"","pr":"7"},{"cluster":"c1","env":"prod","id":"chg_1","tree":"t","pr":"abc"}]')"
[[ -z "${out}" ]] || { echo "FAIL: join wrote with no id or a bad PR number: ${out}"; fail=1; }
out="$(join "${jrecs}" '[{"cluster":"c1","env":"prod","id":"chg_1","tree":"t","pr":"9"}]')"
[[ -z "${out}" ]] || { echo "FAIL: join patched a record of another PR: ${out}"; fail=1; }
out="$(join "${jrecs}" 'not json')"
[[ -z "${out}" ]] || { echo "FAIL: join wrote from a non-array: ${out}"; fail=1; }

[ $fail -eq 0 ] && echo ok || exit 1
