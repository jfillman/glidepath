#!/usr/bin/env bash
# run-testworkflow.yaml's report_failure: a failed run names its failed test cases from the JUnit report, or, without
# one, prints the last output of the first step that did not pass (boarding-api's dev test stage, 2026-10-29: the log
# said only "integration: failed" and "failed=1"). The function is extracted from the rendered Task and run against a
# real node --test report (fixtures/junit-node-failure.xml), a nested JUnit 4 style report, and a fake Testkube log.
# Usage: charts/glidepath-catalog/tests/test_failure_report_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT

expect() { # <name> <output> <substring>
  if [[ "$2" == *"$3"* ]]; then echo "ok   $1"; else echo "FAIL $1: no '$3' in:"; echo "$2" | sed 's/^/    /'; fail=1; fi
}

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/run-testworkflow.yaml > "$d/task.yaml" || { echo "FAIL render"; exit 1; }
yq '.spec.steps[].script' "$d/task.yaml" | awk '/^report_failure\(\) \{/{on=1} on{print} on && /^\}$/{exit}' > "$d/fn.sh"
[[ -s "$d/fn.sh" ]] || { echo "FAIL report_failure not found in the rendered Task"; exit 1; }

RESULT_JSON='{"signature":[{"ref":"rwait","name":"wait-for-target"},{"ref":"rint","name":"integration"},{"ref":"rrep","name":"report"}],
  "result":{"status":"failed","steps":{"rwait":{"status":"passed"},"rint":{"status":"failed"},"rrep":{"status":"passed"}}}}'
{
  printf '2026-10-30T01:08:00.05Z Configuring shell... done\n\n\001\005\006rwait\003start\003\n'
  printf '2026-10-30T01:08:01.47Z \033[90mhealthz answered after 0 retries\033[0m\n\n\001\005\006rint\003start\003\n'
  printf '2026-10-30T01:08:02.88Z connecting to http://x:8080\n2026-10-30T01:08:03.04Z Error: connect ECONNREFUSED 10.0.0.9:8080\n'
  printf '\n\001\005\006rrep\003start\003\n2026-10-30T01:08:03.11Z Root: /\n'
} > "$d/logs"

run() { # <junit file or ""> -> report_failure's output
  rm -f /tmp/junit.xml.test-failure-report
  (cd "$d" && RESULT_JSON="${RESULT_JSON}" REPORT_PATH=data/artifacts/junit.xml EXECUTION_ID=e1 API=http://api JUNIT="$1" LOGS="$d/logs" bash -c '
    set -euo pipefail
    curl() { cat "${LOGS}"; }
    src="$(sed "s#/tmp/junit.xml#${JUNIT:-/nonexistent}#g" fn.sh)"
    eval "${src}"
    report_failure
  ' 2>&1)
}

out="$(run "$PWD/charts/glidepath-catalog/tests/fixtures/junit-node-failure.xml")"
expect "node report: count" "${out}" "1 failed test(s) in data/artifacts/junit.xml:"
expect "node report: name" "${out}" "✖ a scan returns the boarded count against the flight capacity [test]"
expect "node report: message" "${out}" "assert.ok(body.capacity >= body.boarded)"
expect "node report: location" "${out}" "(/app/test/integration/boarding.test.js:76:10)"
[[ "${out}" == *"node:internal"* ]] && { echo "FAIL node report: printed a node:internal frame"; fail=1; }

cat > "$d/nested.xml" <<'EOF'
<testsuites><testsuite name="a"><testcase name="x" classname="com.x.FooTest"><failure message="boom">java.lang.AssertionError: boom
	at com.x.FooTest.bar(FooTest.java:42)</failure></testcase><testcase name="y"/></testsuite>
<testsuite name="b"><testcase name="z"><error>plain error</error></testcase></testsuite></testsuites>
EOF
out="$(run "$d/nested.xml")"
expect "nested report: count" "${out}" "2 failed test(s)"
expect "nested report: failure" "${out}" "✖ x [com.x.FooTest]"
expect "nested report: location" "${out}" "at com.x.FooTest.bar(FooTest.java:42)"
expect "nested report: error with no message" "${out}" "plain error"

cat > "$d/passing.xml" <<'EOF'
<testsuites><testcase name="ok"/></testsuites>
EOF
for junit in "" "$d/passing.xml"; do
  out="$(run "${junit}")"
  expect "step log (report '${junit##*/}'): step" "${out}" "step 'integration' did not pass"
  expect "step log (report '${junit##*/}'): output" "${out}" "  | Error: connect ECONNREFUSED 10.0.0.9:8080"
  [[ "${out}" == *"Root: /"* || "${out}" == *"healthz"* ]] && { echo "FAIL step log: printed another step's output"; fail=1; }
  [[ "${out}" == *"2026-10-30T"* ]] && { echo "FAIL step log: timestamps not stripped"; fail=1; }
done

[[ "${fail}" == 0 ]] && echo "PASS test_failure_report_test" || exit 1
