#!/usr/bin/env bash
# catalog/lib/otel.sh
#
# Shared span-instrumentation helpers built on otel-cli, baked into the toolbox image
# (catalog/toolbox/Dockerfile). Every catalog Task step sources this instead of calling
# otel-cli directly, so a version bump only needs a fix here.
#
# Stateless by design: every span is sent via one bare `otel-cli span`/`exec` call with
# explicit trace-id/span-id/parent-span-id/timestamps - never otel-cli's `span
# background` + `span end` daemon-socket workflow. That workflow can't span Tekton
# Tasks (each runs in its own Pod, and the background daemon's Unix socket is local to
# whichever Pod started it) - confirmed live via spans stuck at a hardcoded 1s
# `timeout` duration. See docs/admin/tracing.md.
#
# Fix: split "begin" (mint identifiers + start timestamp, thread through Tekton
# results/CDEvents like traceparent) from "send" (one stateless call once the real end
# time is known).
#
# Contract:
#   - traceparent is threaded through Tekton params/results, never rediscovered locally.
#   - One flow-root span covers an entire build->test->deploy->release run; every
#     stage's span parents directly to it (flat, not nested - stages don't overlap).
#   - The flow-root traceparent rides inside the CDEvent payload across independently
#     triggered PipelineRuns - see cdevents.sh.

set -euo pipefail

otel_now() {
  date -u +"%Y-%m-%dT%H:%M:%S.%NZ"
}

otel_traceparent_trace_id() {
  cut -d- -f2 <<< "$1"
}

otel_traceparent_span_id() {
  cut -d- -f3 <<< "$1"
}

# otel_flow_root_begin
# Mints identifiers + a start timestamp for a NEW flow-root span, without sending
# anything yet - see the file header. Prints "<traceparent> <start-time>". Called
# exactly once, by build (the only stage that's always a fresh flow start).
otel_flow_root_begin() {
  local trace_id span_id
  trace_id="$(openssl rand -hex 16)"
  span_id="$(openssl rand -hex 8)"
  printf '00-%s-%s-01 %s\n' "${trace_id}" "${span_id}" "$(otel_now)"
}

# otel_stage_span_begin <flow-traceparent>
# Mints a span id + start timestamp for one stage's own span - a sibling of the
# flow-root, reusing the flow's trace id so it lands in the same trace. Prints
# "<span-id> <start-time>".
otel_stage_span_begin() {
  local flow_traceparent="$1"
  # Fail fast here if the incoming traceparent is malformed, rather than only at
  # send time (in a later, possibly much-later, Task).
  otel_traceparent_trace_id "${flow_traceparent}" >/dev/null
  printf '%s %s\n' "$(openssl rand -hex 8)" "$(otel_now)"
}

# _otel_cli_send <args...>
# Retry wrapper around a one-shot `otel-cli span` call. Explicit ids/timestamps make a
# retry of the same call safe (no duplicate side effect). A bare `otel-cli span` call
# has no retry and, without --fail, exits 0 even when it can't reach the collector -
# silently dropping the span. --fail surfaces that as a real exit code; --timeout 5s
# gives more room than otel-cli's default under load (seen failing during a burst of
# concurrent PipelineRuns); 3 attempts with backoff covers a blip without masking a
# genuinely down collector.
_otel_cli_send() {
  local attempt
  for attempt in 1 2 3; do
    if otel-cli "$@" --fail --timeout 5s; then
      return 0
    fi
    [[ "${attempt}" -lt 3 ]] && sleep 2
  done
  echo "otel-cli span send failed after 3 attempts (collector unreachable or rejecting spans) - continuing, not failing the pipeline over lost trace data" >&2
  return 0
}

# otel_span_send <name> <flow-traceparent> <span-id> <start-time> <end-time> <tekton-status> [attrs]
# The only function here that actually talks to the collector for flow-root/stage
# spans - one stateless otel-cli call, safe from any Task/Pod. tekton-status is
# Tekton's own aggregate status string ($(tasks.status) in a Pipeline's finally
# block); translated to an OTel status code here so callers never need their own
# translation step.
#
# <span-id> empty means "this call ends the flow-root span itself": reuse the span
# id already embedded in <flow-traceparent>, send with no parent. Non-empty means
# "this is one stage's own span" (minted by otel_stage_span_begin):
# <flow-traceparent>'s span id becomes this span's *parent* instead.
otel_span_send() {
  : "${OTEL_EXPORTER_OTLP_ENDPOINT:?OTEL_EXPORTER_OTLP_ENDPOINT must be set (in-cluster OTel Collector)}"
  local name="$1" flow_traceparent="$2" span_id="$3" start_time="$4" end_time="$5" tekton_status="$6"
  local attrs="${7:-}"
  local trace_id flow_root_span_id parent_span_id
  trace_id="$(otel_traceparent_trace_id "${flow_traceparent}")"
  flow_root_span_id="$(otel_traceparent_span_id "${flow_traceparent}")"
  if [[ -z "${span_id}" ]]; then
    span_id="${flow_root_span_id}"
    parent_span_id=""
  else
    parent_span_id="${flow_root_span_id}"
  fi
  local status_code="ok"
  [[ "${tekton_status}" != "Succeeded" && "${tekton_status}" != "Completed" ]] && status_code="error"
  local -a args=(
    span --service "glidepath" --name "${name}"
    --force-trace-id "${trace_id}" --force-span-id "${span_id}"
    --start "${start_time}" --end "${end_time}"
    --status-code "${status_code}"
  )
  [[ -n "${parent_span_id}" ]] && args+=(--force-parent-span-id "${parent_span_id}")
  [[ -n "${attrs}" ]] && args+=(--attrs "${attrs}")
  _otel_cli_send "${args[@]}"
}

# otel_task_span_send <name> <flow-traceparent> <parent-span-id> <start-time> <end-time> <tekton-status> [attrs]
# Sends a span for one Task's own work, nested under an explicit <parent-span-id>.
# Distinct from otel_span_send, which reuses its <span-id> arg as the new span's own id
# (correct only for a stage's own span). Calling otel_span_send from a Task instead
# would mint a span sharing the stage's own (trace-id, span-id) - Tempo can only keep
# one of two spans with the same id pair, which is why some Task spans silently never
# appeared. This function always mints a fresh span id, so nesting is correct.
otel_task_span_send() {
  : "${OTEL_EXPORTER_OTLP_ENDPOINT:?OTEL_EXPORTER_OTLP_ENDPOINT must be set (in-cluster OTel Collector)}"
  local name="$1" flow_traceparent="$2" parent_span_id="$3" start_time="$4" end_time="$5" tekton_status="$6"
  local attrs="${7:-}"
  local trace_id span_id
  trace_id="$(otel_traceparent_trace_id "${flow_traceparent}")"
  span_id="$(openssl rand -hex 8)"
  local status_code="ok"
  [[ "${tekton_status}" != "Succeeded" && "${tekton_status}" != "Completed" ]] && status_code="error"
  local -a args=(
    span --service "glidepath" --name "${name}"
    --force-trace-id "${trace_id}" --force-span-id "${span_id}"
    --force-parent-span-id "${parent_span_id}"
    --start "${start_time}" --end "${end_time}"
    --status-code "${status_code}"
  )
  [[ -n "${attrs}" ]] && args+=(--attrs "${attrs}")
  _otel_cli_send "${args[@]}"
}

# otel_log_send <body> <severity> <attrs-json-array>
# Sends one stateless OTLP log record via a raw HTTP POST to the same collector every
# span already targets (OTEL_EXPORTER_OTLP_ENDPOINT). otel-cli has no logs subcommand
# (traces only - see https://github.com/equinix-labs/otel-cli), so this speaks the
# collector's OTLP/HTTP JSON receiver directly instead of going through it.
# <attrs-json-array> is a JSON array of {key, value:{stringValue: ...}} objects - build
# it with jq at the call site, never string concatenation, so a value containing a quote
# or newline (a PR title, an error message) can't break the payload. Same best-effort
# philosophy as _otel_cli_send: retries a transient blip, but never fails the caller's
# pipeline over an observability side-channel.
otel_log_send() {
  : "${OTEL_EXPORTER_OTLP_ENDPOINT:?OTEL_EXPORTER_OTLP_ENDPOINT must be set (in-cluster OTel Collector)}"
  local body="$1" severity="$2" attrs_json="$3"
  local now_ns
  now_ns="$(date -u +%s%N)"
  local payload
  payload="$(jq -c -n \
    --arg body "${body}" \
    --arg severity "${severity}" \
    --arg time "${now_ns}" \
    --argjson attrs "${attrs_json}" \
    '{
      resourceLogs: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "glidepath" } }] },
        scopeLogs: [{
          logRecords: [{
            timeUnixNano: $time,
            severityText: $severity,
            body: { stringValue: $body },
            attributes: $attrs
          }]
        }]
      }]
    }')"

  local attempt
  for attempt in 1 2 3; do
    if curl --fail --silent --show-error --max-time 5 \
      -X POST "${OTEL_EXPORTER_OTLP_ENDPOINT}/v1/logs" \
      -H "Content-Type: application/json" \
      -d "${payload}" >/dev/null; then
      return 0
    fi
    [[ "${attempt}" -lt 3 ]] && sleep 2
  done
  echo "otel_log_send failed after 3 attempts (collector unreachable or rejecting logs) - continuing, not failing the pipeline over lost log data" >&2
  return 0
}

# otel_child_span <name> <flow-traceparent> <parent-span-id> <attrs> -- <command...>
# Runs <command...> wrapped in one stateless child span via `otel-cli exec` - parenting
# is explicit (--force-*-id), not inherited daemon/process state, so this works from
# any Task regardless of which Pod owns the parent span. Used by governance-gate-stub
# to attach a queryable child span (governance.stub=true as a span *attribute*, not an
# event - a span is queryable via TraceQL's `name =`, an event on an unreachable
# background span isn't).
otel_child_span() {
  : "${OTEL_EXPORTER_OTLP_ENDPOINT:?OTEL_EXPORTER_OTLP_ENDPOINT must be set (in-cluster OTel Collector)}"
  local name="$1" flow_traceparent="$2" parent_span_id="$3" attrs="$4"; shift 4
  local trace_id
  trace_id="$(otel_traceparent_trace_id "${flow_traceparent}")"
  otel-cli exec \
    --name "${name}" \
    --service "glidepath" \
    --force-trace-id "${trace_id}" \
    --force-parent-span-id "${parent_span_id}" \
    --attrs "${attrs}" \
    "$@"
}
