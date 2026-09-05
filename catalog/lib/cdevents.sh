#!/usr/bin/env bash
# catalog/lib/cdevents.sh
#
# Emits CDEvents (https://cdevents.dev) to the shared internal broker, chaining
# independently-triggered PipelineRuns (build -> test -> deploy -> release). Assembles
# both chainId (CDEvents' causal-sequence correlator) and platform.traceparent (the
# separate W3C trace context stitching one Tempo trace across the whole flow) - see
# docs/admin/chaining.md and docs/admin/tracing.md.
#
# Auth: the pipeline pod's own audience-bound projected ServiceAccount token, sent as a
# bearer token and verified by the broker via Kubernetes TokenReview - no
# platform-minted credential anywhere in this path.

set -euo pipefail

: "${CDEVENTS_BROKER_URL:?CDEVENTS_BROKER_URL must be set (in-cluster shared EventListener address)}"
: "${NAMESPACE:?NAMESPACE must be set (app namespace, injected via downward API)}"
: "${TEKTON_PIPELINE_RUN:?TEKTON_PIPELINE_RUN must be set (injected via downward API/params)}"

_BROKER_TOKEN_PATH="/var/run/secrets/platform/broker-token"

# cdevent_send <event-type> <subject-type> <subject-id> <subject-content-json>
#   event-type            e.g. dev.cdevents.artifact.published.0.3.0
#   subject-type           CDEvents subject type this event's context.type implies -
#                           e.g. "artifact" for artifact.published, "pipelineRun" for
#                           pipelinerun.*. CamelCase for multi-word names, lowercase for
#                           single-word (matches the CDEvents spec convention) - not
#                           derivable from event-type alone, so passed explicitly.
#   subject-id             the emitting PipelineRun's own name
#   subject-content-json    a JSON object merged into subject.content
#
# Idempotency: the CDEvents "id" is deterministically derived from PipelineRun name +
# event-type, not random - a retried step's at-least-once delivery produces the same id
# every time, so the next-stage Trigger (which names its PipelineRun from this id) sees
# a harmless no-op redelivery, not a duplicate run.

# Cosmetic word lists for chain_slug (see cdevent_send) - human-friendly stand-in for
# the raw chain-id UUID in PipelineRun names. Any bias from a non-power-of-two modulo is
# irrelevant here (visual variety, not security), so list lengths don't need to match or
# be powers of two. Keep entries short (<=6 chars) and lowercase-only (DNS-1123 names).
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

cdevent_send() {
  local event_type="$1" subject_type="$2" subject_id="$3" subject_content_json="$4"

  # PLATFORM_CONFIG_JSON is optional, unlike chain-id/traceparent/flow-start-time above.
  # Each stage's own domain-completion event sets it to cicd.yaml's already-validated
  # content, letting deploy/release skip their own clone+validate (see
  # resolve-notify-config.yaml). Other call sites (pipelinerun.started/finished) leave
  # it unset - nothing downstream of those reads it.
  local config_json="${PLATFORM_CONFIG_JSON:-}"
  [[ -z "${config_json}" ]] && config_json='{}'

  local sa_token
  sa_token="$(cat "${_BROKER_TOKEN_PATH}")"

  local chain_id="${PLATFORM_CHAIN_ID:?PLATFORM_CHAIN_ID must be set - propagated from the triggering event, or generated at flow start}"

  # Truncated to 8 hex chars (32 bits, collision odds irrelevant at this volume): a full
  # sha256sum pushed PipelineRun names like "test-<hash>" past Kubernetes' 63-char
  # resource name limit, so the run silently never got created. Shortened further from
  # the original 20 to make room for chain_slug (below) in the same name budget.
  local event_id
  event_id="$(printf '%s' "${TEKTON_PIPELINE_RUN}:${event_type}" | sha256sum | cut -d' ' -f1 | cut -c1-8)"

  # Human-friendly stand-in for chain-id in PipelineRun names (see flow-triggers.yaml's
  # pipeline-run-name) - deterministic from chain-id, so every PipelineRun in one flow
  # chain gets the same two words, letting them be visually grouped at a glance.
  local chain_slug
  chain_slug="$(chain_id_to_slug "${chain_id}")"

  local payload
  payload="$(jq -n \
    --arg id "${event_id}" \
    --arg type "${event_type}" \
    --arg source "/platform-cicd/${NAMESPACE}/${TEKTON_PIPELINE_RUN}" \
    --arg subjectType "${subject_type}" \
    --arg subjectId "${subject_id}" \
    --arg chainId "${chain_id}" \
    --arg chainSlug "${chain_slug}" \
    --arg traceparent "${PLATFORM_TRACEPARENT:?PLATFORM_TRACEPARENT must be set - see otel.sh}" \
    --arg flowStartTime "${PLATFORM_FLOW_START_TIME:?PLATFORM_FLOW_START_TIME must be set - propagated from the triggering event, or set at flow start (see otel_flow_root_begin in otel.sh)}" \
    --arg configJson "${config_json}" \
    --argjson content "${subject_content_json}" \
    '{
      context: {
        version: "0.4.1",
        id: $id,
        source: $source,
        type: $type,
        timestamp: (now | todate),
        chainId: $chainId
      },
      subject: {
        id: $subjectId,
        source: $source,
        type: $subjectType,
        content: $content
      },
      customData: {
        platform: { traceparent: $traceparent, flow_start_time: $flowStartTime, config_json: $configJson, chain_slug: $chainSlug }
      },
      customDataContentType: "application/json"
    }')"

  curl --fail --silent --show-error \
    --retry 3 --retry-connrefused --max-time 10 \
    -X POST "${CDEVENTS_BROKER_URL}" \
    -H "Authorization: Bearer ${sa_token}" \
    -H "Content-Type: application/cloudevents+json" \
    -d "${payload}"
}

# cdevents_map_outcome <tekton-status>
#   Maps a Tekton PipelineRun status reason (Succeeded/Completed/Failed/Cancelled/...)
#   to CDEvents' pipelineRun/taskRun "outcome" enum. Used by send-cdevent.yaml's
#   optional tekton-status param - see docs/admin/chaining.md.
cdevents_map_outcome() {
  case "$1" in
    Succeeded|Completed) echo "success" ;;
    Failed) echo "failure" ;;
    Cancelled) echo "cancel" ;;
    *) echo "error" ;;
  esac
}
