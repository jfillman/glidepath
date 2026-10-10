#!/usr/bin/env bash
# Runs the release-record sweeper's real script against a stub kubectl and checks what it
# alerts on and deletes. Needs helm, python3 (pyyaml), jq, GNU date.
# Usage: charts/glidepath-control-plane/tests/release_record_sweeper_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
script="$(mktemp)"
helm template t charts/glidepath-control-plane --set platformCicdRepoUrl=https://x/y.git \
  --show-only templates/broker/release-record-sweeper-cronjob.yaml \
  | python3 -c "
import sys,yaml
cj=[d for d in yaml.safe_load_all(sys.stdin.read()) if d and d['kind']=='CronJob'][0]
print(cj['spec']['jobTemplate']['spec']['template']['spec']['containers'][0]['args'][0])" > "$script"

stub="$(mktemp -d)"
cat > "${stub}/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "get namespace") echo "namespace/app-x-cicd"; echo "namespace/kube-system" ;;
  "get configmap") [[ "${NOLIST:-}" == 1 ]] && exit 1; printf '%s' "${RECORDS}" ;;
  "delete configmap") echo "DELETE $3" >> "${LOG}" ;;
  "label configmap") echo "LABEL $3 $6" >> "${LOG}" ;;
  "create -f") echo "EVENT $(grep -E '^reason:|^  name:' | tr '\n' ' ')" >> "${LOG}" ;;
esac
STUB
chmod +x "${stub}/kubectl"

# macOS date has no -d; the toolbox's date does. A shim covers the forms used here:
#   date -u +FORMAT | date -u -d "<ISO8601Z>" +FORMAT | date -u -d "<N> <unit> ago" +FORMAT
if ! date -u -d "2026-01-01T00:00:00Z" +%s >/dev/null 2>&1; then
  cat > "${stub}/date" <<'SHIM'
#!/usr/bin/env python3
import sys, re, datetime as dt
a = sys.argv[1:]
a = [x for x in a if x != "-u"]
t = dt.datetime.now(dt.timezone.utc)
if a and a[0] == "-d":
    spec = a[1]; a = a[2:]
    m = re.match(r"^(\d+) (second|minute|hour|day)s? ago$", spec)
    if m:
        n = int(m.group(1)); u = {"second": "seconds", "minute": "minutes", "hour": "hours", "day": "days"}[m.group(2)]
        t = t - dt.timedelta(**{u: n})
    else:
        t = dt.datetime.strptime(spec, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)
fmt = a[0][1:] if a else "%a %b %d %H:%M:%S UTC %Y"
print(t.strftime(fmt.replace("%s", str(int(t.timestamp())))))
SHIM
  chmod +x "${stub}/date"
fi

ago() { PATH="${stub}:${PATH}" date -u -d "$* ago" +%Y-%m-%dT%H:%M:%SZ; }
rec() { # name state stateAt mergedAt lastFactAt created alerted
  jq -n --arg n "$1" --arg s "$2" --arg sa "$3" --arg m "$4" --arg l "$5" --arg c "$6" --arg a "$7" \
    '{metadata:{name:$n,creationTimestamp:$c,labels:(if $a=="" then {} else {"hangar.io/stall-alerted":$a} end)},data:({state:$s,stateAt:$sa,mergedAt:$m,lastFactAt:$l}|with_entries(select(.value!="")))}'
}
run() { LOG="$(mktemp)"; export LOG; PATH="${stub}:${PATH}" RECORDS="{\"items\":[$1]}" NOLIST="${2:-0}" RELEASE_STALL_MINUTES=45 RECORD_TTL_DAYS=14 bash "$script" >/dev/null 2>&1 || { echo "FAIL: script exited non-zero"; fail=1; }; cat "$LOG"; }
expect() { # description needle haystack
  grep -q -- "$2" <<<"$3" || { echo "FAIL: $1 (wanted '$2', got: ${3:-nothing})"; fail=1; }; }
refute() { grep -q -- "$2" <<<"$3" && { echo "FAIL: $1 (did not want '$2', got: $3)"; fail=1; }; true; }

out="$(run "$(rec merged-old merged "$(ago 2 hours)" "$(ago 2 hours)" "" "$(ago 2 hours)" "")")"
expect "a merged release with no fact for 2h is stalled" "ReleaseStalled" "$out"
expect "and it is labelled so it alerts once" "LABEL release-tracking-merged-old\|LABEL merged-old" "$out"
out="$(run "$(rec merged-new merged "$(ago 5 minutes)" "$(ago 5 minutes)" "" "$(ago 5 minutes)" "")")"
refute "a release merged 5 minutes ago is not stalled" "ReleaseStalled" "$out"
out="$(run "$(rec prog-quiet progressing "$(ago 3 hours)" "$(ago 3 hours)" "$(ago 90 minutes)" "$(ago 3 hours)" "")")"
expect "progressing with no fact for 90 min is stalled" "ReleaseStalled" "$out"
out="$(run "$(rec prog-ok progressing "$(ago 3 hours)" "$(ago 3 hours)" "$(ago 10 minutes)" "$(ago 3 hours)" "")")"
refute "progressing with a recent heartbeat is not stalled" "ReleaseStalled" "$out"
out="$(run "$(rec already merged "$(ago 2 hours)" "$(ago 2 hours)" "" "$(ago 2 hours)" merged)")"
refute "an already-alerted state does not alert again" "ReleaseStalled" "$out"
out="$(run "$(rec healthy-old healthy "$(ago 20 days)" "" "" "$(ago 21 days)" "")")"
expect "a healthy record past retention is deleted" "DELETE" "$out"
out="$(run "$(rec healthy-new healthy "$(ago 2 days)" "" "" "$(ago 3 days)" "")")"
refute "a recent healthy record is kept" "DELETE" "$out"
out="$(run "$(rec proposed-old proposed "" "" "" "$(ago 20 days)" "")")"
expect "a proposal past retention is deleted" "DELETE" "$out"
out="$(run "$(rec legacy "" "" "" "" "$(ago 30 days)" "")")"
expect "a state-less legacy record past retention is deleted" "DELETE" "$out"
out="$(run "$(rec merged-ancient merged "$(ago 30 days)" "$(ago 30 days)" "" "$(ago 30 days)" merged)")"
refute "a merged record is never swept while it may still be live" "DELETE" "$out"
out="$(run "$(rec x merged "$(ago 2 hours)" "$(ago 2 hours)" "" "$(ago 2 hours)" "")" 1)"
refute "no list permission is skipped, not fatal" "EVENT" "$out"

# ADR-0021 phase 4: the newest five records that reached healthy per app/env/cluster are the
# rollback-eligibility window and outlive retention; a sixth, older one does not.
healthy_rec() { # name healthyAt app
  jq -n --arg n "$1" --arg h "$2" --arg app "${3:-x}" --arg old "$(ago 30 days)" \
    '{metadata:{name:$n,creationTimestamp:$old,labels:{"hangar.io/app":$app,"hangar.io/env":"staging","hangar.io/cluster":"kind-prod"}},
      data:{state:"superseded",stateAt:$old,healthyAt:$h}}'
}
window=""
for i in 1 2 3 4 5 6; do window="${window}${window:+,}$(healthy_rec "h${i}" "$(ago $(( 20 + i )) days)")"; done
window="${window},$(healthy_rec other-app "$(ago 40 days)" y)"
out="$(run "${window}")"
for i in 1 2 3 4 5; do refute "healthy record h${i} is in the five-record window and is kept" "DELETE h${i}\$" "$out"; done
expect "the sixth-newest healthy record is past retention and deleted" "DELETE h6" "$out"
refute "another app's only healthy record is its own window, kept" "DELETE other-app" "$out"

[ $fail -eq 0 ] && echo ok || exit 1
