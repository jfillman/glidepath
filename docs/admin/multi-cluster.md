# Multi-cluster releases + the ArgoCD feedback loop

`build`/`test`/`deploy` stay on the dev cluster always - this doc is only about where
`release` promotions land and how their outcome gets back to the dev cluster's broker.

**2026-08-16: the GitOps app-of-apps delivery mechanism this doc describes below (the
Application manifest `open-release-pr.yaml` used to write into `gitopsApplicationsPath`,
plus the RBAC/outcome-hook Jobs and everything downstream of them - the relay, the
`release-outcome-*` Pipeline/Trigger, cluster-mapped DORA) was removed from
`open-release-pr.yaml`.** `airframe`'s `ApplicationEnvironment` composition
now scaffolds every `gitops-<app-name>` repo as `<cluster>/<env>/values.yaml`, and its
own tenant-onboarding `ApplicationSet` already creates and owns the ArgoCD `Application`
for that path generically - this platform writing a second, competing `Application` for
the same path would only race idp's, not add anything. `open-release-pr.yaml`'s job is
back down to patching the image and opening the PR; `cluster` is now required on every
release (the manifest path itself needs it), not just the cluster-mapped case this doc
originally scoped it for. Everything below this point (through the "GitOps app-of-apps
delivery" section and the two live-verification write-ups) is kept as an honest record
of what was actually built and live-verified, same spirit as the Notifications->hooks
supersession later in this doc - **not a current, working mechanism for the
Application/RBAC-manifest-writing part.**

**2026-08-17: the DORA-metrics/Slack-notification gap this left is rebuilt** - see the
"Outcome reporting, rebuilt (2026-08-17)" section further down for the current design.
Short version: the two ArgoCD sync-hook Jobs described below (PostSync/SyncFail,
`platform-outcome-postsync`/`platform-outcome-syncfail`) are real again, unchanged in
shape and behavior - same relay, same HTTP path, same per-app shared secret. What
changed is WHO writes them: `open-release-pr.yaml` no longer writes a second,
GitOps-delivered Application manifest to carry them (the mechanism the rest of this
section describes) - they're now rendered directly by `airframe`'s own
`airframe-application` Helm chart, off a `releaseTracking:` values block
`open-release-pr.yaml` patches into the SAME `<cluster>/<env>/values.yaml` commit it
already writes `rollout.image.*` into. Read everything below as accurate history of the
mechanism's *design* (why hooks over Notifications, why a shared secret, why the relay
is shaped the way it is, the real bugs found live) - just not accurate about WHERE the
hook Jobs get written from any more.

## Terminology

- **env** - a logical upper-environment name from `cicd.yaml` (`deploy.upperEnvironments`,
  a release step's own `env:` field). Not tied to any particular cluster by name -
  `staging` is just whatever name a tenant's `cicd.yaml` uses.
- **cluster** - which physical Kubernetes cluster hosts a given env, named via a release
  step's `cluster:` field and resolved against the control-plane chart's `clusters:`
  registry (`charts/glidepath-control-plane/values.yaml`). An env with no `cluster`
  set stays on the dev cluster, exactly like today - this is the fully-backward-compatible
  default every existing tenant is still on.

Why two separate names instead of one: an env is a tenant-facing config concept (what a
developer writes in `cicd.yaml`), a cluster is platform infrastructure (which physical
cluster the platform operator provisioned). Collapsing them would mean every tenant's
`cicd.yaml` has to know real cluster identifiers, and every cluster rename would be a
tenant-facing breaking change. Keeping them separate, joined only through the registry,
means `cicd.yaml` never hardcodes infrastructure.

## Why not just extend ArgoCD on the dev cluster to manage a remote cluster

Considered and rejected (design discussion, 2026-08-04): registering a remote cluster's
credentials into the dev cluster's own ArgoCD would put a dev-cluster-resident credential
capable of touching the upper cluster back in the picture - the exact blast-radius shape
this platform's broker/impersonation model was built to avoid elsewhere (see
[chaining.md](chaining.md)). Each upper cluster gets its **own** ArgoCD instance instead,
and the only thing that ever crosses from dev to an upper cluster is a reviewed, merged
git commit - never a live API call in that direction.

## The first upper cluster: `kind-prod`

A kind cluster literally named `prod` (podman container `prod-control-plane`), created
2026-08-03 during this feature's own design exploration. Its **name** is `prod`; the
**env** it currently hosts is `staging` (matching every existing hardcoded reference in
the codebase before this work - see the Phase C section below) - don't read the cluster's
name as which env lives on it, that mapping lives entirely in the `clusters:` registry.

Bootstrapped via [hack/bootstrap-upper-cluster.sh](../../hack/bootstrap-upper-cluster.sh):
pinned ArgoCD (`argo/argo-cd` chart `10.3.2`, app `v3.5.0` - unlike kind-observe's
reused ArgoCD, which has no version recorded anywhere in this repo) with the
Notifications controller enabled (`argocd-notifications-controller`, confirmed live,
`1/1 Ready`).

**Cross-cluster reachability, confirmed live (2026-08-10)**: podman's `kind` provider
puts every kind cluster's node container on one shared `kind` bridge network by default
(`podman network ls` - unlike Docker Desktop's kind provider, which gives each cluster
its own isolated network). No `podman network connect` step was needed - verified with a
direct container-to-container call:

```
podman exec prod-control-plane curl -sk https://10.89.0.2:6443/healthz    # kind-observe's apiserver -> ok
podman exec observe-control-plane curl -sk https://10.89.0.3:6443/healthz # kind-prod's apiserver -> ok
```

Practical implication: a NodePort Service on either cluster is reachable from the other
cluster's pods at `<node-container-ip>:<nodePort>` with no extra networking setup. This
is what the relay service (see the feedback-path section, added once Phase E lands)
exposes itself through - same-host podman reachability is enough for this pass; a
genuinely remote upper cluster would need real ingress/DNS instead (see
[installation.md](installation.md)'s "What's different on a real cluster" section for the
equivalent caveat already documented for the dev cluster).

## The cluster registry and config-driven envs

`cicd.yaml`'s `deploy.upperEnvironments` (already existed, previously RBAC-only) is now
the single source of truth for which upper envs an app has and, optionally, which
cluster each one lives on:

```yaml
deploy:
  lowerEnvironments: [dev]
  upperEnvironments:
    - staging                          # same-cluster (today's only previous behavior)
    - { name: prod, cluster: kind-prod-2 }  # hosted on a different cluster
```

A release step's own `env:` must name one of these; its optional `cluster:` (if set)
must agree with what the registry entry says - see `validateFlows` in
`charts/glidepath-app/templates/_helpers.tpl`. The cluster name itself resolves
against the control-plane chart's own `clusters:` registry
(`charts/glidepath-control-plane/values.yaml`, rendered as a ConfigMap by
`templates/clusters/cluster-registry.yaml`) - `cicd.yaml` never embeds infrastructure
details (gitops path, relay secret) directly, only the cluster's name.

For a same-cluster env (no `cluster:`), behavior is byte-identical to before this work:
`release-application.yaml`/`appproject.yaml` still directly render the `Application`/
RBAC objects on this cluster. For a cluster-mapped env, this cluster's chart renders
nothing for it at all - see the GitOps delivery section below (added once Phase D
lands).

**A real gap this closed**: before this session, a release step's `env` was required by
schema but never checked against `upperEnvironments` at all (unlike `deploy`, which
already had this check) - `cicd-flow-test-app`'s own real, live config had two release
flows using `env: staging` with `upperEnvironments: []`, silently relying on nothing.
Fixed both the check and that tenant's config.

## GitOps app-of-apps delivery (cluster-mapped envs)

For an env whose registry entry names a cluster, `open-release-pr.yaml` does two things
in the SAME commit/PR instead of one: the usual image-tag patch, plus writing/updating
that cluster's Application manifest at `<gitopsApplicationsPath>/<app-name>-<env>.yaml`
in the tenant's own gitops repo (resolved live from the `cluster-registry` ConfigMap via
a new, narrowly-scoped `get`-only Role - `charts/glidepath-app/templates/clusters/
read-registry-rbac.yaml`, only rendered for apps that actually have a cluster-mapped
env). This is the whole mechanism that keeps the dev-cluster pipeline from ever calling
the remote cluster's API: the target cluster's ArgoCD is bootstrapped once, out of band,
to watch that same `clusters/<cluster-name>/applications/` path, and picks up the change
on its own once the PR merges.

**Per-release tracking data does NOT live in the Application manifest's annotations**
(it did in an earlier version of this design - a real bug, not just a style choice, see
"What feeds the relay, and what didn't work first" below). The Application manifest is
purely structural (name/labels/spec, nothing that varies release to release). Per-release
data (`flowStartTime`/`gitUrl`/`gitRevision`/`configJson`) instead rides directly on the
outcome-reporting hook Jobs themselves - baked in as plain env vars by
`open-release-pr.yaml`, committed in the SAME commit as the image-tag patch, into
`<app-name>/<env>/` (the path the CHILD Application, not the root, actually watches) -
see the feedback-relay section below for the full mechanism.
`mark-release-pending.yaml`'s live `kubectl annotate` (the same-cluster mechanism) is
still a deliberate no-op for a cluster-mapped env - see that Task's own `cluster` param
- these hook Jobs are what replaces it.

**Historical, no longer live**: `deployment.yaml` used to also carry a small set of
operator-visibility tracking annotations (added 2026-08-11, at the user's request) -
`hangar.io/dora-{git-revision,image,flow-start-time,gitops-pr-url}` -
(`kubectl describe deployment` on the live upper-env cluster, no cross-referencing
pipeline logs needed to see which build produced what's running). That mechanism doesn't
exist any more: it targeted a raw `deployment.yaml`, from before releases moved onto the
`airframe-application` Helm chart's `<cluster>/<env>/values.yaml`, and was never ported over -
this doc simply went stale rather than the feature being deliberately dropped. Because
`gitops-pr-url` was one of those annotations, it was also the reason (alongside the
release-tracking hook-Job fields below) that `open-release-pr.yaml` used to push a SECOND
commit to every release PR, after opening it, once the PR's own URL existed. That second
commit was removed 2026-08-31 - see "pr-url/pr-created-at don't round-trip through git"
below - since it meant every governance gate on the gitops repo re-ran in full a second
time on every single release, for zero informational gain (the two commits never
differed in anything those gates inspect).

A real, pre-existing gap surfaced and fixed while building this: `app-type`
(platformIdentity.type, needed for the `<app-type>-<app-name>-<env>` namespace
convention `deploy.yaml` already relies on) was never threaded through
`deliver-onboarding-files.yaml`'s git-rooted PipelineRun generation at all, for any
stage - only the event-chained path (`flow-triggers.yaml`) had it. A git-rooted `deploy`
or a cluster-mapped git-rooted `release` would have hit a missing-required-param
admission error the first time either was actually exercised. Fixed by threading
`app-type` through the same `<APP_TYPE>` substitution mechanism `<APP_NAMESPACE>`/
`<APP_NAME>` already use (`onboarding-resync.yaml` catalog Pipeline, the app-repo
onboarding-resync.yaml template, and deliver-onboarding-files.yaml's own sed loop).

Known, deliberate gap in this pass (not hidden): the GitOps-delivered Application uses
`project: default` (ArgoCD's built-in, unscoped project) rather than also delivering a
matching scoped `AppProject` to the remote cluster - the same-cluster path's narrower
scoping (`appproject.yaml`) isn't replicated there yet.

## The feedback relay (Phase E)

`platform/broker/cmd/argocd-outcome-relay` - a small Go HTTP service in
`platform-system`, sibling to `token-review-interceptor`, exposed via a fixed NodePort
(`30880`) Service since it's the one endpoint in this platform genuinely called from
outside the cluster. `POST /outcome/<cluster>` with `Authorization: Bearer <token>`:

1. Resolves `<cluster>` against the `cluster-registry` ConfigMap -> `relaySecretName`,
   reads that Secret live, compares (constant-time) against the bearer token. This is
   the real trust boundary for this path - proves "a legitimate caller for cluster X",
   not "this event is really about app Y" (the payload's claimed app/env rides on top,
   unverified - which apps can even run a hook Job on cluster X in the first place is
   already gated by Phase D's PR-review flow).
2. Reshapes the request body directly into a real CDEvent
   (`dev.cdevents.environment.deployed.0.1.0`, a new event type - matches
   `catalog/lib/cdevents.sh`'s exact JSON shape) and POSTs it to the existing,
   **unmodified** broker using its own in-cluster projected SA token - the same
   mechanism `send-cdevent.yaml` already uses. The broker's TokenReview interceptor,
   CEL filters, and every existing Trigger are untouched.
3. Also calls the DORA exporter's new `/argocd-outcome` endpoint directly (Phase F
   below) - not routed through the broker/a PipelineRun, matching
   `docs/dora-metrics.md`'s original reasoning for keeping DORA off the
   CDEvents-subscriber model.

Everything in the request body is trusted as-is - there's no fetch, no live cluster
read, nothing to reconcile against. That's not the simple version of a design that got
complicated elsewhere; it's the end state after two real, live-caught problems in
earlier versions ruled out the alternatives - worth knowing before touching this again.

## What feeds the relay, and what didn't work first

**Caller: ArgoCD sync hooks, not ArgoCD Notifications.** The first working version used
ArgoCD's built-in Notifications controller (`selector: hangar.io/dora-track=true`,
trigger on `operationState.phase`) to call the relay. Live testing (deliberately
adversarial, at the user's request before committing to either direction) found a real,
already-shipped bug: **Notifications fired on ANY completed sync operation, including
pure selfHeal drift-correction with zero release involved.** Manually scaling a
Deployment on `kind-prod` (bypassing git entirely) triggered a genuine, real call to the
relay for an app that had never actually been released - confirmed via the relay's own
logs, not inferred. `oncePer: app.status.operationState.finishedAt` dedupes *redelivery
of the same finishedAt*; it does not distinguish *why* a new finishedAt happened, and
selfHeal produces one on every drift correction, same as a real release does.

The replacement: ArgoCD **sync hooks** - two Jobs, `PostSync` (fires on success) and
`SyncFail` (fires on failure), annotated `argocd.argoproj.io/hook-delete-policy:
BeforeHookCreation`, committed by `open-release-pr.yaml` into `<app-name>/<env>/`
alongside `deployment.yaml` - i.e. the path the CHILD Application (not the app-of-apps
root) actually watches. Confirmed live, with a purpose-built isolated test
(a Deployment with an artificial ~25s startup delay, so health convergence is clearly
separated in time from "manifest applied"):

- `PostSync` genuinely waits for health, not just apply-success - the hook Job's pod
  appeared in the exact same poll tick `status.health.status` became `Healthy`, and
  `operationState.phase` didn't reach `Succeeded` until *after* that (the sync
  operation as a whole doesn't complete until the hook phase does).
- Hooks are scoped to "did this specific resource need reapplying," not "did any sync
  happen." The same drift-correction test that spuriously fired the old Notifications
  trigger did NOT re-create the PostSync hook Job (verified by the Job/pod's own
  creation timestamp staying unchanged across the drift-correction sync) - ArgoCD only
  reprocesses hooks tied to resources that actually had a diff to apply, and the hook
  manifest itself hadn't changed.

So the hook design fixes both problems the Notifications design had - not just the one
it was originally chosen to fix (an earlier, GitHub-Contents-API-fetching version of
this relay existed specifically to solve a *different* real bug, a root/child
Application sync race - see below - and turned out unnecessary once hooks removed the
need to trust anything about the Application object's state at all).

**Why not just read the ConfigMap/Deployment's own annotations from the notification
template**: considered (twice - once for a dedicated ConfigMap, once for putting the
data directly on the Deployment). Neither works: ArgoCD Notification templates only
expose `.app.status.resources[]` for managed resources, which is limited to
`group/kind/namespace/name/status/health` - never `metadata.annotations` or `data`, for
any resource kind. The only way to surface arbitrary resource data into that struct at
all is a custom `resource.customizations.health.<kind>` Lua script in `argocd-cm`
(global, cluster-wide ArgoCD config) that smuggles it through `health.message` - doable
for a ConfigMap (which has no built-in health check, so customizing it doesn't affect
anything else), but doing the equivalent for `apps/Deployment` would override the REAL,
load-bearing health check every Deployment on the cluster gets evaluated with. Moot
either way once hooks removed the need for the notification template to see any of this
in the first place.

**Everything the hook Jobs report is baked in by `open-release-pr.yaml` at commit
time** - `APP_NAMESPACE`/`APP_NAME`/`ENV`/`GIT_URL`/`GIT_REVISION`/`FLOW_START_TIME` as
plain env vars, `CONFIG_JSON_B64` (base64, the one field that's an arbitrary JSON blob
rather than a flat string - sidesteps any risk of the printf-per-line manifest
generation mis-escaping it) the same way. `PHASE` is baked in per-hook-type
(`Succeeded`/`Failed`) rather than discovered live - which hook ran already tells you
the outcome. `CHAIN_ID` (this flow's own chain-id, threaded from `release.yaml`'s
`start-flow` result - see "The outcome span" below for what it's for) joined this list
2026-08-12, optional (`:-`, not `:?`, in the hook script - an app onboarded before it
existed must keep releasing without it). The hook script itself
(`catalog/lib/argocd-outcome-hook.sh`, baked into the toolbox image, not embedded in the
generated Job YAML - avoids re-hitting the same YAML-block-scalar-indentation class of
bug documented below) does exactly two live things: read a small per-app,
hand-provisioned `platform-outcome-relay-token` Secret in its OWN namespace (RBAC
delivered alongside the hooks, see `platform-outcome-rbac.yaml`), and `curl` the relay -
`RELAY_URL` itself also baked in, sourced from the cluster registry's new
`outcomeRelayURL` field.

**`pr-url`/`pr-created-at` don't round-trip through git, unlike everything else above**
(changed 2026-08-31). They briefly joined the baked-in-fields list the same way
`CHAIN_ID` did (`PR_CREATED_AT` on 2026-08-12), each committed via a second commit pushed
right after the PR opened - the PR's own URL obviously can't be known before it exists.
That worked, but it meant every governance gate on the gitops repo (`sast`/`sbom`/
`image-scan`/`provenance`/etc., see `docs/admin/release-guardrails.md`) independently
re-verified the PR's HEAD a second time on every single release, since per-commit
verification is deliberate (see `pull-request-sast.yaml`'s own CEL comment) - doubled
Check Runs and doubled real work (e.g. `image-scan`'s live Trivy re-scan) against an
image/commit that never actually changed between the two commits. Fixed by dropping the
second commit entirely: `open-release-pr.yaml` now writes `pr-url`/`pr-created-at` to a
short-lived `release-tracking-<chain-id>` ConfigMap on the DEV cluster (the same cluster
that Task already runs on - never a cross-cluster call) instead, and
`release-outcome-notify.yaml`'s `resolve-release-tracking` Task reads (and deletes) it by
chain-id when the outcome CDEvent eventually arrives, rather than expecting the CDEvent
itself to carry those two fields. `pr-namespace-ttl-sweep` is the backstop for a release
whose PR never merges (so that Task never runs to consume/delete the ConfigMap).

**A new per-app Trigger** (`charts/glidepath-app/templates/triggers/
release-outcome-trigger.yaml`, rendered only for apps with a cluster-mapped upper env)
consumes the relay's forwarded CDEvent and fires `release-outcome-notify`
(`charts/glidepath-catalog/templates/pipelines/release-outcome-notify.yaml`) - real
Slack notifications for a confirmed cluster-mapped release outcome, not just a CDEvent
landing unseen. Two independent tasks as of 2026-08-12 (see "The outcome span" below for
the second one, added that day - before it, this Pipeline really was the single
`notify-slack` wrapper this paragraph used to describe). Its CEL
filter can't use `extensions.app_namespace` the way every Trigger in
`flow-triggers.yaml` does (the caller is always the relay's own `platform-system`
identity, not the target app's) - it matches on the event's own claimed
`body.subject.content.appNamespace` instead, a deliberate, narrower trust boundary than
the TokenReview-verified path, documented in both the relay's and the Trigger's own
headers. This piece was never affected by the Notifications-vs-hooks change - the
CDEvent shape the relay produces stayed identical throughout.

**The Slack message for a confirmed cluster-mapped outcome links back to its GitOps PR**
(added 2026-08-11) - `pr_url` (resolved once `open-release-pr.yaml` actually opens the
PR) rides the same path as everything else: hook Job env var -> hook script's payload ->
relay -> CDEvent's `subject.content.prUrl` -> the Trigger's binding -> a `pr-url` param
on `release-outcome-notify` -> `notify-slack.yaml`'s own `pr-url` param, which takes
priority over that Task's existing live-TaskRun-lookup path (which only works for
`release` itself, since `release-outcome-notify` runs in a completely different
PipelineRun than the one that has `open-release-pr` as a sibling TaskRun to look up).

## The outcome span (added 2026-08-12)

Slack was the only thing `release-outcome-notify` did until a real gap was found live:
this Pipeline had **no tracing task at all**, so a confirmed cluster-mapped outcome was
invisible in Tempo/Grafana - only reachable by pasting a trace ID by hand once you
already somehow knew one existed. `catalog/tasks/release-outcome-span.yaml` closes that
gap, but not by extending the original flow's trace - see docs/tracing.md's "Release
outcome: a deliberately separate trace" for the full reasoning (short version: the
flow-root span is already closed by the time a human merges this PR, see that doc's
"Which stage closes the flow-root span"; appending a child span here, potentially days
later, would reproduce the exact "span after root closed" bug already fixed there).

**Why the span's start-time is `pr-created-at`, not `flow-start-time`** - a real design
change made at the user's own request ("connect the PR release and the release outcome
so we can see how long it takes between creating the PR and application deployment").
`flow-start-time` (commit time) was the original anchor, but that means this span's own
duration double-counted this platform's own automated build/test/deploy/release
execution time - already fully visible as the `stage:release` span's own duration in the
ORIGINAL flow trace (docs/tracing.md). Anchoring on `pr-created-at` instead isolates the
actually-interesting, previously-invisible segment: how long the human-review-plus-
ArgoCD-sync gap itself took, with no overlap against what the flow trace already shows.
`flow-start-time` is still carried as a span *attribute* (`platform.flow_start_time`),
not thrown away - anyone wanting full commit-to-deploy DORA lead time instead can still
derive it from there, it's a strict superset of this span's own start/end.

**Correlation back to the original flow is via chain-id, not trace-id** - the two
always live in different Tempo traces (this span's own, deliberately standalone), so
there's no structural link between them the way a parent/child span relationship would
give. `chain-id` (CDEvents' own causal-sequence correlator - see docs/tracing.md's file
header on `otel.sh`) rides the exact same path `pr-created-at` does (hook Job env var ->
hook script -> relay -> CDEvent -> Trigger binding -> Pipeline param), and lands on the
span as the `platform.chain_id` attribute - `{ platform.chain_id = "..." }` against
Tempo directly (not through the `CICD Variant 1` dashboard) surfaces both the original
flow's spans and this standalone one together, even though they're structurally
unrelated traces. **Not surfaced as a dashboard column** - tried exposing it via
Grafana's table "nested" field (the only place Tempo's search API puts a matched span's
own attributes that aren't already flat trace-level fields), found live 2026-08-12 that
its actual field type is `other`/`json.RawMessage` with no `cellOptions` this session
could find that renders it as anything but raw JSON text in the cell (real Grafana rough
edge, not a misconfiguration - see `grafana/grafana#100032`). Reverted rather than ship
a panel showing raw JSON; the outcome (`Succeeded`/`Failed`) itself is still shown
cleanly, baked directly into the release-outcome span's own name instead (a real, flat
field, no nested-frame rendering involved) - see this section's own "outcome span"
naming, `release-outcome:<app>/<env> [<status>]`.

## Relay-token distribution via External Secrets Operator (built 2026-08-19)

**Was deferred, now built** - the thing this was waiting on ("package this platform's
k8s-app delivery as a proper Helm chart") is exactly what `airframe`'s
`airframe-application` chart now is. Every app onboarded to a cluster-mapped env used to need
its own hand-provisioned `platform-outcome-relay-token` Secret (`hack/
bootstrap-upper-cluster.sh`'s old per-app step) - the same shared per-cluster token
value, copied by hand into every app's namespace on that cluster.

Now: the upper cluster (kind-prod) has its own Infisical-backed `platform-secret-store`
(`gitops-cluster-kind-prod/10-crds-operators/external-secrets/`, mirroring
platform-cicd's own on kind-dev - see [secrets-management.md](secrets-management.md)),
and `airframe-application`'s own `templates/release-tracking/
relay-token-external-secret.yaml` syncs `platform-outcome-relay-token` from it
automatically, gated on `releaseTracking` exactly like the hook Jobs/RBAC it
accompanies. The token value still has to be planted into that cluster's own Infisical
project by hand (once per cluster, not per app) - what's gone is the N-times-per-app
manual Secret copy `hack/bootstrap-upper-cluster.sh` used to require.

## The DORA exporter (Phase F)

`platform/dora-exporter` gained a second input path, **not a replacement** for its
original one (an earlier draft of this plan said "replace" - wrong, caught while
implementing: same-cluster Applications still live on THIS cluster and the informer is
still the right, working mechanism for them). Both paths now funnel into one shared
`recordOutcome()`:

1. **Same-cluster** (unchanged): the `applications.argoproj.io` informer, still reading
   the `hangar.io/dora-baseline-started-at` annotation to tell "the sync I'm waiting
   for" apart from an unrelated selfHeal sync.
2. **Cluster-mapped** (new): `POST /argocd-outcome`, called directly by the relay,
   which the outcome hook Jobs feed. No baseline check needed here - a hook only runs
   as part of the sync it's actually reporting on, so there's no "which sync is this"
   ambiguity to resolve after the fact the way the same-cluster path has to.

**Known, deliberate gap**: MTTR (`dora_time_to_restore_seconds_experimental`) isn't
tracked for cluster-mapped apps in this pass - path 1 persists a "last failure time"
annotation back onto the live Application object between calls; path 2 has no such
object on a remote cluster to persist it onto, and an in-memory map in the exporter
would be wrong the moment it runs more than one replica or restarts. Not worth that
fragility for an already-`_experimental` metric - `recordOutcome()`'s own comment has
the full reasoning.

## Live end-to-end verification, v1 - ArgoCD Notifications (2026-08-10, superseded)

Kept as an honest record of what was actually tested, even though the mechanism it
verified was replaced shortly after (see "What feeds the relay, and what didn't work
first" above) - the plumbing downstream of the relay (CDEvent shape, the broker, the
Trigger, DORA) is identical either way, so this run is still real evidence for that half
of the path.

Ran the complete loop for real against `kind-prod` and the `cicd-flow-test-app` tenant:
a real `release` PipelineRun (`env: staging`, `cluster: kind-prod`) opened a real PR
against `gitops-cicd-flow-test-app` containing both the image-tag patch and the new
Application manifest; merged; the app-of-apps root on `kind-prod` picked it up; ArgoCD
synced it; ArgoCD Notifications fired with a correctly-shaped payload; the relay
authenticated and forwarded to both the broker and `dora-exporter`; the new per-app
Trigger fired `release-outcome-notify`, which completed successfully; `dora_deployments_total`/
`dora_releases_total` incremented for real. Confirmed via direct Prometheus/`kubectl`
inspection at every hop, not inferred from one end to the other.

**Two real bugs found and fixed only by actually running this live** - `helm template`
had caught neither:

1. **A YAML block-scalar indentation bug in `open-release-pr.yaml`**: the first draft
   embedded the Application manifest via a raw heredoc at column 0, inside a Task
   `script: |` block already indented 8 spaces from prior lines - YAML terminated the
   block scalar early at the first under-indented line, corrupting the surrounding
   Task's own YAML (`helm lint` caught this one before it ever reached a live cluster:
   "could not find expected ':'", pointing at unrelated later lines). Fixed by building
   the manifest via `printf '%s\n'` with one quoted line per arg instead - this file's
   own `commit_message`/`governance_text` already used exactly this pattern for the
   identical reason, documented in their own comments before this session even started.
2. **An event-id collision in `argocd-outcome-relay`, live-cluster-only** (a
   `helm lint`/unit-test could never have caught this - it's a matter of which byte
   values two DIFFERENT live events over TIME happen to hash to): `context.source` has
   no per-attempt uniqueness (a fixed string per app/env/cluster, unlike a real
   PipelineRun's own unique name), so the CDEvents `id` computed from `source+eventType`
   collided across every distinct outcome for the same app/env - only the FIRST ever
   release-outcome created a `release-outcome-notify` PipelineRun; every later one
   silently no-op'd as "already exists" at the Trigger's own admission step (Kubernetes
   correctly rejecting a duplicate name, doing exactly what it was asked). Confirmed
   live: `dora_deployments_total` kept incrementing correctly on every call (it doesn't
   dedup by pipeline-run-name), while `kubectl get pipelinerun -l
   hangar.io/subcomponent=release-outcome` stayed stuck at one, exposing the
   mismatch. Fixed by folding `finishedAt` into the id - each genuinely distinct sync
   outcome now produces a distinct id, while true redelivery of the SAME outcome (which
   would carry the same `finishedAt`) stays idempotent, matching ArgoCD Notifications'
   own `oncePer` key.

**Also live-only findings, not code bugs**: forgetting to actually `helm upgrade` the
catalog chart after editing it (every `helm template`/`lint` check had been dry-run
only) meant the first live test ran against the OLD `release`/`open-release-pr`
definitions with none of this feature's params - caught by the promotion silently
missing the Application-manifest file entirely. And re-`kubectl apply`-ing
`platform/argocd-notifications/kind-prod.yaml` (which, in its first version, declared
`argocd-notifications-secret` inline with a `REPLACE_ME` placeholder) silently
overwrote an already-correctly-set real token back to the placeholder - the manifest no
longer declares that Secret at all, see its own header for the fix and the general
lesson (an idempotently-reapplied manifest must never contain a real secret value it
doesn't own the lifecycle of).

## The ArgoCD hook-timing test (2026-08-10)

Before committing to the sync-hook redesign, verified the two things it depends on with
an isolated, throwaway test (a standalone Application + Deployment with an artificial
~25s startup delay, plus a trivial `PostSync` hook Job - `hook-timing-test/` in
`gitops-cicd-flow-test-app`, deleted afterward, not part of any real app's flow):

- **Does `PostSync` actually wait for health, or just apply-success?** Polled
  `operationState.phase`/`status.health.status`/the hook Job's own pod existence every
  2s through a real sync. `health` and the hook Job's pod appeared in the SAME poll
  tick (~t=24s, matching the artificial delay); `operationState.phase` didn't reach
  `Succeeded` until the NEXT tick after that. Confirms the sync operation as a whole
  doesn't complete until the hook phase does - `PostSync` genuinely waits for health.
- **Does a hook re-fire on a sync that didn't actually need it?** Caused real drift
  (`kubectl scale` directly on the live Deployment, bypassing git) and watched selfHeal
  correct it - a genuinely new `operationState.finishedAt` appeared, but the `PostSync`
  Job's pod creation timestamp stayed unchanged (same instance from the original sync,
  never recreated). Then, to make the comparison concrete rather than theoretical,
  labeled the SAME test Application with `hangar.io/dora-track: "true"` (what the
  *real*, then-still-deployed Notifications subscription matched on) and repeated the
  drift - the real `on-platform-cicd-outcome` trigger fired for real, calling the real
  relay, for a sync that had nothing to do with a release (confirmed via the relay's
  own logs: a real request, failing only because this throwaway app had no
  `platform-release-meta.yaml` to fetch). This is what settled the decision - not just
  "hooks avoid a hypothetical problem," but "here is that exact problem, live, in what
  was already shipped."

## Live end-to-end verification, v2 - ArgoCD sync hooks (2026-08-11)

Decommissioned the v1 (ArgoCD Notifications) config on `kind-prod` for real - removed
the `service.webhook.*`/`subscriptions`/`template.*`/`trigger.*` keys from
`argocd-notifications-cm` and deleted `argocd-notifications-secret` outright (confirmed
the notifications controller stayed healthy and picked up both changes via its own
cache-invalidation logs, no restart needed) - then re-ran the complete loop against the
final hook-based design: a real `release` PipelineRun opened a real PR containing the
image-tag patch plus the three new hook/RBAC manifests (`platform-outcome-rbac.yaml`,
`platform-outcome-postsync.yaml`, `platform-outcome-syncfail.yaml`); merged (branch
protection required a human admin-override merge - the repo's required status checks
are wired to a CI that was never configured to run against this gitops repo, so they can
never be satisfied automatically; approval-from-non-pusher is a real, working gate,
admin-override is the correct way past a check that can structurally never pass, not a
gap to close here); picked up by the app-of-apps root; synced by ArgoCD. Confirmed live:
`PostSync` fired only after `status.health.status` reached `Healthy` (operationState sat
in `Running` through the health transition, exactly matching the hook-timing test's
prediction), reported to the relay, and drove a real `release-outcome-notify`
PipelineRun to completion. A **second**, unplanned but equally real exercise of the
`SyncFail` path happened in the same session (see bug #1 below): once fixed, forcing a
fresh sync produced a genuine `SyncFail`-triggered report (`phase=Failed`) that also
made it all the way through - confirmed via `dora_releases_total{outcome="failed"}`
incrementing alongside `{outcome="succeeded"}`, and both `release-outcome-notify`
PipelineRuns' own `status` param carrying the correct phase. This is the first time
both hook variants were confirmed live in the same real app's namespace, not just the
throwaway hook-timing-test app.

**One real bug found only by running this live** - a genuine bash parser gotcha, not an
RBAC/logic/typo issue, and `bash -n`/`helm lint` both passed cleanly on the broken
version:

`catalog/lib/argocd-outcome-hook.sh` had `: "${POD_NAMESPACE:?...see the Job's own
env}"` - a lone apostrophe inside a double-quoted `${VAR:?message}` guard. Bash's
brace-matching scanner for `${...}` parameter expansions tracks quote state
independently of the surrounding double-quote context (needed so a `}` inside a
single-quoted `message` doesn't prematurely end the expansion) - so that one apostrophe
opened an unterminated single-quoted region that swallowed everything up to the *next*
apostrophe in the file, several lines later inside `-o jsonpath='{.data.token}'` on the
`token=$(kubectl get secret ...)` line. Bash silently reparsed the two lines in between
as one garbled no-op (`:` with a mangled string argument) instead of the real assignment
- `token` was never actually set, so the later `${token}` reference under `set -u` died
with "unbound variable", but only *after* the script had already printed its "reporting
to..." line, making the failure look like a mid-request curl problem rather than a
parse-time one. Confirmed by extracting the two lines into a standalone repro and
watching `set -x` show the exact swallowed span; fixed by rephrasing the message to
avoid the apostrophe. Swept the rest of the repo (every `${VAR:?...}`/`${VAR:-...}` in
`catalog/`, `charts/`, `hack/`, `platform/`) for the same pattern - this was the only
occurrence. Worth remembering for any future guard-clause message in this codebase:
contractions ("Job's", "doesn't", "can't") are unsafe inside `${VAR:?message}` even
though the whole expression is double-quoted.

## Status

Phases A through F are done, live-deployed, and live-verified end to end against the
real `kind-prod` cluster and the `cicd-flow-test-app` tenant - not just code-complete.
The design went through two full live-verified iterations: v1 (ArgoCD Notifications,
2026-08-10) found and fixed the root/child sync race; v2 (ArgoCD sync hooks, 2026-08-11,
current) replaced Notifications entirely after live testing showed it also fired on
selfHeal drift-correction with no release involved, and confirmed hooks don't share that
flaw. v1's config is fully decommissioned from the live `kind-prod` cluster, not just
superseded in the repo.

A follow-up increment the same day added the deployment.yaml tracking annotations and
the Slack PR-link (both above), which needed the two-commit restructuring of
`open-release-pr.yaml`. Also live-verified: a real release produced a PR with the
expected two commits in order (release change, then outcome-reporting artifacts once the
PR URL was known), the live Deployment on `kind-prod` carried all four
`hangar.io/dora-*` annotations with correct values including the real PR URL, and
`release-outcome-notify`'s own `notify-slack` TaskRun actually posted (not skipped) with
`pr-url` resolved correctly through the full relay/CDEvent/Trigger chain.

Historical as of 2026-08-31: the deployment.yaml annotations described here didn't
survive the later migration onto the `airframe-application` Helm chart (see the "Historical,
no longer live" note above), and the two-commit restructuring itself was removed the
same day the redundant-check-run cost it caused was found - see "pr-url/pr-created-at
don't round-trip through git" above for the replacement mechanism. `pr-url` still
resolves correctly through `notify-slack`/`release-log-emit`, just via a ConfigMap now,
not a second commit.

Remaining, explicitly deferred: self-service onboarding tooling for additional
tenants/clusters, real TLS/ingress hardening for the relay (same-host podman
reachability is what's actually verified), and a real second env/cluster beyond this one
proof. Relay-token distribution onto External Secrets Operator - deferred at the time
this section was written - is now built, see "Relay-token distribution via External
Secrets Operator (built 2026-08-19)" above.

**2026-08-12 follow-up** (see "The outcome span" above for the full design): closed a
real gap found live - `release-outcome-notify` had never had a tracing task at all.
`chain-id` and `pr-created-at` now ride the same hook-Job/relay/Trigger path
`flow-start-time`/`config-json`/`pr-url` already established, feeding a new, real
standalone span (`release-outcome-span.yaml`) live-verified end to end: fired a
synthetic outcome directly at the relay with `chainId`/`prCreatedAt` set, confirmed both
values reached the `release-outcome-notify` PipelineRun and its `span` TaskRun, and
confirmed via a direct Tempo query that the resulting span's real start time was exactly
`prCreatedAt`, not `flowStartTime` - not just that the Pipeline completed successfully.

## Outcome reporting, rebuilt (2026-08-17)

`open-release-pr.yaml`'s 2026-08-16 migration to `<cluster>/<env>/values.yaml` (see the
top-of-file note) retired the GitOps app-of-apps delivery mechanism above wholesale -
including the outcome-hook Jobs it used to write, since they lived inside the same
now-removed Application manifest. Everything downstream of the hook Jobs (the relay, the
`release-outcome-*` Pipeline/Trigger/span, cluster-mapped DORA) stayed real code the
whole time, just structurally unreachable with nothing calling it.

**Design constraint that ruled out the obvious alternative**: could the dev cluster just
read the upper cluster's `Application.status` directly instead of waiting for a push? No
- that's exactly the "extend ArgoCD on the dev cluster to manage a remote cluster"
approach rejected at the top of this doc, for the same reason: it would mean a
dev-cluster-resident credential capable of touching the upper cluster, the blast-radius
shape this platform's broker/impersonation model was built to avoid. The only sanctioned
crossing stays a reviewed git commit (dev -> upper) plus a hook Job pushing over HTTP
with a shared secret (upper -> dev, the relay) - never a live API call from dev to an
upper cluster.

**The fix: relocate the hook Jobs, not their trust model.** The two ArgoCD sync-hook
Jobs (`PostSync`/`SyncFail`) are unchanged in shape and behavior from the "What feeds the
relay" section above - same `catalog/lib/argocd-outcome-hook.sh` script, same
`hook-delete-policy: BeforeHookCreation`, same env-var payload. What changed is who
renders them:

- **Before**: `open-release-pr.yaml` wrote a second, competing Application manifest
  (`<gitopsApplicationsPath>/<app-name>-<env>.yaml`) purely to carry the hook Jobs
  alongside it, race-prone against idp's own ApplicationSet-owned Application for the
  same path.
- **Now**: `airframe/charts/airframe-application` renders the hook Jobs itself
  (`templates/release-tracking/hooks.yaml`+`rbac.yaml`), gated behind a `releaseTracking:`
  values block that's `null` by default - see that chart's own `values.yaml` header.
  They render as part of the SAME Application idp's tenant-onboarding `ApplicationSet`
  already owns. No second writer, no race.

`open-release-pr.yaml` patches that `releaseTracking:` block into the SAME
`<cluster>/<env>/values.yaml` commit it already writes `rollout.image.*` into - well, a
SECOND commit on the same PR branch, pushed right after the PR opens, for the same
reason as before: `prUrl`/`prCreatedAt` don't exist until the PR does. `outcomeRelayURL`
is resolved live from platform-cicd's OWN cluster-registry ConfigMap (`platform-system`)
via a re-added, narrowly-scoped Role (`charts/glidepath-app/templates/clusters/
read-registry-rbac.yaml`, get-only, this ConfigMap only) - `gitopsApplicationsPath`, the
other field that ConfigMap used to carry, was dropped from the schema entirely since
nothing writes an Application manifest from it any more.

**Also fixed while rebuilding this**: the control-plane's own `clusters:` registry
(`charts/glidepath-control-plane/values.yaml`) had gone back to empty (`[]`) at some
point after platform-cicd's control plane moved onto `kind-dev` as its own, independent
instance - confirmed live, not assumed
(`clusters: []`, `data: null` on the real ConfigMap). Since `argocd-outcome-relay`'s own
Deployment/RBAC/Service are all gated behind `if .Values.clusters`
(`templates/clusters/argocd-outcome-relay.yaml`), the relay wasn't even deployed on
`kind-dev` at all - a second, independent reason cluster-mapped outcome reporting had no
working path, on top of the hook-Job removal. Re-populated with a real `kind-prod` entry
to fix both at once.

**2026-08-19 update**: `platform-outcome-relay-token` no longer needs hand-provisioning
per app namespace - see "Relay-token distribution via External Secrets Operator (built
2026-08-19)" above. At the time this section was written, it still did.

**2026-08-22 update: relay generified, hook script now builds the CDEvent itself.**
`argocd-outcome-relay` used to receive a flat, bespoke JSON request from the hook
script and reshape it into a CDEvent in Go (~80 lines duplicating `catalog/lib/
cdevents.sh`'s envelope shape, with no retry on the broker POST, unlike every other
call site in this codebase). Flipped: `catalog/lib/argocd-outcome-hook.sh` now builds
the full CDEvent envelope itself (own deterministic id, own outcome/status mapping
from `PHASE`, `curl --retry 3 --retry-connrefused` matching `cdevent_send`'s
resilience) and POSTs that as-is. The relay's job shrank to what its header comment
always said the trust model required: authenticate the shared secret, then forward
those exact bytes to the broker with its own SA token - no reshaping.

One field is still checked, not blindly forwarded: `subject.content.cluster` must
match the `<cluster>` the URL path (`/outcome/<cluster>`) authenticated against. Every
other field remains a self-asserted claim by the hook script, same trust boundary as
before - but cluster identity is exactly what the shared-secret lookup proves, so it's
the one place a mismatch is rejected (400) rather than trusted at face value. This
needed a new field threaded through: `releaseTracking.cluster`, set by
`open-release-pr.yaml` from the same `${CLUSTER}` param it already resolves
`outcomeRelayURL` from, consumed by `airframe`'s `hooks.yaml` as a new
`CLUSTER` env var on both hook Jobs.

**Also done in this same pass**: the relay's direct `forwardToDoraExporter` HTTP call
(Phase F) is gone. `dora-exporter`'s `/argocd-outcome` endpoint is now called by a new
Task, `update-dora-metrics.yaml`, run from `release-outcome-notify`'s own PipelineRun
alongside `notify`/`span` - a failure there is now a real, visible TaskRun failure
instead of a silently-logged-and-dropped side call. Doing this also closed the
cluster-mapped MTTR gap: `dora-exporter` got its own state store on its own cluster
(`dora-cluster-mapped-state` ConfigMap, `platform-system`) instead of needing API
access to a remote cluster's Application object - see "MTTR for cluster-mapped envs"
in [dora-metrics.md](dora-metrics.md) for the full mechanism.

**Deploy steps for this change**: git-side is done - `platform-cicd`/`idp-service-
catalog` (tagged `v0.3.37`) both committed+pushed, `targetRevision` repointed in every
tenant-onboarding `ApplicationSet` that pins it (`gitops-cluster-dev`,
`gitops-cluster-kind-prod`, and `gitops-cluster-template` for future clusters) -
`airframe-application`'s own `Chart.yaml` `version:` field is decorative here (confirmed via
`git show <tag>:...Chart.yaml`, stayed `0.3.0` across 36+ real tags) since these charts
are consumed straight from a pinned git tag, not a packaged/published chart repo.

**Still not done**: rebuild+push three images - `ghcr.io/jfillman/platform-cicd-toolbox`
(bakes in the hook script AND `update-dora-metrics.yaml`'s step image),
`ghcr.io/jfillman/platform-cicd-argocd-outcome-relay`, and
`ghcr.io/jfillman/dora-exporter` (all `:latest`, all `imagePullPolicy: IfNotPresent` -
a stale node-cached image has bitten this exact relay before, see the
"ghcr.io/jfillman" comment in `argocd-outcome-relay.yaml`) - and a rollout restart of
the `argocd-outcome-relay` and `dora-exporter` Deployments afterward, not just a
re-apply. Safe to do in any order or partially: the old hook script sends the old flat
request shape the old relay still expects, and the old relay's own direct dora-exporter
call still works against the old dora-exporter binary - nothing breaks until all three
images are rebuilt, and nothing new activates until they are.

**Live-verified end to end, 2026-08-17**, against the real `checkout-api` tenant on the
real `kind-prod` cluster - not just `helm template`. A real commit to `checkout-api`
(`main`) ran the full `build -> test -> deploy(dev) -> release(prod)` chain;
`open-release-pr.yaml` opened a real two-commit PR against `gitops-checkout-api`
(image-tag patch, then `releaseTracking:`) - confirmed via `gh pr diff` that the second
commit's `releaseTracking:` block held real, non-placeholder values (`chainId`,
`flowStartTime`, `prCreatedAt`, and `configJsonB64` decoding to the real `cicd.yaml`).
Merging it synced on `kind-prod`; both hook variants fired for real:

- **`SyncFail`** fired repeatedly while `checkout-api`'s own container crash-looped
  (unrelated app-level liveness-probe issue, not this mechanism - see below) - 8
  real, distinct `release-outcome-notify` PipelineRuns succeeded, `notify-slack` logged
  `ok` (a real post, not a skip) each time, `dora_releases_total{outcome="failed"}`
  reached 8.
- **`PostSync`** fired once `checkout-api` was scaled to 0 (trivially Healthy) - one more
  `release-outcome-notify` PipelineRun succeeded, and both `dora_deployments_total` and
  `dora_releases_total{outcome="succeeded"}` incremented to 1.

Confirmed via `argocd-outcome-relay`'s own code that it logs nothing on a fully
successful request (only on auth/forward failure) - empty relay logs during this test
were a red herring, not evidence the mechanism wasn't firing; the real proof is the
PipelineRuns/Slack/metrics above, plus a manual debug-pod run of the exact hook script
(`platform-outcome-hook` ServiceAccount identity) that confirmed the RBAC/secret-read
path works in isolation too.

**Three separate, real infra gaps found and fixed getting here** (none caused by this
session's code changes, all pre-existing on `kind-prod`):

1. `kind-prod` had no `rollouts.argoproj.io` or `monitoring.coreos.com/v1` (ServiceMonitor)
   CRDs at all - `hack/bootstrap-upper-cluster.sh` only ever installed ArgoCD there.
   Installed Argo Rollouts (the exact pinned manifest `gitops-cluster-dev/
   10-crds-operators/argo-rollouts/install.yaml` already vendors) and just the
   ServiceMonitor CRD (extracted from the same `kube-prometheus-stack` chart
   version `40-observability` pins, not the full stack - `kind-prod` still has no
   Prometheus of its own).
2. The control-plane's own `clusters:` registry had gone back to `[]` (and
   `argocd-outcome-relay` - gated behind `if .Values.clusters` - wasn't even deployed)
   at some point after platform-cicd's control plane moved onto its own, independent
   `kind-dev` instance. Re-populated with a real `kind-prod` entry (see this doc's own
   "Outcome reporting, rebuilt" section above).
3. `ghcr.io/jfillman/checkout-api` is a private package and `kind-prod` has no
   ExternalSecret mechanism (no ESO installed) - fixed with a hand-provisioned
   `ghcr-pull-secret` `docker-registry` Secret, referenced via
   `serviceAccount.imagePullSecrets` directly in `kind-prod/prod/values.yaml` (committed,
   not live-patched - see the comment there).

**Separate, NOT fixed here**: `checkout-api`'s own container genuinely crash-loops on
`kind-prod` (exit 137, liveness probe on `/` never passes) - an application-level issue
in the `checkout-api` repo itself, unrelated to any of the above. Scaled to 0 in
`kind-prod/prod/values.yaml` as a stopgap so ArgoCD's periodic retry doesn't keep
generating real-but-redundant `SyncFail` Slack messages/DORA `failed` counts - remove
that override once the app side is fixed.
