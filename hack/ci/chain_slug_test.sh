#!/usr/bin/env bash
# Checks catalog/lib/chain-slug.sh's word lists and pins its mapping.
#
# The lists are the mapping (index = position), and Tower carries a mirror
# (tower src/tekton/chainSlug.ts) that must agree. The golden vectors below are
# repeated in Tower's chainSlug.test.ts: change one side, change both.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../../catalog/lib/chain-slug.sh
source "${here}/../../catalog/lib/chain-slug.sh"

fail=0
check_list() {
  local name="$1"; shift
  local words=("$@")
  if [[ ${#words[@]} -ne 256 ]]; then
    echo "FAIL ${name}: ${#words[@]} words, want 256"; fail=1
  fi
  local dups
  dups="$(printf '%s\n' "${words[@]}" | sort | uniq -d)"
  if [[ -n "${dups}" ]]; then
    echo "FAIL ${name}: duplicates: ${dups//$'\n'/ }"; fail=1
  fi
  local w
  for w in "${words[@]}"; do
    if [[ ! "${w}" =~ ^[a-z]{2,6}$ ]]; then
      echo "FAIL ${name}: '${w}' is not 2-6 lowercase letters"; fail=1
    fi
  done
}
check_list adjectives "${_CHAIN_SLUG_ADJECTIVES[@]}"
check_list nouns "${_CHAIN_SLUG_NOUNS[@]}"

both="$(comm -12 <(printf '%s\n' "${_CHAIN_SLUG_ADJECTIVES[@]}" | sort) <(printf '%s\n' "${_CHAIN_SLUG_NOUNS[@]}" | sort))"
if [[ -n "${both}" ]]; then
  echo "FAIL: in both lists: ${both//$'\n'/ }"; fail=1
fi

# chain-id -> slug. Keep in step with tower src/tekton/chainSlug.test.ts.
while read -r id want; do
  got="$(chain_id_to_slug "${id}")"
  if [[ "${got}" != "${want}" ]]; then
    echo "FAIL: ${id} -> ${got}, want ${want}"; fail=1
  fi
done <<'EOF'
00000000-0000-4000-8000-000000000000 able-heron
ffff0000-0000-4000-8000-000000000000 cheeky-smelt
c429209a-1111-4111-8111-111111111111 still-pipit
0d563bd5-2222-4222-8222-222222222222 balmy-moose
7f80abcd-3333-4333-8333-333333333333 lucky-jackal
EOF

if [[ ${fail} -ne 0 ]]; then exit 1; fi
echo "OK: chain-slug word lists (256 x 256) and mapping"
