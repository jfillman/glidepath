#!/usr/bin/env bash
# open-release-pr's close_stale_proposals: when a new release PR opens, an earlier "proposed" record of the same app,
# environment and cluster whose PR GitHub reports closed and unmerged becomes "closed" (2026-10-30: five superseded
# releases sat in "proposed" and Tower's Ops Wall counted them as awaiting approval). Merged, open, unreadable PRs,
# another repository's PR and the release's own record are left alone. The function is extracted from the rendered
# Task and run against a fake kubectl and curl.
# Usage: charts/glidepath-catalog/tests/close_stale_proposals_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/open-release-pr.yaml > "$d/task.yaml" || { echo "FAIL render"; exit 1; }
yq '.spec.steps[].script' "$d/task.yaml" | awk '/^close_stale_proposals\(\) \{/{on=1} on{print} on && /^\}$/{exit}' > "$d/fn.sh"
[[ -s "$d/fn.sh" ]] || { echo "FAIL close_stale_proposals not found in the rendered Task"; exit 1; }

out="$(cd "$d" && bash -c '
  set -euo pipefail
  NAMESPACE=app-shop-cicd APP_NAME=shop ENV=prod CLUSTER=hangar-prod CHAIN_ID=new owner=o gitops_repo=gitops-shop token=t
  # Records (all "proposed": the label and state selection is kubectl'"'"'s, checked by the call log) and their prUrl.
  names="release-tracking-new release-tracking-closed release-tracking-merged release-tracking-open release-tracking-gone release-tracking-foreign"
  url_of() {
    case "$1" in
      release-tracking-new) echo -n https://github.com/o/gitops-shop/pull/9 ;;
      release-tracking-closed) echo -n https://github.com/o/gitops-shop/pull/1 ;;
      release-tracking-merged) echo -n https://github.com/o/gitops-shop/pull/2 ;;
      release-tracking-open) echo -n https://github.com/o/gitops-shop/pull/3 ;;
      release-tracking-gone) echo -n https://github.com/o/gitops-shop/pull/4 ;;
      release-tracking-foreign) echo -n https://github.com/x/gitops-other/pull/5 ;;
    esac
  }
  kubectl() {
    echo "kubectl $*" >> calls
    case "$1 $2" in
      "get configmap")
        if [[ "$*" == *" -l "* ]]; then printf "%s\n" ${names}; else url_of "$3"; fi ;;
      "patch configmap") echo "patched $3" >> patched ;;
    esac
  }
  curl() {
    local u="${@: -1}"; echo "curl $u" >> calls
    case "$u" in
      */pulls/1) echo "{\"state\":\"closed\",\"merged\":false}" ;;
      */pulls/2) echo "{\"state\":\"closed\",\"merged\":true}" ;;
      */pulls/3) echo "{\"state\":\"open\",\"merged\":false}" ;;
      *) return 22 ;;
    esac
  }
  : > calls; : > patched
  source fn.sh
  close_stale_proposals
  echo "rc=$?"
  echo "patched: $(sort patched | tr "\n" " ")"
  grep -c "hangar.io/app=shop,hangar.io/env=prod,hangar.io/cluster=hangar-prod" calls | sed "s/^/selector-calls=/"
  grep -q "pulls/9" calls && echo "asked about its own PR" || true
  grep -q "gitops-other" calls && echo "asked another repository" || true
' 2>&1)"

expect() { # <name> <substring>
  if [[ "${out}" == *"$2"* ]]; then echo "ok   $1"; else echo "FAIL $1: no '$2' in:"; echo "${out}" | sed 's/^/    /'; fail=1; fi
}
expect "closes only the closed-unmerged one" "patched: patched release-tracking-closed "
expect "says so" "release-tracking-closed: proposed -> closed (#1 was closed unmerged)"
expect "never fails the release" "rc=0"
expect "selects this app, env and cluster" "selector-calls=1"
[[ "${out}" == *"asked about its own PR"* ]] && { echo "FAIL looked up its own record's PR"; fail=1; }
[[ "${out}" == *"asked another repository"* ]] && { echo "FAIL followed a PR URL outside gitops-<app>"; fail=1; }

[[ "${fail}" == 0 ]] && echo "PASS close_stale_proposals_test" || exit 1
