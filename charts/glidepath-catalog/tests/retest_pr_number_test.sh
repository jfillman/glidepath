#!/usr/bin/env bash
# validate-values.yaml and policy-validation-gate.yaml on a /retest: the issue_comment payload has no pull_request,
# so PaC passes pr-number as the literal "{{ body.pull_request.number }}". The changed-file block must resolve the
# PR from the commit instead of calling /pulls/{{ ... }} (values-m6hl2, 2026-10-28). The block is extracted from the
# rendered Task and run with a fake api().
# Usage: charts/glidepath-catalog/tests/retest_pr_number_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT

expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}

for task in validate-values policy-validation-gate; do
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
    --show-only "templates/tasks/${task}.yaml" > "$d/${task}.yaml" || { echo "FAIL ${task}: render"; exit 1; }
  # From the first PR_NUMBER test through the changed-file if/else (the same span before and after the fallback).
  yq '.spec.steps[].script' "$d/${task}.yaml" \
    | awk '/^if \[\[ -n "\$\{PR_NUMBER\}" \]\]/{on=1} on{print} on && /^fi$/ && seen{exit} on && /^else$/{seen=1}' \
    > "$d/${task}.block.sh"
  [[ -s "$d/${task}.block.sh" ]] || { echo "FAIL ${task}: changed-file block not found"; fail=1; continue; }

  for pr in '{{ body.pull_request.number }}' 53 ''; do
    out="$(cd "$d" && owner=acme repo=gitops-x REVISION=abc123 PR_NUMBER="${pr}" bash -c '
      set -euo pipefail
      api() { echo "$1" >> calls; case "$1" in
        */commits/abc123/pulls) echo "[{\"number\":53}]" ;;
        */pulls/53) echo "{\"base\":{\"sha\":\"base1\"}}" ;;
        */compare/base1...abc123|*/commits/abc123) echo "{\"files\":[{\"filename\":\"a/values.yaml\",\"status\":\"modified\"}]}" ;;
        *) echo "unexpected call $1" >&2; exit 3 ;;
      esac; }
      : > calls
      source "'"$d/${task}.block.sh"'"
      echo "${PR_NUMBER}|${changed}|$(grep -c "{{" calls || true)"
    ' 2>&1)"
    case "${pr}" in
      53) want="53|a/values.yaml|0" ;;
      '') want="|a/values.yaml|0" ;;
      *)  want="53|a/values.yaml|0" ;;
    esac
    expect "${task}: pr-number '${pr}'" "${out}" "${want}"
  done
done

[[ "${fail}" == 0 ]] && echo "PASS retest_pr_number_test" || exit 1
