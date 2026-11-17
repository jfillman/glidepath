# Frozen copy of origin/main's stage-preflight chain and trace block (as of the promotion-lineage change), the baseline
# tests/promotion_lineage_test.py holds every run WITHOUT origin annotations to. Do not edit it to follow the task.
if [[ -n "${INCOMING_CHAIN_ID}" ]]; then
  chain_id="${INCOMING_CHAIN_ID}"
  printf '%s' "${INCOMING_TRACEPARENT}" > "$(results.traceparent.path)"
  printf '%s' "${INCOMING_FLOW_START_TIME}" > "$(results.start-time.path)"
  printf '%s' "${chain_id}" > "$(results.chain-id.path)"
else
  read -r traceparent start_time <<< "$(otel_flow_root_begin)"
  printf '%s' "${traceparent}" > "$(results.traceparent.path)"
  printf '%s' "${start_time}" > "$(results.start-time.path)"
  # printf, not `uuidgen | tee` - uuidgen's trailing newline once landed in the
  # result verbatim and broke otel-cli's CSV-based --attrs parser downstream
  # (platform.stage silently dropped from every stage span, caught in Tempo).
  chain_id="$(uuidgen)"
  printf '%s' "${chain_id}" > "$(results.chain-id.path)"
fi
# A pure function of chain-id - always the same word pair a downstream
# CDEvent-triggered stage's own PipelineRun name derives for this chain.
printf '%s' "$(chain_id_to_slug "${chain_id}")" > "$(results.chain-slug.path)"
if [[ -n "${INCOMING_CHAIN_ID}" ]]; then
  echo "flow continued: chain ${chain_id} ($(cat "$(results.chain-slug.path)")), trace $(otel_traceparent_trace_id "$(cat "$(results.traceparent.path)")"), started $(cat "$(results.start-time.path)")"
else
  echo "flow started: chain ${chain_id} ($(cat "$(results.chain-slug.path)")), trace $(otel_traceparent_trace_id "$(cat "$(results.traceparent.path)")")"
fi

