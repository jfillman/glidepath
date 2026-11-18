#!/usr/bin/env bash
# Runs image-scan.yaml's real fetch-exceptions and scan steps (accepted findings, docs/admin/image-scan-exceptions.md) with
# the network stubbed: curl serves the GitHub API (a default branch "main" at a known commit, and the file at that commit or at the
# PR head's commit), trivy is a stub that reads its fixture scan and applies --ignorefile the way trivy 0.73.0 does
# (pkg/result/ignore.go: expired entries pruned, matches moved to ExperimentalModifiedFindings with the statement).
# Needs helm, python3 (pyyaml), jq, yq (mikefarah).
# Usage: charts/glidepath-catalog/tests/image_scan_exceptions_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
pass=0
tmp="$(mktemp -d)"
check() { # name condition-exit-code
  if [[ "$2" == 0 ]]; then pass=$((pass + 1)); else echo "FAIL: $1"; fail=1; fi
}

render() { # step-name
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/image-scan.yaml \
    | python3 -c "
import sys,yaml
t=yaml.safe_load(sys.stdin.read())
print([s for s in t['spec']['steps'] if s['name']=='$1'][0]['script'])"
}
mkdir -p "${tmp}/bin" "${tmp}/lib" "${tmp}/fx"
echo 'github_app_installation_token() { echo faketoken; }' > "${tmp}/lib/github-app.sh"
echo ':' > "${tmp}/lib/otel.sh"
render fetch-exceptions > "${tmp}/fetch.raw"
render scan > "${tmp}/scan.raw"
[[ -s "${tmp}/fetch.raw" && -s "${tmp}/scan.raw" ]] || { echo "FAIL: could not render the steps"; exit 1; }

MAIN_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
HEAD_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
cat > "${tmp}/bin/curl" <<STUB
#!/usr/bin/env bash
# curl --silent ... -o <file> -w '%{http_code}' -H ... <url>
out=""; url="\${!#}"
args=("\$@")
for ((i = 0; i < \$#; i++)); do [[ "\${args[i]}" == "-o" ]] && out="\${args[i+1]}"; done
echo "\${url}" >> "\${CURL_LOG}"
body=""; code=200
case "\${url}" in
  https://api.github.com/repos/o/gitops-app) body='{"default_branch":"main"}' ;;
  https://api.github.com/repos/o/gitops-app/commits/main) [[ "\${API_FAIL:-}" == commit ]] && code=500 || body='{"sha":"${MAIN_SHA}"}' ;;
  "https://api.github.com/repos/o/gitops-app/contents/.trivyignore.yaml?ref=${MAIN_SHA}")
    if [[ -n "\${MAIN_FILE:-}" ]]; then body="\$(cat "\${MAIN_FILE}")"; else code=404; fi ;;
  "https://api.github.com/repos/o/gitops-app/contents/.trivyignore.yaml?ref=${HEAD_SHA}")
    body="\$(cat "\${HEAD_FILE}")" ;;
  *) echo "curl stub: unexpected URL \${url}" >&2; code=500 ;;
esac
[[ -n "\${out}" ]] && printf '%s' "\${body}" > "\${out}"
printf '%s' "\${code}"
[[ "\${API_FAIL:-}" == transfer && "\${url}" == *contents* ]] && exit 18   # 200 sent, then the transfer broke
exit 0
STUB
cat > "${tmp}/bin/trivy" <<'STUB'
#!/usr/bin/env bash
# image ... [--ignorefile F] [--show-suppressed] --output O <ref>;  convert ... is a no-op.
[[ "$1" == convert ]] && exit 0
echo "$*" >> "${TRIVY_LOG}"
out=""; ign=""; args=("$@")
for ((i = 0; i < $#; i++)); do
  [[ "${args[i]}" == "--output" ]] && out="${args[i+1]}"
  [[ "${args[i]}" == "--ignorefile" ]] && ign="${args[i+1]}"
done
python3 - "${out}" "${ign}" <<'PY'
import datetime, json, sys, yaml
out, ign = sys.argv[1], sys.argv[2]
vulns = [
    {"VulnerabilityID": "CVE-2026-1111", "PkgName": "picomatch", "InstalledVersion": "4.0.3", "Severity": "HIGH", "Title": "t"},
    {"VulnerabilityID": "CVE-2026-2222", "PkgName": "zlib", "InstalledVersion": "1.3", "Severity": "CRITICAL", "Title": "t"},
]
entries = []
if ign:
    conf = yaml.safe_load(open(ign)) or {}
    now = datetime.datetime.now(datetime.timezone.utc)
    for e in conf.get("vulnerabilities") or []:
        exp = e.get("expired_at")
        if isinstance(exp, datetime.date) and not isinstance(exp, datetime.datetime):
            exp = datetime.datetime(exp.year, exp.month, exp.day, tzinfo=datetime.timezone.utc)
        if exp is not None and exp.tzinfo is None:
            exp = exp.replace(tzinfo=datetime.timezone.utc)
        if exp is not None and exp < now:   # IgnoreFindings.Prune
            continue
        entries.append(e)
kept, mod = [], []
for v in vulns:
    hit = next((e for e in entries if e["id"] == v["VulnerabilityID"]), None)
    if hit:
        mod.append({"Type": "vulnerability", "Status": "ignored", "Statement": hit.get("statement", ""), "Source": ign, "Finding": v})
    else:
        kept.append(v)
res = {"Target": "image (debian 13)", "Vulnerabilities": kept}
if mod:
    res["ExperimentalModifiedFindings"] = mod
json.dump({"Results": [res]}, open(out, "w"))
PY
STUB
chmod +x "${tmp}/bin/curl" "${tmp}/bin/trivy"

today_plus() { python3 -c "import datetime,sys;print(datetime.date.today()+datetime.timedelta(days=int(sys.argv[1])))" "$1"; }

# run <name> <exceptions repo url or ""> ; env: MAIN_FILE, HEAD_FILE, EXC_MAX_DAYS, API_FAIL. Sets $outcome, $summary, $rc, $status.
run() {
  local name="$1" repo="$2" w="${tmp}/${1}"
  mkdir -p "${w}/exc" "${w}/res"
  : > "${w}/curl.log"; : > "${w}/trivy.log"
  sed "s#/exceptions#${w}/exc#g" "${tmp}/fetch.raw" > "${w}/fetch.sh"
  sed -e "s#/tmp/#${w}/#g" -e "s#/exceptions#${w}/exc#g" \
      -e "s#\$(results.findings-summary.path)#${w}/res/summary#g" -e "s#\$(results.outcome.path)#${w}/res/outcome#g" "${tmp}/scan.raw" > "${w}/scan.sh"
  local envs=(PATH="${tmp}/bin:${PATH}" PLATFORM_LIB="${tmp}/lib" GITHUB_TOKEN_BROKER_URL=x CURL_LOG="${w}/curl.log" TRIVY_LOG="${w}/trivy.log"
              EXC_REPO_URL="${repo}" EXC_MAX_DAYS="${EXC_MAX_DAYS:-90}" MAIN_FILE="${MAIN_FILE:-}" HEAD_FILE="${HEAD_FILE:-}" API_FAIL="${API_FAIL:-}"
              IMAGE_REF=ghcr.io/o/app:1 TRIVY_SERVER_URL= FLOW_TRACEPARENT= STAGE_SPAN_ID= DOCKER_CONFIG=/nonexistent)
  env "${envs[@]}" bash "${w}/fetch.sh" > "${w}/fetch.out" 2>&1
  env "${envs[@]}" bash "${w}/scan.sh" > "${w}/scan.out" 2>&1
  rc=$?
  outcome="$(cat "${w}/res/outcome" 2>/dev/null)"; summary="$(cat "${w}/res/summary" 2>/dev/null)"
  status="$(cat "${w}/exc/status" 2>/dev/null)"; dir="${w}"
}
file() { cat > "${tmp}/fx/$1"; }   # file <name> < content ; prints nothing
has() { grep -qF -- "$1" <<<"$2"; }

REPO=https://github.com/o/gitops-app.git
d60="$(today_plus 60)"; d5="$(today_plus 5)"; d120="$(today_plus 120)"; dpast="$(today_plus -3)"; d200="$(today_plus 150)"

# 1. no .trivyignore.yaml on main: nothing is accepted, trivy is not given an ignore file, both findings count.
MAIN_FILE="" HEAD_FILE="" run nofile "${REPO}"
[[ "${status}" == none && "${outcome}" == failed && "${rc}" == 1 ]]; check "no file: failed, status none ($status/$outcome/$rc)" $?
has "2 finding(s)" "${summary}"; check "no file: both findings counted: ${summary}" $?
! has "Accepted" "${summary}" && ! grep -q -- "--ignorefile" "${dir}/trivy.log"; check "no file: no ignore file passed, nothing listed as accepted" $?

# 2. a build-time scan (no exceptions repo) never reads a file and never calls the API.
MAIN_FILE="" run buildtime ""
[[ "${status}" == none ]] && [[ ! -s "${dir}/curl.log" ]]; check "no repo: no API call" $?

# 3. valid file accepts one finding: still failing on the other, the accepted one is listed apart with its details.
file ok1 <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: "no fixed version of picomatch published; re-check before expiry"
    expired_at: ${d60}
YAML
MAIN_FILE="${tmp}/fx/ok1" run accept1 "${REPO}"
[[ "${status}" == ok && "${outcome}" == failed ]]; check "accepts one: other finding still fails ($status/$outcome)" $?
has "1 finding(s) (HIGH/CRITICAL, vuln scanner only) after 1 accepted" "${summary}" && has "CVE-2026-2222 - zlib@1.3" "${summary}"; check "accepts one: failing list has only the other: ${summary}" $?
has "Accepted, not counted above: .trivyignore.yaml on main aaaaaaaaaaaa (1 finding(s))" "${summary}"; check "accepts one: records the default-branch commit: ${summary}" $?
has "- CVE-2026-1111 picomatch@4.0.3 (HIGH): no fixed version of picomatch published; re-check before expiry - expires ${d60}" "${summary}"; check "accepts one: id, package, statement, expiry listed: ${summary}" $?
grep -q -- "--ignorefile ${dir}/exc/.trivyignore.yaml" "${dir}/trivy.log" && grep -q -- "--show-suppressed" "${dir}/trivy.log"; check "accepts one: trivy got --ignorefile and --show-suppressed" $?
! has "CVE-2026-1111 - picomatch" "${summary}" ; check "accepts one: accepted finding is not in the failing list" $?

# 4. both accepted: the gate passes, and says what it let through.
file ok2 <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: picomatch has no fix
    expired_at: ${d60}
  - id: CVE-2026-2222
    statement: zlib has no fix
    expired_at: ${d60}
YAML
MAIN_FILE="${tmp}/fx/ok2" run acceptall "${REPO}"
[[ "${outcome}" == passed && "${rc}" == 0 ]]; check "accepts all: passed ($outcome/$rc)" $?
has "0 finding(s)" "${summary}" && has "CVE-2026-1111 picomatch" "${summary}" && has "CVE-2026-2222 zlib" "${summary}"; check "accepts all: lists both: ${summary}" $?

# 5. an expired entry accepts nothing: the finding counts again, and the lapse is reported.
file expired <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: picomatch has no fix
    expired_at: ${dpast}
YAML
MAIN_FILE="${tmp}/fx/expired" run expired "${REPO}"
[[ "${status}" == ok && "${outcome}" == failed ]] && has "2 finding(s)" "${summary}"; check "expired: finding counts again: ${summary}" $?
has "expired: CVE-2026-1111 on ${dpast}" "${summary}"; check "expired: lapse reported" $?

# 6. missing expired_at / missing statement / quoted date / beyond the maximum: refused, the gate fails, trivy never runs.
refused() { # name file-content expected-text [max]
  file "$1" <<<"$2"
  MAIN_FILE="${tmp}/fx/$1" EXC_MAX_DAYS="${4:-90}" run "$1" "${REPO}"
  [[ "${status}" == refused && "${outcome}" == failed && "${rc}" == 1 ]]; check "$1: refused and failed ($status/$outcome/$rc)" $?
  has "$3" "${summary}"; check "$1: message says '$3': ${summary}" $?
  [[ ! -s "${dir}/trivy.log" ]]; check "$1: trivy never ran" $?
}
refused noexpiry $'vulnerabilities:\n  - id: CVE-2026-1111\n    statement: no fix' "vulnerabilities[0] CVE-2026-1111: expired_at is required"
refused nostatement $'vulnerabilities:\n  - id: CVE-2026-1111\n    expired_at: '"${d60}" "vulnerabilities[0] CVE-2026-1111: statement (the reason) is required"
refused quoted $'vulnerabilities:\n  - id: CVE-2026-1111\n    statement: x\n    expired_at: "'"${d60}"'"' "must be an unquoted YAML date"
refused toofar $'vulnerabilities:\n  - id: CVE-2026-1111\n    statement: x\n    expired_at: '"${d120}" "more than 90 days ahead"
refused zerodate $'vulnerabilities:\n  - id: CVE-2026-1111\n    statement: x\n    expired_at: 0001-01-01' "0001-01-01 is not a date"
refused onebad $'vulnerabilities:\n  - id: CVE-2026-1111\n    statement: x\n    expired_at: '"${d60}"$'\n  - id: CVE-2026-2222\n    statement: y' "vulnerabilities[1] CVE-2026-2222: expired_at is required"
refused typo $'vulnerabilites:\n  - id: CVE-2026-1111\n    statement: x\n    expired_at: '"${d60}" "unknown top-level key"
refused badyaml $'vulnerabilities: [' "not valid YAML"
# the maximum is configurable
file far <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: x
    expired_at: ${d120}
YAML
MAIN_FILE="${tmp}/fx/far" EXC_MAX_DAYS=200 run farok "${REPO}"
[[ "${status}" == ok ]]; check "a larger exceptions-max-days accepts 120 days ahead ($status)" $?
MAIN_FILE="${tmp}/fx/far" EXC_MAX_DAYS=100 run farno "${REPO}"
[[ "${status}" == refused ]] && has "more than 100 days ahead" "${summary}"; check "exceptions-max-days is honoured in the message" $?

# 7. read from main, not the PR head: the head copy would accept both findings, main's accepts one; only main's commit is requested.
file headall <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: head
    expired_at: ${d60}
  - id: CVE-2026-2222
    statement: head
    expired_at: ${d60}
YAML
MAIN_FILE="${tmp}/fx/ok1" HEAD_FILE="${tmp}/fx/headall" run frommain "${REPO}"
[[ "${outcome}" == failed ]] && has "CVE-2026-2222 - zlib" "${summary}"; check "main's file applies, the head copy does not: ${summary}" $?
grep -q "ref=${MAIN_SHA}" "${dir}/curl.log" && ! grep -q "${HEAD_SHA}" "${dir}/curl.log" && ! grep -q "/pulls" "${dir}/curl.log"; check "only the default branch's commit is fetched" $?
# main has no file but the head would add one: still nothing accepted.
MAIN_FILE="" HEAD_FILE="${tmp}/fx/headall" run headonly "${REPO}"
[[ "${status}" == none && "${outcome}" == failed ]]; check "a file that exists only on the PR head accepts nothing ($status/$outcome)" $?

# 8. warnings: expiring within 14 days, and an entry that matches nothing.
file warn <<YAML
vulnerabilities:
  - id: CVE-2026-1111
    statement: picomatch has no fix
    expired_at: ${d5}
  - id: CVE-2026-9999
    statement: gone
    expired_at: ${d60}
YAML
MAIN_FILE="${tmp}/fx/warn" run warn "${REPO}"
has "WARNING CVE-2026-1111 expires in 4 day(s)" "${summary}" || has "WARNING CVE-2026-1111 expires in 5 day(s)" "${summary}"; check "warns about an entry expiring within 14 days: ${summary}" $?
has "unused: CVE-2026-9999 matches nothing" "${summary}"; check "reports an unused entry" $?

# 9. an unreadable default branch fails closed as an error, not as "no exceptions".
MAIN_FILE="${tmp}/fx/ok2" API_FAIL=commit run apifail "${REPO}"
[[ "${status}" == error && "${outcome}" == error && "${rc}" == 1 ]]; check "API failure: outcome error ($status/$outcome/$rc)" $?

# 10. a transfer that fails after a 200 (curl exit != 0) is an error, never a partial file.
MAIN_FILE="${tmp}/fx/ok2" API_FAIL=transfer run xfer "${REPO}"
[[ "${status}" == error && "${outcome}" == error && "${rc}" == 1 && ! -e "${dir}/exc/.trivyignore.yaml" ]]; check "transfer failure after 200: error, no file ($status/$outcome/$rc)" $?

[[ ${fail} -eq 0 ]] && echo "ok (${pass} checks)" || exit 1
