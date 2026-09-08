# Practical Debugging Guide: OTEL & CDEvents in Action

This guide shows how to use Tempo, Grafana, and the broker logs to troubleshoot your CI/CD platform.

---

## Part 1: Querying Traces in Tempo

### Find a Specific Flow

If you have a git commit SHA and need to see the entire flow:

```bash
# Get the flow's traceparent from the build PipelineRun:
kubectl -n app-myapp-cicd get pipelinerun build-abc123def456 -o jsonpath='{.spec.params[?(@.name=="flow-traceparent")].value}'
# Output: 00-7f2e4a8c1d6b5f3e2c9a4d8f1b6e3a7c-4c9e2a7d1f3b8e5c-01
```

**In Tempo UI (http://localhost:3000/explore):**

**Query 1: All spans in the flow**
```
Trace ID: 7f2e4a8c1d6b5f3e2c9a4d8f1b6e3a7c
```
Returns: All build/test/deploy/release stages + tasks as one unified view

**Query 2: Find by chain_id attribute**
```
TraceQL: {resource.attributes.platform.chain_id="c4b8e2f5-7d1a-4c9f-b3e6-2a8d5f1c9e4b"}
```
Finds all spans tagged with this flow's chain ID

**Query 3: Find failed stages**
```
TraceQL: {status.code="ERROR"}
```
Returns: Any spans that failed (build, test, deploy, or individual tasks)

**Query 4: Find slow stages**
```
TraceQL: {span.duration > 60s && name=~"stage:.*"}
```
Returns: Stages that took longer than 60 seconds

### Common Tempo Queries

```
# All platform-cicd spans
{resource.attributes.service.name="platform-cicd"}

# Platform spans in a specific namespace
{resource.attributes.k8s.namespace.name="app-demo-cicd"}

# All stage spans
{name=~"stage:.*"}

# All task spans
{name=~"build-image|clone-repo|run-tests|deploy|.*task.*"}

# Find spans by PipelineRun name (if captured as attribute)
{attributes.tekton.pipelinerun="build-abc123def456"}

# Spans with errors
{status.code="ERROR"} && {span.duration > 30s}

# Find chains by app name
{attributes.app="nodejs-demo-app"}
```

---

## Part 2: Debugging Failed Flows

### Scenario 1: Deploy Failed - Find the Root Cause

```bash
# Step 1: Check what failed
kubectl -n app-demo-cicd get pipelinerun deploy-b3e9d7a2c8f1e5b4 -o yaml

# Look for status.conditions with reason: "Failed"
# Step 2: Get the traceparent
traceparent=$(kubectl -n app-demo-cicd get pipelinerun deploy-b3e9d7a2c8f1e5b4 \
  -o jsonpath='{.spec.params[?(@.name=="flow-traceparent")].value}')

# Step 3: Extract trace ID
trace_id=$(echo $traceparent | cut -d- -f2)
echo "Trace ID: $trace_id"
```

**In Tempo:**
```
Query: {traceId="$trace_id"}
Look for: Red spans (status=ERROR), longest duration task, error logs
```

### Scenario 2: Test Passed but No Deploy Triggered

Check if the `testcaserun.finished` CDEvent was actually sent:

```bash
# Check test PipelineRun's finally block output
kubectl -n app-demo-cicd get pipelinerun test-a2f8c3e1d5b9e7a2 \
  -o jsonpath='{.status.taskRuns}'

# Look for send-cdevent task status
# If it's missing or failed, the event never reached the broker
```

Check broker logs:

```bash
# Broker EventListener logs
kubectl -n platform-system logs -l app=el-cdevents-broker

# Look for:
# - "202 Accepted" messages (events received)
# - Trigger matching/not matching
# - Reason why deploy wasn't triggered
```

**Common issues:**
- Wrong chainId in event (broker doesn't recognize flow)
- Missing traceparent in customData
- Trigger selector doesn't match event type

### Scenario 3: OTEL Spans Not Appearing in Tempo

Check if the otel-collector is receiving spans:

```bash
# OTEL Collector logs
kubectl -n observability logs -l app.kubernetes.io/name=opentelemetry-collector

# Look for:
# - "Received span" messages (data arriving)
# - "Dropped" messages (queue overflow)
# - "Error" messages (processing failures)
```

Common issues in otel.sh:
```bash
# 1. OTEL_EXPORTER_OTLP_ENDPOINT not set
#    → Task step won't emit spans
#    → Check emit-image-ref-and-span, etc. for env vars

# 2. Malformed traceparent
#    → otel_stage_span_begin() validation fails
#    → Look for "Traceparent must be set" error

# 3. Clock skew
#    → Pod's system clock behind actual time
#    → Spans show negative duration or way in past
#    → Fix: `kubectl patch nodes --patch '{"spec":{"time":...}}'`

# 4. Network unreachable
#    → OTEL_EXPORTER_OTLP_ENDPOINT is wrong
#    → curl inside task container to test:
kubectl exec -it <pod> -- curl -v http://otel-collector-opentelemetry-collector.observability.svc.cluster.local:4318
```

---

## Part 3: Inspecting CDEvents

### View All Events Sent

```bash
# Broker EventListener webhook logs
kubectl -n platform-system logs -l app=el-cdevents-broker -f

# You'll see:
# Incoming HTTP POST
# Trigger match/no-match
# TriggerTemplate resolution
# PipelineRun creation
```

### Manually Send a Test CDEvent

```bash
# Get a ServiceAccount token for auth
token=$(kubectl -n app-demo-cicd create token platform-cicd-sa --duration=1h)

# Construct a CDEvent
cat > /tmp/test-event.json <<'EOF'
{
  "context": {
    "version": "0.4.1",
    "id": "test-event-12345",
    "source": "/platform-cicd/app-demo-cicd/test-pipelinerun",
    "type": "dev.cdevents.testcaserun.finished.0.3.0",
    "timestamp": "2025-08-06T15:00:00Z",
    "chainId": "test-chain-id-here"
  },
  "subject": {
    "id": "test-pipelinerun",
    "type": "testCaseRun",
    "content": {
      "outcome": "success",
      "testName": "manual-test"
    }
  },
  "customData": {
    "platform": {
      "traceparent": "00-7f2e4a8c1d6b5f3e2c9a4d8f1b6e3a7c-4c9e2a7d1f3b8e5c-01",
      "flow_start_time": "2025-08-06T14:32:00.000000000Z"
    }
  }
}
EOF

# Send it
curl -X POST http://localhost:8080/cdevents \
  -H "Authorization: Bearer $token" \
  -H "Content-Type: application/cloudevents+json" \
  -d @/tmp/test-event.json -v

# Watch broker logs to see it processed
kubectl -n platform-system logs -l app=el-cdevents-broker -f
```

### Extract CDEvent Data from PipelineRun Results

```bash
# Get the test PipelineRun that received the CDEvent
pipelinerun_name="test-a2f8c3e1d5b9e7a2"

# Reverse-engineer what CDEvent triggered it:
# Name follows: test-$(body.context.id)
# So event ID = "a2f8c3e1d5b9e7a2"

# Find the corresponding build PipelineRun that sent it:
build_pr=$(kubectl -n app-demo-cicd get pipelinerun -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep build)

# Get its send-cdevent task output:
kubectl -n app-demo-cicd logs $(kubectl -n app-demo-cicd get pod -l tekton.dev/pipelineRun=$build_pr --sort-by=.metadata.creationTimestamp | tail -1 | awk '{print $1}') -c send | tail -100
```

---

## Part 4: Inspecting Task & Span Code Paths

### Trace How a Specific File's Spans Are Emitted

Example: How does `build-image.yaml` emit its span?

```bash
# File: charts/glidepath-catalog/templates/tasks/build-image.yaml
# Look at the emit-image-ref-and-span step (the one with bash)

# Key env vars passed in:
# - FLOW_TRACEPARENT ($(params.flow-traceparent))
# - STAGE_SPAN_ID ($(params.stage-span-id))
# - OTEL_EXPORTER_OTLP_ENDPOINT

# The step runs:
source "${PLATFORM_LIB}/otel.sh"
otel_task_span_send "build-image" "${FLOW_TRACEPARENT}" "${STAGE_SPAN_ID}" \
  "${SPAN_START}" "$(date -u +"%Y-%m-%dT%H:%M:%S.%NZ")" "Succeeded"

# Which calls otel_task_span_send() from otel.sh:
# 1. Extract trace_id from FLOW_TRACEPARENT
# 2. Generate fresh span_id (openssl rand -hex 8)
# 3. Use STAGE_SPAN_ID as parent
# 4. Call otel-cli with explicit --force-* flags
```

### Verify otel.sh Functions Work

```bash
# Manually test in a pod:
kubectl run -it --image=ghcr.io/jfillman/platform-cicd-toolbox:latest debug-otel-cli -- bash

# Inside pod:
source /opt/platform/lib/otel.sh

# Test otel_now
otel_now
# Output: 2025-08-06T15:23:45.123456789Z

# Test traceparent parsing
traceparent="00-7f2e4a8c1d6b5f3e2c9a4d8f1b6e3a7c-4c9e2a7d1f3b8e5c-01"
otel_traceparent_trace_id "$traceparent"
# Output: 7f2e4a8c1d6b5f3e2c9a4d8f1b6e3a7c

otel_traceparent_span_id "$traceparent"
# Output: 4c9e2a7d1f3b8e5c

# Test flow-root generation
otel_flow_root_begin
# Output: 00-<random>-<random>-01 <timestamp>

# Test with real collector (if running)
export OTEL_EXPORTER_OTLP_ENDPOINT="http://otel-collector-opentelemetry-collector.observability.svc.cluster.local:4318"
otel_task_span_send "test-span" "$traceparent" "$STAGE_SPAN_ID" \
  "$(otel_now)" "$(otel_now)" "Succeeded"
# If successful: no output
# If failed: "OTEL_EXPORTER_OTLP_ENDPOINT must be set" or curl error
```

---

## Part 5: Performance Analysis

### Find Bottlenecks in Your Pipeline

```bash
# Query: Build stage duration breakdown
# Tempo Query:
{name=~"build-image|build-source|build|sast-scan|image-scan|generate-sbom"} && {resource.attributes.platform.stage="build"}

# Expected structure for build stage:
# [build] 14:32:05 → 14:32:50 (45s total)
#   ├─ [build-image] 14:32:06 → 14:32:45 (39s) ← Kaniko is expensive
#   ├─ [sast-scan] 14:32:05 → 14:32:25 (20s, concurrent)
#   └─ [unit-test] 14:32:05 → 14:32:20 (15s, concurrent)

# Action items:
# - If build-image > 30s: optimize Dockerfile, cache layers
# - If sast-scan > 15s: reduce rule complexity, exclude paths
# - If unit-test > 10s: parallelize tests, reduce fixture data
```

### Compare Stage Durations Across Runs

```bash
# Query multiple flows by namespace + stage
Tempo Query: {resource.attributes.k8s.namespace.name="app-demo-cicd"} && {name="stage:deploy"}

# This returns all deploy stages for this app
# Visualize as: bar chart of durations

# If one deploy is 5x slower than others:
# - Check for pod resource constraints
# - Check for quota limits
# - Check if rollout is waiting for pod ready
```

### Track End-to-End Flow Duration

```bash
# Query: Root span duration
{name="build" && parent_span_id=null}

# Shows total flow time from build start to final stage completion
# Phase 1: build → test → deploy (typical: 2-5 minutes)
# Phase 2: build → test → deploy → release (typical: 5-15 minutes, includes approvals)

# Alert if exceeded:
# - 10 minutes for build-only flow (check for blocked stages)
# - 30 minutes for release flow (check approval gates)
```

---

## Part 6: Common Issues & Solutions

### Issue: "traceparent must be set" Error

```bash
# Symptom: Task step logs show:
# otel.sh: line 45: OTEL_EXPORTER_OTLP_ENDPOINT must be set (in-cluster OTel Collector)

# Root cause: Missing env var in step

# Fix: Check your task's step definition:
spec:
  steps:
    - name: emit-image-ref-and-span
      env:
        - name: OTEL_EXPORTER_OTLP_ENDPOINT
          value: http://otel-collector-opentelemetry-collector.observability.svc.cluster.local:4318
        - name: FLOW_TRACEPARENT
          value: $(params.flow-traceparent)
        - name: STAGE_SPAN_ID
          value: $(params.stage-span-id)
```

### Issue: Spans Have Negative Duration or Wrong Timestamps

```bash
# Symptom: Tempo shows span with start > end

# Root cause: Clock skew between container and OTEL collector

# Debug:
kubectl exec -it <pod> -- date -u
kubectl exec -it $(kubectl get pod -n observability -l app=otel-collector -o name | head -1) -- date -u

# Should be within 1 second of each other

# Fix: Sync node clocks
ntpd -g  # Or use Kubernetes time sync admission controller
```

### Issue: CDEvent Never Triggers Next Stage

```bash
# Symptom: build completes, but test PipelineRun never created

# Debug checklist:
# 1. Verify send-cdevent task ran and succeeded
kubectl -n app-demo-cicd logs <build-pod> -c send-cdevent

# 2. Check broker received it
kubectl -n platform-system logs -l app=el-cdevents-broker | grep "artifact.published"

# 3. Check TriggerTemplate selector matches
kubectl -n platform-system get triggertemplates test-trigger-template -o yaml
# Look for: when.name: "event-type", value: "dev.cdevents.artifact.published.*"

# 4. Check test PipelineRun was created
kubectl -n app-demo-cicd get pipelinerun | grep "test-"

# Common fixes:
# - Broker URL is wrong → fix in send-cdevent.yaml env
# - Trigger selector pattern doesn't match → update when clause
# - ServiceAccount token expired → refresh (projected tokens auto-refresh)
```

### Issue: OTEL Spans Not in Tempo but Logs Show Success

```bash
# Symptom: otel-cli prints no error, but span never appears in Tempo

# Possible causes:
# 1. Collector is dropping samples (buffer full)
#    Check: kubectl -n observability logs -l app=otel-collector | grep -i drop

# 2. Exporter not configured to send to Tempo
#    Check: kubectl -n observability get configmap otel-collector-config -o yaml | grep -A 10 exporters

# 3. Span attributes don't match Tempo's filtering
#    Check: Go to Tempo UI, manually search by trace ID

# 4. Trace ID collision (unlikely but possible)
#    Check: Are you sending spans with hardcoded trace IDs instead of generated ones?
```

---

## Part 7: Monitoring & Alerting

### PrometheusRules for Platform Health

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: platform-cicd-health
  namespace: observability
spec:
  groups:
    - name: platform-cicd
      interval: 30s
      rules:
        # Alert if flow takes > 10 minutes
        - alert: PlatformCICDFlowTooSlow
          expr: |
            histogram_quantile(0.95, rate(
              platform_cicd_flow_duration_seconds_bucket[5m]
            )) > 600
          for: 10m
          annotations:
            summary: "Platform flow taking > 10 minutes (p95)"

        # Alert if build stage fails > 5% of runs
        - alert: BuildStageTooManyFailures
          expr: |
            rate(platform_cicd_stage_failures_total{stage="build"}[1h]) /
            rate(platform_cicd_stage_total{stage="build"}[1h]) > 0.05
          for: 5m
          annotations:
            summary: "Build stage failing > 5% of runs"

        # Alert if otel-cli calls are failing
        - alert: OTELSpanSendFailures
          expr: |
            rate(platform_cicd_span_send_errors_total[5m]) > 0
          annotations:
            summary: "OTEL spans failing to send"
```

### Grafana Dashboard Queries

```
# Panel 1: Flow duration over time
SELECT
  $__timeGroup(timestamp, 1h) AS time,
  AVG(flow_duration_seconds) AS avg_duration,
  MAX(flow_duration_seconds) AS max_duration
FROM platform_cicd_flows
WHERE timestamp > now() - interval '7 days'
GROUP BY time
ORDER BY time DESC
```

```
# Panel 2: Stage success rate by app
SELECT
  app_name,
  stage,
  COUNT(CASE WHEN outcome='success' THEN 1 END) * 100.0 /
  COUNT(*) AS success_rate_percent
FROM platform_cicd_events
WHERE timestamp > now() - interval '24 hours'
GROUP BY app_name, stage
ORDER BY app_name, stage
```

```
# Panel 3: Trace volume (events per hour)
SELECT
  $__timeGroup(timestamp, 1h) AS time,
  COUNT(*) AS event_count,
  COUNT(DISTINCT chain_id) AS unique_flows
FROM platform_cicd_events
WHERE timestamp > now() - interval '7 days'
GROUP BY time
ORDER BY time DESC
```
