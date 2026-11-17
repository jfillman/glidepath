#!/usr/bin/env bash
# catalog/lib/retry.sh
#
# Generic retry wrapper for a single command against a flaky remote. Exists
# specifically for `git clone`/`git push` against github.com's smart-HTTP endpoint -
# curl's own --retry/--retry-connrefused/--max-time flags (see github-app.sh and every
# catalog/tasks/*.yaml curl call) can't reach those, since git - not curl - owns that
# connection. GitHub REST API and registry calls should keep using curl's native retry
# flags directly rather than wrapping curl in this too; this is only for commands with
# no retry mechanism of their own.

set -euo pipefail

# with_retry <max-attempts> -- <command> [args...]
# Retries <command> up to <max-attempts> times with exponential backoff (2s, 4s, 8s...),
# on any nonzero exit - git gives no structured way to tell a transient network error
# (e.g. a 503 mid-clone) from a permanent one (bad URL, auth failure, merge conflict), so
# a genuinely broken command still burns the full attempt budget before failing loudly.
# Deliberately generic (no git-specific logic) so it's reusable for any command in the
# same situation.
with_retry() {
  local max_attempts="$1"; shift
  [[ "${1:-}" == "--" ]] && shift

  # What a failure line names: a label the caller supplies (WITH_RETRY_LABEL) or the command word, NEVER its arguments. Arguments carry URLs
  # and revisions, and a log excerpt is posted into pull requests (docs/admin/security.md); a credential is never in an argument either, but
  # this does not depend on that.
  local label="${WITH_RETRY_LABEL:-${1##*/}}"
  local attempt=1 delay=2
  until "$@"; do
    if (( attempt >= max_attempts )); then
      echo "with_retry: '${label}' failed after ${attempt} attempts, giving up" >&2
      return 1
    fi
    echo "with_retry: '${label}' failed (attempt ${attempt}/${max_attempts}), retrying in ${delay}s..." >&2
    sleep "${delay}"
    delay=$(( delay * 2 ))
    (( attempt++ ))
  done
}
