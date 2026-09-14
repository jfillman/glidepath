#!/usr/bin/env bash
# catalog/lib/argocd-outcome-hook.sh
#
# Runs as an ArgoCD PostSync/SyncFail hook Job on a cluster-mapped upper-env cluster -
# reports a confirmed release outcome straight to the dev cluster's
# argocd-outcome-relay. Replaces an earlier ArgoCD-Notifications design: Notifications
# fired on any completed sync (including pure selfHeal drift, no release involved), and
# reading per-release data off the Application object's own metadata was racy against
# the app-of-apps root's independent reconcile loop. A hook Job avoids both - it only
# runs when its own sync had something to apply, PostSync only fires once healthy, and
# every value here is an env var baked in by open-release-pr.yaml at commit time
# instead of read live. See docs/admin/multi-cluster.md.
#
# PHASE is baked in per-hook-type (Succeeded/Failed) rather than discovered live -
# which hook ran already tells us the outcome.
#
# This script builds the full CDEvents envelope itself (mirroring catalog/lib/
# cdevents.sh's shape - not sourcing that file directly, since its cdevent_send
# assumes an in-cluster broker-audience SA token this hook Job, running on a remote
# cluster, doesn't have) and sends it to argocd-outcome-relay as-is. The relay's only
# job is authenticating this call (shared secret, since TokenReview can't validate a
# token from a different cluster's API server) and forwarding the same bytes on to the
# broker with its own credentials - it no longer reshapes a flat request into a
# CDEvent. See argocd-outcome-relay/main.go's header for the other side of this split.
set -euo pipefail

: "${RELAY_URL:?RELAY_URL must be set}"
: "${APP_NAMESPACE:?APP_NAMESPACE must be set}"
: "${APP_NAME:?APP_NAME must be set}"
: "${ENV:?ENV must be set}"
: "${CLUSTER:?CLUSTER must be set}"
: "${PHASE:?PHASE must be set}"
: "${POD_NAMESPACE:?POD_NAMESPACE must be set - downward API, set on the hook Job spec}"

# CHAIN_ID is optional, unlike the required fields above - it feeds
# release-outcome-span.yaml's Tempo correlation, and making it required would break
# every release from an app onboarded before it existed. PR_URL/PR_CREATED_AT used to be
# read here too, but never round-trip through git/this hook Job any more - they can't be
# known at PR-open commit time (the PR doesn't exist yet), and a second commit to add
# them once it did re-triggered every governance gate on the gitops repo a second time
# on every release. release-outcome-notify.yaml's resolve-release-tracking Task reads
# them straight from a dev-cluster ConfigMap instead - see open-release-pr.yaml's header.

# Shared per-cluster secret, hand-provisioned once per app namespace on THIS cluster -
# never committed to git, never passed through open-release-pr.yaml (which runs on the
# dev cluster and never sees it). See docs/admin/multi-cluster.md for provisioning.
token="$(kubectl get secret platform-outcome-relay-token -n "${POD_NAMESPACE}" -o jsonpath='{.data.token}' | base64 -d)"
finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Two vocabularies, both pre-computed here rather than left for a downstream consumer
# to derive: "outcome" is CDEvents' success/failure convention, "status" is the
# Succeeded/Failed(/Syncing) vocabulary notify-slack.yaml expects. PHASE already tells
# us which hook ran, so this is a plain case, not a live status read.
#
# Syncing (PreSync, added 2026-09-14) gets its own CDEvent type
# (environment.deploying), not a third value layered onto environment.deployed's
# outcome/status pair - every other consumer of that type (release-outcome-notify's
# Pipeline/Trigger, DORA lead-time/MTTR, the release log, the outcome span) hard-
# assumes a TERMINAL result, so folding a non-terminal phase into the same type would
# either need every one of those to grow a Syncing branch or risk one silently
# mishandling it. A distinct type lets the Syncing case be consumed by its own
# Trigger/Pipeline with none of the existing ones touched at all.
outcome="failure"
status="Failed"
event_type="dev.cdevents.environment.deployed.0.1.0"
if [[ "${PHASE}" == "Succeeded" ]]; then
  outcome="success"
  status="Succeeded"
elif [[ "${PHASE}" == "Syncing" ]]; then
  outcome="in_progress"
  status="Syncing"
  event_type="dev.cdevents.environment.deploying.0.1.0"
fi
source="/platform-cicd/${APP_NAMESPACE}/${CLUSTER}-${APP_NAME}-${ENV}-outcome"

# finishedAt is part of the id, not just source+eventType: source is a fixed string
# per app/env/cluster (no per-attempt uniqueness), so without finishedAt every
# distinct release outcome for the same app/env would collide on the same id and only
# the first would ever create a release-outcome-notify PipelineRun - later ones would
# silently no-op as "already exists". A true redelivery of the same outcome still
# carries the same finishedAt, so idempotency is preserved. Same truncated-sha256
# scheme as cdevents.sh's own cdevent_send, for the same 63-char resource-name-limit
# reason.
event_id="$(printf '%s' "${source}:${event_type}:${finished_at}" | sha256sum | cut -d' ' -f1 | cut -c1-20)"

config_json="{}"
if [[ -n "${CONFIG_JSON_B64:-}" ]]; then
  decoded="$(printf '%s' "${CONFIG_JSON_B64}" | base64 -d 2>/dev/null || true)"
  if jq -e . >/dev/null 2>&1 <<<"${decoded}"; then
    config_json="${decoded}"
  else
    echo "[argocd-outcome-hook] warning: CONFIG_JSON_B64 did not decode to valid JSON, falling back to {}" >&2
  fi
fi

# jq -n --arg, not string interpolation, so every field is safely JSON-encoded
# regardless of content (same convention as cdevents.sh).
payload="$(jq -n \
  --arg id "${event_id}" \
  --arg type "${event_type}" \
  --arg source "${source}" \
  --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg chainId "${CHAIN_ID:-}" \
  --arg appNamespace "${APP_NAMESPACE}" \
  --arg appName "${APP_NAME}" \
  --arg env "${ENV}" \
  --arg cluster "${CLUSTER}" \
  --arg phase "${PHASE}" \
  --arg outcome "${outcome}" \
  --arg status "${status}" \
  --arg revision "${GITOPS_REVISION:-}" \
  --arg gitUrl "${GIT_URL:-}" \
  --arg gitRevision "${GIT_REVISION:-}" \
  --arg flowStartTime "${FLOW_START_TIME:-}" \
  --arg finishedAt "${finished_at}" \
  --argjson configJson "${config_json}" \
  '{
    context: {
      version: "0.4.1",
      id: $id,
      source: $source,
      type: $type,
      timestamp: $timestamp,
      chainId: $chainId
    },
    subject: {
      id: $source,
      source: $source,
      type: "environment",
      content: {
        appNamespace: $appNamespace,
        appName: $appName,
        env: $env,
        cluster: $cluster,
        phase: $phase,
        outcome: $outcome,
        status: $status,
        revision: $revision,
        gitUrl: $gitUrl,
        gitRevision: $gitRevision,
        flowStartTime: $flowStartTime,
        finishedAt: $finishedAt,
        configJson: ($configJson | tojson)
      }
    },
    customData: {
      platform: { traceparent: "", flow_start_time: $flowStartTime, config_json: "{}" }
    },
    customDataContentType: "application/json"
  }')"

echo "[argocd-outcome-hook] app=${APP_NAME} env=${ENV} cluster=${CLUSTER} phase=${PHASE}: reporting to ${RELAY_URL}"
curl --fail --silent --show-error --max-time 15 \
  --retry 3 --retry-connrefused \
  -X POST "${RELAY_URL}" \
  -H "Authorization: Bearer ${token}" \
  -H "Content-Type: application/cloudevents+json" \
  -d "${payload}"
echo
