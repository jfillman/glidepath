# ADR-0022: CDEvents conformance and the Glidepath event vocabulary

*Status: Proposed (2026-10-06). This ADR changes no code. It records what the current
events do against the CDEvents specification, which events a rollout deployment should
generate, and a staged way to close the gaps. Checked against `cdevents/spec` **v0.5.1**
(the latest tag; v0.5.0 differs only in the `specversion` string) by reading the spec
text and the published JSON schemas, not from memory.*

## Context

Glidepath chains stages with CDEvents ([ADR-0002](0002-cdevents-broker-tokenreview.md)).
Two outside reviews (`chatgpt-cdevents.md`, `chatgpt-cdevents-broker.md`) raised that the
envelope is behind the spec and that Tower could consume the stream. This ADR checks both
against the real spec and adds what they missed.

**The architecture is sound.** Events are used as declarative facts ("an artifact was
published") and the Trigger CEL decides what to do with them, which is the adapter pattern
the CDEvents primer describes. Nothing here argues for changing that.

**Nothing consumes these events through a CDEvents-aware tool today.** The only consumers
are our own Tekton Triggers, which match on `context.type`, `context.source`, `chainId` and
a handful of fields. That is why the gaps below have cost nothing yet, and why they are
worth closing only as far as they are cheap.

### What the current events do against the spec

| # | Spec (v0.5.1) | Today | Effect |
|---|---|---|---|
| 1 | `context.specversion` is required | We emit `context.version: "0.4.1"` | Not a valid 0.5.x envelope |
| 2 | v0.4.1 spells the chain field `chain_id`; 0.5.x spells it `chainId` | We emit `chainId` with `version: "0.4.1"` | A hybrid that validates against neither version |
| 3 | `subject.type` was removed in 0.5; the subject schema is closed | We emit `subject.type` | Rejected by a 0.5.x schema |
| 4 | `subject.id` identifies the subject, and later events about the same subject must reuse it | For `artifact`, `service`, `change` and `testCaseRun` events it is the **emitting PipelineRun's name** | Two deployments of one service get different subject ids |
| 5 | `subject.content` is a closed schema per event (`additionalProperties: false`) | Standard events carry extra fields: `artifact.published` has `image`, `revision`, `git-url`, `env` (the schema allows only `sbom`, `user`); `service.deployed` has `git-url`, `revision`; `testcaserun.finished` has `image`, `revision`, `git-url`, `env`, `test-name`; `change.created` has `image`, `revision`, `git-url` | Rejected by any schema-validating consumer |
| 6 | Required fields | `pipelinerun.started` lacks `uri`; `testcaserun.finished` lacks `environment`; `service.deployed.artifactId` is a bare image ref, the spec asks for a Purl | Incomplete |
| 7 | `testcaserun.finished.outcome` is one of `success`, `failure`, `cancel`, `error` | `test.yaml` passes `$(tasks.status)` (Tekton's words) into it; whether it is mapped first is unchecked | Possibly invalid |
| 8 | `change.created` is `0.4.0` | We emit `0.3.0` | Wrong event version |
| 9 | The `dev.cdevents.` namespace is reserved for events the spec defines; extensions use `dev.cdeventsx.<tool>-<subject>.<predicate>` | `environment.deploying.0.1.0` and `environment.deployed.0.1.0` are ours, under `dev.cdevents.`, with a made-up version | Collides with the reserved namespace. The spec has no event named either. |
| 10 | `links` and `chainId` are optional | `chainId` yes, `links` never | Causality is implicit in our own conventions |

`pipelinerun.started/finished.0.3.0`, `artifact.published.0.3.0`, `service.deployed.0.3.0`
and `testcaserun.finished.0.3.0` are the right event types at the right versions.

**Correction to the outside review:** it is right about 1 to 3 and 10, but it misses 4 to 9,
which matter more for anyone validating events than the version string does. It also treats
`service.deployed` as the answer for rollouts; see decision 4.

### `customData` and the `cicd.yaml` blob

`customData.platform` carries `traceparent`, `flow_start_time`, `chain_slug` and
`config_json`. The spec allows any `customData`, and the first three are platform
metadata, which is what it is for. `config_json` is different: it is the whole `cicd.yaml`,
carried on **every** event so a later stage can skip cloning the repo. That is state
transport, not event meaning. It makes every event large, copies an application's config
into every consumer and any future store, and ties each stage to whatever config the first
stage saw. The release outcome events add a second copy as `subject.content.configJson`.

The outside review proposes the same fix this ADR does: keep the event thin and hold the
state under `chainId`. [ADR-0021](0021-rollout-facts-and-release-record.md) already builds
that for releases (the ReleaseRecord, keyed by chain-id, on dev); this extends the idea to
the flow's config.

## Decision (proposed)

1. **Target envelope: CDEvents 0.5.1.** `context.specversion: "0.5.1"`, no `subject.type`,
   `chainId` as already spelled, event types at the versions the spec lists. Do not bump
   event-type versions wholesale; they are per-event (`change.created` is `0.4.0`, most are
   `0.3.0`).
2. **Content discipline.** A standard event's `subject.content` carries only the fields the
   spec defines for that event. Everything Glidepath needs beyond that (image, revision,
   git url, env, test name, app namespace, release id, flow start time, outcome detail) goes
   in `customData.platform`. Fill the spec's required fields: `uri` on pipeline runs,
   `environment` on test and service events, `artifactId` as a Purl
   (`pkg:oci/<app>@sha256:<digest>?repository_url=<registry/repo>`), `outcome` mapped to the
   spec's four values.
3. **Subject ids identify the subject.** Artifact: its Purl. Service: `service/<app>`.
   Environment: `{id: "<cluster>/<env>"}`. Pipeline run: the run name (already correct).
   The emitting run's name moves to `customData.platform` where it is still needed.
4. **Events for a rollout deployment.** The spec has no "deployment started" and no
   "deployment failed" event: `service.*` events describe a successful state change, and an
   unsuccessful deployment is reported as a finished pipeline run with `outcome` and
   `errors`. A rollout is therefore modelled as a pipeline run named
   `rollout/<app>/<cluster>/<env>` (subject id: the `release-id` from ADR-0021), plus the
   service events for what it changed:

   | Moment | Event |
   |---|---|
   | Release PR opened | `change.created.0.4.0` (content: `repository`, `description`) |
   | Release PR merged | `change.merged.0.3.0` (optional; the Tower poller or a GitHub webhook can emit it; it is the approval anchor for lead time) |
   | Rollout begins (`Progressing`) | `pipelinerun.started.0.3.0` |
   | Canary step, pause, analysis | None. These stay in the ReleaseRecord and the trace; no spec event fits and a custom one would be noise. |
   | Rollout `Healthy`, first release of this app in this environment | `service.deployed.0.3.0` and `pipelinerun.finished` with `outcome: success` |
   | Rollout `Healthy`, a later release | `service.upgraded.0.3.0` and `pipelinerun.finished` with `outcome: success` |
   | Abort, `Degraded`, analysis failure | `pipelinerun.finished` with `outcome: failure` (or `cancel` for a manual abort) and `errors`. No service event: the spec has none for a failed deploy. |
   | Rollback release `Healthy` | `service.rolledback.0.3.0` and `pipelinerun.finished` |
   | Observed release-id matches no open release (drift) | No spec event. Optional `dev.cdeventsx.glidepath-release.drifted.0.1.0`; decide when drift alerting is built. |
   | SLO breach and recovery (Tower's SLO notifications) | `incident.detected.0.3.0` and `incident.resolved.0.3.0` (continuous operations), which is the spec-native feed for MTTR |
   | Environment added or removed (Tower Environments tab) | `environment.created.0.3.0`, `environment.modified.0.3.0`, `environment.deleted.0.3.0` |

   `service.deployed` versus `service.upgraded` is decided from the ReleaseRecord: whether
   an earlier release of this app is recorded Healthy in this environment. This replaces
   `environment.deploying` and `environment.deployed`, which are retired once their
   consumers (`release-progress-trigger`, `release-outcome-trigger`) read the new events.
5. **Links, cheaply, later.** Each stage's events carry
   `links: [{linkType: RELATION, linkKind: TRIGGER, target: {contextId: <triggering event id>}}]`.
   The triggering event's id is already `body.context.id` in the Trigger. The first event of
   a chain has no link, the last carries `END`. We build no links service: the point is that
   an external tool (or Tower, if it ever wants this) could. Low value until a consumer
   exists.
6. **The `cicd.yaml` blob leaves the events.** A `FlowRecord` (a ConfigMap keyed by
   chain-id, written once at flow start, immutable, TTL-swept) holds the config and flow
   start time; `preflight` (then `resolve-notify-config`) reads it by chain-id instead of from the event. This
   is the same record the ReleaseRecord extends, so there is one record per chain, not two.
   A missing record needs a defined fallback before this ships: the stage re-reads `cicd.yaml`
   from git, which is slower but correct.
7. **Not now: an event store, a NATS or Postgres gateway, or Tower as a CDEvents consumer.**
   The outside review proposes a persistent gateway and fan-out. We have one consumer (Tekton
   Triggers) and one other reader (Tower), and Tower already reads PipelineRuns (with
   Tekton Results) and Rollouts directly, plus the ReleaseRecord for releases. A durable
   store is a new stateful component for no current need. The terminal ReleaseRecord is
   written to the release log (Loki) and to git; writing it in CDEvents shape
   (`service.upgraded` plus links) is free and gives any later consumer the history without
   a new store. Revisit when a second consumer needs replay, or when Autopilot wants to
   subscribe rather than poll.
8. **A conformance test.** A CI job renders each event the catalog emits (from fixtures)
   and validates it against the pinned CDEvents JSON schemas. It starts with an explicit
   list of expected failures (rows 1 to 9 above) and shrinks as each is fixed, so the gap
   is measured and cannot grow.

## Consequences

- Closing rows 1 to 8 is mechanical and local to `cdevents.sh`, the three event builders
  (`cdevent_send`, the hook script, the relay's fact path) and `flow-triggers.yaml`, which
  reads a handful of non-spec content fields (`revision`, `git-url`, `env`, and on the
  outcome path `appNamespace`, `gitUrl`). Moving those fields to `customData.platform` needs the
  bindings to read both places during the change (`has()`), then drop the old one.
- Row 9 is the only one that changes meaning: consumers of `environment.deployed` and
  `environment.deploying` (`release-outcome-trigger`, `release-progress-trigger`,
  `release-outcome-notify`, the DORA path) move to `pipelinerun.finished` and
  `service.upgraded`. It is sequenced after ADR-0021 phase 3, when the hook path that builds
  those events is deleted, so the migration happens once.
- ADR-0021's relay emits the legacy `environment.*` events in phase 1 on purpose, to keep
  both paths comparable. Its new events (`service.rolledback`, and the rollout pipeline run)
  follow this ADR from the start.
- Modelling a rollout as a pipeline run is a judgment call the spec does not state. The
  alternative is a custom `dev.cdeventsx.glidepath-rollout.*` family. It is rejected here
  because the spec says custom events should be a last resort and `pipelineRun` is defined
  as an execution of a delivery process, which a rollout is. Revisit if a consumer finds it
  confusing.
- Fully validating consumers do not exist yet, so none of this is urgent. The reason to do
  it anyway is that the work is small now and grows with every new event and consumer.

## Build order

| Phase | What | Verifiable by |
|---|---|---|
| 0 | Conformance test with the expected-failure list (decision 8) | CI shows the gap per row |
| 1 | Envelope, content, subject ids, required fields, the `outcome` mapping, `change.created` version; triggers read both places | Failure list shrinks to row 9 and the blob; a full build-to-release flow still chains |
| 2 | `FlowRecord`; `config_json` and `configJson` leave the events | An event is a fraction of today's size; a flow still chains with a missing record |
| 3 | Links | Events validate with links; a chain can be walked by id |
| 4 | Retire `environment.deploying` and `environment.deployed` in favour of decision 4's events (after ADR-0021 phase 3) | Release outcome Slack, DORA and the release log unchanged on a live release |
| 5 | Incident and environment events from Tower | `incident.detected` on an SLO transition |

## Not decided here

- Whether to publish our event shapes in the CDEvents custom-event registry. Only worth it
  if we keep any `dev.cdeventsx` events.
- Whether `change.merged` is emitted by a webhook, by the Tower poller, or not at all.
- Whether the event `source` should be one stable value per cluster instead of one per
  pipeline run. The spec allows both; per-run is more specific and is kept until a consumer
  asks.
