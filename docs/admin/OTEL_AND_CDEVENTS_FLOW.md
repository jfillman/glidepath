# OTEL Tracing & CDEvents Flow Architecture

## Overview

This document illustrates how OpenTelemetry spans and CloudEvents (CDEvents) are created, threaded through Tekton PipelineRuns, and used to correlate distributed traces across a complete CI/CD flow (build → test → deploy → release).

---

## Key Concepts

### W3C Traceparent Format
```
00-trace-id-span-id-01
   ↓          ↓        ↓
   |          |        +-- trace flags (01 = sampled)
   |          +---------- span-id (16 hex chars = 64 bits)
   +------------------- trace-id (32 hex chars = 128 bits)
```

### Two Distinct but Complementary Correlators

| Field | Purpose | Owner | Lifecycle |
|-------|---------|-------|-----------|
| **traceparent** | W3C distributed trace context; stitches OTEL spans across all PipelineRuns into one Tempo trace | OTEL | Flows unchanged through entire build→test→deploy→release sequence |
| **chainId** | CDEvents' own causal-sequence identifier; documents event ordering | CDEvents spec | Also flows unchanged through sequence; used by Trigger templates |

Both are generated at flow-start (build) and threaded through all downstream stages.

---

## Stage 1: BUILD Pipeline

### 1.1 Flow Initialization (start-flow-root-span task)

```bash
# otel_flow_root_begin generates:
traceparent="00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01"
flow_start_time="2025-08-06T14:32:15.123456789Z"
chain_id="550e8400-e29b-41d4-a716-446655440000"

# These are emitted as Task results and passed as params to all downstream stages
```

### 1.2 Build Stage Span Creation

```bash
# start-stage-span task calls otel_stage_span_begin:
stage_span_id="f2e1d3c5a7b9e1f3"  # Fresh 16-char hex ID
stage_start_time="2025-08-06T14:32:20.234567890Z"

# Result structure:
span-id: f2e1d3c5a7b9e1f3
start-time: 2025-08-06T14:32:20.234567890Z
```

### 1.3 Building & Publishing Image

Task `build-image.yaml` runs kaniko and emits a span:

```bash
# In emit-image-ref-and-span step:
otel_task_span_send "build-image" \
  "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01" \
  "f2e1d3c5a7b9e1f3" \
  "2025-08-06T14:32:20.234567890Z" \
  "2025-08-06T14:32:45.567890123Z" \
  "Succeeded"
```

**OTEL Span Emitted to Collector:**
```json
{
  "traceId": "2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c",
  "spanId": "<freshly-minted-by-openssl-rand>",
  "parentSpanId": "a7e3f2d8c1b5e9a2",
  "name": "build-image",
  "startTime": "2025-08-06T14:32:20.234567890Z",
  "endTime": "2025-08-06T14:32:45.567890123Z",
  "status": {
    "code": "OK"
  },
  "attributes": {
    "service.name": "platform-cicd"
  }
}
```

### 1.4 Build Pipeline Completes - Emit CDEvent

The `finally` block calls `send-cdevent` task with:

```bash
PLATFORM_CHAIN_ID="550e8400-e29b-41d4-a716-446655440000"
PLATFORM_TRACEPARENT="00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01"
PLATFORM_FLOW_START_TIME="2025-08-06T14:32:15.123456789Z"
TEKTON_PIPELINE_RUN="build-nodejs-demo-app-abc123"
NAMESPACE="app-myapp-cicd"
```

**CDEvent Payload Sent to Broker:**
```json
{
  "context": {
    "version": "0.4.1",
    "id": "a1b2c3d4e5f6g7h8",
    "source": "/platform-cicd/app-myapp-cicd/build-nodejs-demo-app-abc123",
    "type": "dev.cdevents.artifact.published.0.3.0",
    "timestamp": "2025-08-06T14:33:12.123456Z",
    "chainId": "550e8400-e29b-41d4-a716-446655440000"
  },
  "subject": {
    "id": "build-nodejs-demo-app-abc123",
    "source": "/platform-cicd/app-myapp-cicd/build-nodejs-demo-app-abc123",
    "type": "artifact",
    "content": {
      "name": "myapp",
      "version": "abc123",
      "uri": "ghcr.io/myorg/myapp@sha256:abc123def456"
    }
  },
  "customData": {
    "platform": {
      "traceparent": "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01",
      "flow_start_time": "2025-08-06T14:32:15.123456789Z"
    }
  },
  "customDataContentType": "application/json"
}
```

### 1.5 End Flow-Root Span (Build's Finally Block)

```bash
# end-flow-root-span task calls otel_span_send with empty span_id:
otel_span_send \
  "build" \
  "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01" \
  "" \  # Empty = use flow root's own span ID as the span ID
  "2025-08-06T14:32:15.123456789Z" \
  "2025-08-06T14:33:12.456789012Z" \
  "Succeeded"
```

**OTEL Flow-Root Span:**
```json
{
  "traceId": "2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c",
  "spanId": "a7e3f2d8c1b5e9a2",
  "parentSpanId": null,
  "name": "build",
  "startTime": "2025-08-06T14:32:15.123456789Z",
  "endTime": "2025-08-06T14:33:12.456789012Z",
  "status": { "code": "OK" },
  "attributes": {
    "service.name": "platform-cicd",
    "platform.flow": "build"
  }
}
```

---

## Stage 2: TEST Pipeline

### 2.1 Receiving the Event

Tekton Triggers receives the CDEvent from broker and creates a new PipelineRun:

```bash
# TriggerTemplate uses:
chain-id="$(body.context.chainId)"  # ← Same as build!
traceparent="$(body.customData.platform.traceparent)"
flow-start-time="$(body.customData.platform.flow_start_time)"
```

**New PipelineRun created:**
```yaml
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  name: test-a1b2c3d4e5f6g7h8  # Named from CDEvent ID
  namespace: app-myapp-cicd
spec:
  pipelineRef:
    resolver: cluster
    params:
      - name: name
        value: test
  params:
    - name: flow-traceparent
      value: "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01"
    - name: chain-id
      value: "550e8400-e29b-41d4-a716-446655440000"
    - name: flow-start-time
      value: "2025-08-06T14:32:15.123456789Z"
```

### 2.2 Test Stage Span

```bash
# start-stage-span generates fresh test stage span (same trace ID, different span ID):
trace_id="2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c"  # ← SAME as build!
stage_span_id="e9d2f7a1c4b8e3f6"  # ← DIFFERENT sibling
stage_start_time="2025-08-06T14:33:45.789012345Z"

# Relationship:
# Flow-root span (a7e3f2d8c1b5e9a2) spans entire flow
#   ├── Build stage span (f2e1d3c5a7b9e1f3) [parent: flow-root]
#   └── Test stage span (e9d2f7a1c4b8e3f6) [parent: flow-root] ← NEW
```

### 2.3 Test Execution Spans

```bash
# Multiple nested Task spans under test stage:

# Task: clone-repo
otel_task_span_send "clone-repo" \
  "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01" \
  "e9d2f7a1c4b8e3f6" \
  "2025-08-06T14:33:45.789012345Z" \
  "2025-08-06T14:33:48.901234567Z" \
  "Succeeded"

# Task: run-tests
otel_task_span_send "run-tests" \
  "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01" \
  "e9d2f7a1c4b8e3f6" \
  "2025-08-06T14:33:50.123456789Z" \
  "2025-08-06T14:34:02.345678901Z" \
  "Succeeded"
```

### 2.4 Test Pipeline Finally Block

```bash
# end-stage-span sends test stage span:
otel_span_send \
  "stage:test" \
  "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01" \
  "e9d2f7a1c4b8e3f6" \
  "2025-08-06T14:33:45.789012345Z" \
  "2025-08-06T14:34:05.567890123Z" \
  "Succeeded"

# attrs includes chain_id:
# "platform.chain_id=550e8400-e29b-41d4-a716-446655440000,platform.stage=test"
```

### 2.5 Test Success CDEvent

```json
{
  "context": {
    "version": "0.4.1",
    "id": "b2c3d4e5f6g7h8i9",
    "source": "/platform-cicd/app-myapp-cicd/test-a1b2c3d4e5f6g7h8",
    "type": "dev.cdevents.testcaserun.finished.0.3.0",
    "timestamp": "2025-08-06T14:34:10.123456Z",
    "chainId": "550e8400-e29b-41d4-a716-446655440000"
  },
  "subject": {
    "id": "test-a1b2c3d4e5f6g7h8",
    "source": "/platform-cicd/app-myapp-cicd/test-a1b2c3d4e5f6g7h8",
    "type": "testCaseRun",
    "content": {
      "outcome": "success",
      "testSuite": "nodejs-demo-app",
      "testName": "unit-tests"
    }
  },
  "customData": {
    "platform": {
      "traceparent": "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01",
      "flow_start_time": "2025-08-06T14:32:15.123456789Z"
    }
  }
}
```

**Note:** Same `traceparent` and `chainId` as build! This is what stitches the distributed trace together.

---

## Stage 3: DEPLOY Pipeline

### 3.1 Trigger Receives Test CDEvent

```bash
# Extract from CDEvent context:
chain-id="550e8400-e29b-41d4-a716-446655440000"
traceparent="00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01"
flow-start-time="2025-08-06T14:32:15.123456789Z"

# PipelineRun created:
metadata:
  name: deploy-b2c3d4e5f6g7h8i9  # ← Named from test CDEvent ID
```

### 3.2 Deploy Stage Spans

```bash
# Deploy stage span (sibling to build and test):
stage_span_id="c3d5e8f2a6b9d1e4"  # ← DIFFERENT, but same trace
stage_start_time="2025-08-06T14:34:30.123456789Z"

# Span hierarchy in Tempo:
Trace: 2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c
├─ Flow-root span: a7e3f2d8c1b5e9a2 (14:32:15 → flow-end)
│  ├─ Build stage: f2e1d3c5a7b9e1f3 (14:32:20 → 14:33:12)
│  │  └─ build-image task: <random> (14:32:20 → 14:32:45)
│  ├─ Test stage: e9d2f7a1c4b8e3f6 (14:33:45 → 14:34:05)
│  │  ├─ clone-repo task: <random> (14:33:45 → 14:33:48)
│  │  └─ run-tests task: <random> (14:33:50 → 14:34:02)
│  └─ Deploy stage: c3d5e8f2a6b9d1e4 (14:34:30 → 14:35:10) ← NOW
│     └─ deploy task: <random> (14:34:30 → 14:35:05)
```

### 3.3 Deploy Success CDEvent

```json
{
  "context": {
    "version": "0.4.1",
    "id": "c3d4e5f6g7h8i9j0",
    "source": "/platform-cicd/app-myapp-cicd/deploy-b2c3d4e5f6g7h8i9",
    "type": "dev.cdevents.service.deployed.0.3.0",
    "timestamp": "2025-08-06T14:35:15.123456Z",
    "chainId": "550e8400-e29b-41d4-a716-446655440000"
  },
  "subject": {
    "id": "deploy-b2c3d4e5f6g7h8i9",
    "source": "/platform-cicd/app-myapp-cicd/deploy-b2c3d4e5f6g7h8i9",
    "type": "service",
    "content": {
      "environment": "dev",
      "service": "myapp",
      "deployment": "myapp-deployment"
    }
  },
  "customData": {
    "platform": {
      "traceparent": "00-2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c-a7e3f2d8c1b5e9a2-01",
      "flow_start_time": "2025-08-06T14:32:15.123456789Z"
    }
  }
}
```

---

## Stage 4: RELEASE Pipeline (Optional Phase 2+)

Similar structure, but:
- Promotes from dev → staging → prod
- May have additional governance/approval gates
- Sends final promotion CDEvent

---

## Complete Trace View in Tempo

When you query Tempo for `traceparent = 2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c`:

```
Trace Duration: ~2m 55s (14:32:15 → 14:35:10)

Timeline:
  14:32:15 ◆─────────────────────────────── build (1m 57s) ◆
           │  14:32:20 ◆──────────◆ build-image (25s)
           │
           └─────────────────────────────── test (20s) ◆
              14:33:45 ◆──●──────●◆ clone+run-tests
              
           └─────────────────────────────── deploy (40s) ◆
              14:34:30 ◆──────────◆ deploy task

Platform Events Captured:
✓ flow-root span → platform-cicd build orchestration
✓ stage spans + attributes → stage timing + chain_id for audit
✓ task spans → individual work unit timing
✓ CDEvent chainId → cross-PipelineRun causality tracking
```

---

## Data Threading Summary

```
BUILD PipelineRun (triggered by git commit)
  │
  ├─ otel_flow_root_begin()
  │  └─ generates: traceparent, flow_start_time
  │
  ├─ otel_stage_span_begin()
  │  └─ generates: build stage span_id + start_time
  │
  ├─ Task steps
  │  └─ otel_task_span_send() → OTEL Collector
  │
  └─ finally: send-cdevent (artifact.published)
     ├─ includes: traceparent, chain_id, flow_start_time
     └─ POST to broker → triggers TEST

TEST PipelineRun (triggered by CDEvent from build)
  │
  ├─ Receives from CDEvent:
  │  ├─ traceparent (unchanged)
  │  ├─ chain_id (unchanged)
  │  └─ flow_start_time (unchanged)
  │
  ├─ otel_stage_span_begin() → new span, SAME trace_id
  │
  ├─ Task spans (nested under test stage)
  │  └─ otel_task_span_send() → OTEL Collector
  │
  └─ finally: send-cdevent (testcaserun.finished)
     ├─ includes: SAME traceparent, SAME chain_id
     └─ POST to broker → triggers DEPLOY

DEPLOY PipelineRun (triggered by CDEvent from test)
  │
  ├─ Receives from CDEvent:
  │  ├─ traceparent (unchanged)
  │  ├─ chain_id (unchanged)
  │  └─ flow_start_time (unchanged)
  │
  ├─ otel_stage_span_begin() → new span, SAME trace_id
  │
  ├─ Deploy task
  │  └─ otel_task_span_send() → OTEL Collector
  │
  └─ finally: send-cdevent (service.deployed)
     └─ includes: SAME traceparent, SAME chain_id
```

---

## Key Invariants

1. **Trace ID is Constant**
   - Generated once at flow start (build)
   - Embedded in traceparent
   - Never changes across all PipelineRuns

2. **Stage Spans are Siblings**
   - All parent to flow-root span
   - Never nested under each other
   - Each gets unique span ID

3. **Chain ID is Constant**
   - Generated once at flow start
   - Carried in CDEvent context
   - Used by Trigger templates to name downstream PipelineRuns

4. **Flow Start Time is Constant**
   - Set at build start
   - Carried through all CDEvents
   - Useful for calculating total flow duration

5. **Idempotent Event IDs**
   - CDEvent ID = hash(PipelineRun:event-type)[0:20]
   - Same step retried = same event ID
   - Broker deduplicates based on this

---

## Tempo Query Examples

```promql
# All spans in this flow
{traceId="2f8d7a4c3b1e9f6d2a8c5e7b3f1d9a4c"}

# All spans with platform.chain_id attribute
{resource.attributes.platform.chain_id="550e8400-e29b-41d4-a716-446655440000"}

# Find all stage spans
{spanName=~"stage:.*"}

# Find all task spans
{spanName=~"build-image|clone-repo|run-tests|deploy"}

# Find errors
{status.code="ERROR"}
```

---

## Files Involved

| File | Purpose |
|------|---------|
| [otel.sh](../../catalog/lib/otel.sh) | Span minting & sending functions |
| [cdevents.sh](../../catalog/lib/cdevents.sh) | Event payload construction & broker delivery |
| [build.yaml](../../charts/glidepath-catalog/templates/pipelines/build.yaml) | Build Pipeline (flow start) |
| [start-flow-root-span.yaml](../../charts/glidepath-catalog/templates/tasks/start-flow-root-span.yaml) | Initializes traceparent & chain_id |
| [start-stage-span.yaml](../../charts/glidepath-catalog/templates/tasks/start-stage-span.yaml) | Stage span initialization |
| [end-stage-span.yaml](../../charts/glidepath-catalog/templates/tasks/end-stage-span.yaml) | Stage span completion to OTEL |
| [end-flow-root-span.yaml](../../charts/glidepath-catalog/templates/tasks/end-flow-root-span.yaml) | Flow-root span completion |
| [send-cdevent.yaml](../../charts/glidepath-catalog/templates/tasks/send-cdevent.yaml) | CDEvent delivery to broker |
| [build-image.yaml](../../charts/glidepath-catalog/templates/tasks/build-image.yaml) | Example of task span emission |
| [deploy.yaml](../../charts/glidepath-catalog/templates/pipelines/deploy.yaml) | Deploy Pipeline (receives CDEvent) |
