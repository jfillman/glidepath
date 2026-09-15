#!/usr/bin/env bash
# catalog/lib/chain-slug.sh
#
# chain_id_to_slug alone, split out of cdevents.sh (2026-09-16) so a Task that only
# wants the human-friendly slug - not to actually emit a CDEvent - doesn't have to
# source cdevents.sh and satisfy its hard CDEVENTS_BROKER_URL/NAMESPACE/
# TEKTON_PIPELINE_RUN requirements (`: "${VAR:?...}"`, under `set -euo pipefail`) just
# to call one pure function. cdevents.sh now sources this file itself, so cdevent_send's
# own chain_slug computation is unchanged - one definition, not two copies drifting
# apart.

set -euo pipefail

# Cosmetic word lists for chain_slug (see chain_id_to_slug below) - human-friendly
# stand-in for the raw chain-id UUID in PipelineRun names. Any bias from a
# non-power-of-two modulo is irrelevant here (visual variety, not security), so list
# lengths don't need to match or be powers of two. Keep entries short (<=6 chars) and
# lowercase-only (DNS-1123 names).
_CHAIN_SLUG_ADJECTIVES=(swift brave calm quiet bold keen glad warm cool sharp quick deep
  light soft firm wise kind eager plain still vivid fresh gentle mighty nimble ready
  steady sunny lucky merry jolly spry tidy brisk hardy lively dapper jaunty plucky)
_CHAIN_SLUG_NOUNS=(otter fox wolf hawk lynx bear seal crow heron finch robin swan crane
  moose bison viper cobra shark whale eagle raven stork ibis egret puma civet mole vole
  newt toad gecko heron egret quail grebe plover osprey falcon kite jay)

# chain_id_to_slug <chain-id-uuid>
#   Deterministic two-word slug from a chain-id's first 4 hex chars (the UUID's
#   time_low field - no RFC4122 version/variant bits fixed there, so no encoding bias
#   worth avoiding). Same chain-id always yields the same slug, so every PipelineRun in
#   one flow chain shows the same words, letting them be visually grouped without
#   needing the full chain-id.
chain_id_to_slug() {
  local chain_id="$1"
  local adj_idx noun_idx
  adj_idx=$(( 16#${chain_id:0:2} % ${#_CHAIN_SLUG_ADJECTIVES[@]} ))
  noun_idx=$(( 16#${chain_id:2:2} % ${#_CHAIN_SLUG_NOUNS[@]} ))
  printf '%s-%s' "${_CHAIN_SLUG_ADJECTIVES[$adj_idx]}" "${_CHAIN_SLUG_NOUNS[$noun_idx]}"
}
