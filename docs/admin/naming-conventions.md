# Naming conventions

Written 2026-08-06 after auditing real inconsistencies (not guessed) - see the specific
findings inline below. This is the durable reference; new catalog Tasks, Pipelines,
namespaces, and files should follow this rather than the nearest existing example, since
a few existing examples are exactly the inconsistencies this doc fixes.

## Namespaces

**One flat pattern: `<type>-<app-name>-<env>`.** `type` is `app` (a regular Application)
or `infra` (a shared/platform-adjacent service onboarded with its own pipeline - e.g. a
future shared DB operator) - more types added as real cases show up, not invented
speculatively. `env` is whichever environment that particular namespace represents -
`cicd` (the Application's own pipeline-execution namespace), `dev`, `staging`,
`pr-<number>`, or any other declared deploy target. All of these are **siblings under
the same pattern**, not a base-plus-suffix hierarchy - a deploy namespace has nothing to
do with "cicd" conceptually (it's where the Application *runs*, not where its pipeline
runs), so it is never `<type>-<app-name>-cicd-<env>`.

Examples, all structurally identical 3-part names: `app-nodejs-demo-app-cicd` (pipeline
execution), `app-nodejs-demo-app-dev` (deploy target), `app-nodejs-demo-app-staging`
(release staging), `app-nodejs-demo-app-pr-42` (PR ephemeral env),
`infra-payments-db-cicd` (an `infra`-type Application's own pipeline execution).

`charts/glidepath-app`'s `glidepath-app.envNamespace` helper computes any of
these from `platformIdentity.type` + `platformIdentity.appName` + a given `env` value -
nothing is a separately-set, independently-typed field that could drift from the
convention.

Watch the 63-character Kubernetes namespace limit (a real DNS-1123 constraint, not a
style preference) on longer app names combined with the `-pr-<number>` suffix.

**Platform-level namespaces** keep the existing `platform-*` prefix - already consistent,
not changing: `platform-system`, `platform-catalog`, `platform-catalog-canary`,
`platform-secrets`.

**Third-party namespaces** (`argocd`, `tekton-chains`, `fulcio-system`,
`external-secrets`, `observability`, `crossplane-system`) are not ours to rename - keep
whatever that tool's own install convention uses.

**Helm release name = namespace name, exactly.** `helm install <type>-<app-name>-cicd
charts/glidepath-app ...` - one less thing to keep in sync by hand.

## Catalog Task names

Verb-noun (or verb-only), kebab-case, matching the file name exactly
(`build-image.yaml` -> `build-image`). Already consistent across the whole catalog -
confirmed by auditing all 26 Task names live before writing this doc. Keep it that way.

## Catalog Pipeline names

Noun matching the stage or check it represents: `build`, `test`, `deploy`, `release`,
`sast-check`, `image-scan-check`, `provenance-check`, `sbom-check`, `qa-check`,
`governance-check`, `image-promotion-check`, `bypass-merge-check`, `onboarding-resync`.

**Not actually consistent until 2026-08-23**: this doc originally claimed the list above
was "already consistent," but missed a real gap - `policy-check` was the *gate's own
name*, not just its Pipeline's `-check` suffix (every other real gate's name, e.g.
`sast`, has no suffix at all; its Pipeline adds `-check`). That made `policy-check` the
only gate whose identity redundantly baked in "check" - and it didn't say what it
actually verified (gitsign commit signatures + SLSA provenance). Renamed the gate to
`provenance` (Pipeline: `provenance-check`, matching every other gate's pattern) - see
docs/admin/release-guardrails.md.

## Step names within a Task - the real inconsistency, now fixed

Audited live: three genuinely different patterns existed under one implied name,
`emit-span`/span-related steps:

1. **Dedicated step, sole job is sending a pre-computed span** (`otel_task_span_send`):
   `build-source.yaml`, `run-tests.yaml`, `sast-scan.yaml` - each has a step named
   exactly `emit-span`. **This is the standard - use this shape whenever a Task can
   afford a dedicated trailing step.**
2. **The same `otel_task_span_send` call, folded into a step doing something else**:
   `build-image.yaml`'s `emit-image-ref-result` step also sends the Task's span, for a
   real structural reason (kaniko has no shell, so the span has to be sent from
   whichever bash step runs after it, and that step already exists to extract kaniko's
   results) - but the step's name didn't disclose the second job. **Fixed**: renamed to
   `emit-image-ref-and-span`. When a span-send has to be folded into another step for a
   similar structural reason, name the step to disclose both jobs - never let a name
   describe only one of two things a step does.
3. **No separate step at all - `otel_child_span` wraps the live command directly**:
   `image-scan.yaml`, `generate-sbom.yaml`. This is a genuinely different, correct
   mechanism (you can't retroactively wrap a step that already finished with a span
   covering its execution), not a naming gap - document it as an intentional exception
   where it appears, don't leave it looking like an oversight.

`start-span`/`end-span` (used by `start-stage-span.yaml`/`end-stage-span.yaml`/
`start-flow-root-span.yaml`/`end-flow-root-span.yaml`) are intentionally a different
name from `emit-span` - they're a genuine two-phase begin/end pair spanning *separate*
Tekton Tasks (sometimes separate PipelineRuns entirely), not a one-shot send within a
single Task. Don't unify these names - the distinction is real and worth keeping
visible.

Other step-name patterns already consistent, keep using them: `resolve-*` for
config/parameter-resolution steps that run before the real work (`resolve-build-config`,
`resolve-test-command`, `resolve-build-script-path`), and a plain verb (or
verb-noun) for the step doing the actual work (`scan`, `build-and-push`,
`run-build-script`, `unit-test`, `generate-and-attest`).

## PipelineRun naming

**`generateName` must always end in `-`, except the gitops-repo governance-check trigger
files, which must NOT.** Two real, live-confirmed findings layered on top of each other
here - read both before touching either:

1. Missing the trailing dash is the actual root cause of PipelineRun names like
   `sastgdn8r` instead of `sast-gdn8r` - confirmed live: the five gitops-repo
   governance-check trigger files that existed at the time (`sast`, `policy-check` -
   since renamed `provenance`, `image-scan`, `sbom`, `bypass-check`) were missing the
   trailing dash, while the app-repo side (`build-`, `pr-validate-`,
   `onboarding-resync-`) already had it right. First fix: add the dash everywhere.
2. **That first fix was then partially reverted, deliberately, once tested live**:
   Pipelines-as-Code derives the GitHub Check context name directly from
   `metadata.generateName`, verbatim - no trailing-dash trimming (confirmed via a real
   test PR against `gitops-nodejs-demo-app`: adding the dash produced a real PipelineRun
   name like `sast-gdn8r`, but the check name became `sast-` too, not `sast`). No
   annotation to decouple the two was found after a real search of PaC's docs. Since the
   clean check name (`sast`, not `sast-`) is the explicitly stronger preference, every
   gitops-repo governance-check onboarding template (nine as of 2026-08-23, after
   `itsm`/`qa`/`policy-validation`/`image-promotion` were added on top of the original
   five - see docs/admin/release-guardrails.md) **keeps its dash-free `generateName`**
   deliberately - the resulting `sastgdn8r`-style PipelineRun name is the accepted cost
   of the cleaner, more visible check name. Every new gitops-repo onboarding template
   added since has followed this (dash-free) convention, not the general app-repo-side
   one. Don't "fix" this again without a real mechanism to set the check name
   independently of `generateName` - reverting the original dash-everywhere fix, for
   just these files, was itself the fix.

Deterministic (non-`generateName`) PipelineRun names fired by the broker follow
`<flowName>-<index>-<stageName>-<chainSlug>-$(body.context.id)` (e.g.
`ci-1-test-brave-otter-a1b2c3d4`), where:
- `index` is the step's zero-based position in the flow's `steps` list - added so two
  steps sharing a stage name (e.g. two `test` steps in one flow) don't collide, and so
  the CEL filter chaining one step to the next can match on the exact previous step
  rather than any step with that stage name (see `flow-triggers.yaml`).
- `chainSlug` (`$(body.customData.platform.chain_slug)`) is a two-word deterministic
  stand-in for the flow's chain-id (see `cdevents.sh`'s `chain_id_to_slug`) - derived
  from the first 4 hex chars of chain-id, which never changes across a flow, so every
  chained PipelineRun in the same flow instance shows the same two words, making them
  easy to spot as belonging together at a glance without reading the full chain-id.
- `$(body.context.id)` is a deterministic hash of the *emitting* PipelineRun's own name
  plus the event type it emitted (see `cdevents.sh`), truncated to 8 hex chars (down
  from an original 20, to make room for `chainSlug` within Kubernetes' 63-char name
  limit) - this is what preserves redelivery idempotency: a retried CDEvent delivery
  reproduces the exact same id, so the next-stage Trigger's PipelineRun creation is a
  harmless no-op instead of a duplicate run. `chainSlug`, though also deterministic,
  does NOT provide this on its own, since it's shared across an entire flow rather than
  unique per stage-transition - it must stay a middle segment, never a replacement for
  `context.id` as the name's uniqueness key.

A flow's git-rooted first step (`trigger.source: git`, e.g. `build`) is generated by PaC
from `.tekton/*.yaml` in the app's own repo, not by the broker - at that point, no
CDEvent (and therefore no chain-id or `context.id`) exists yet, since chain-id itself is
minted by `start-flow-root-span` during build's own execution, after Kubernetes has
already assigned build's PipelineRun its name. So build's name can carry neither
`chainSlug` nor a `context.id`-style hash; it instead uses a real Kubernetes
`generateName` of `<flowName>-0-<rootStage>-` (e.g. `ci-0-build-`), letting the API
server append its own random suffix (`ci-0-build-knc2z`) - structurally consistent with
the broker's pattern (`<flowName>-<index>-<stageName>-`) even though the trailing
segment comes from a different mechanism and can't share `chainSlug` with the rest of
the flow's PipelineRuns.

**Deferred idea, not implemented**: build's `metadata.name` itself can never carry
`chainSlug` (Kubernetes object names are immutable, and chain-id doesn't exist until
after the name is already assigned - see above), but build's PipelineRun *object* could
still be labeled with its flow's `chainSlug` after the fact, since labels (unlike names)
can be patched post-creation. `start-flow-root-span` mints chain-id and would need one
more step to compute `chain_slug` (reusing `cdevents.sh`'s `chain_id_to_slug`) and
`kubectl label pipelinerun $(context.pipelineRun.name) hangar.io/chain-slug=<slug>
--overwrite` against itself - Tekton exposes `$(context.pipelineRun.name)` inside a
step for exactly this. Would need confirming `pipeline-runner`'s ServiceAccount can
`patch` PipelineRuns in its own namespace first. This would let `kubectl get pipelinerun
-l hangar.io/chain-slug=<slug>` pull back build alongside test/deploy/release even
though build's name string alone never will. Raised and consciously left undone
2026-09-05 - worth reconsidering if build's isolation from the rest of its flow becomes
an actual pain point (e.g. in dashboards or triage), not just a naming curiosity.

**Deferred idea, not implemented**: the gitops-repo governance checks (`sast`,
`image-scan`, `sbom`, etc. - see "PipelineRun naming" below) have no `chainSlug`
correlation to their release either, and unlike build there's no existing chain-id
channel into them at all - they're triggered by PaC straight off a GitHub PR event on
the gitops repo, not through the CDEvents broker. Two paths considered and rejected for
now:
1. Bake a `hangar.io/chain-slug` label into the static `.tekton/pull-request-*.yaml`
   check files when `open-release-pr.yaml` opens the release PR - rejected because that
   Task deliberately makes "exactly ONE commit per release PR" touching only the
   manifest (a prior incident, 2026-08-31, found that widening the diff re-ran every
   governance gate a second time); rewriting the check files every release reintroduces
   that same class of unwanted churn.
2. Have `open-release-pr.yaml` attach the chain-slug as a GitHub label on the PR itself
   (no repo file touched), and have each governance-check Task read it via GitHub API
   and self-label its own PipelineRun. Workable, but touches ~9 separate catalog Tasks
   (`sast-scan`, `image-scan`, `generate-sbom`, and the rest) for a naming-correlation
   nicety - out of proportion to the ask right now. Raised and consciously left undone
   2026-09-05, same day as the build-labeling idea above - reconsider both together if
   cross-flow triage pain actually shows up.

## GitHub Check / status context names

Short, no trailing dash, matching the concept the file represents (`sast`, `provenance`,
`image-scan`, `sbom`, `qa`, `itsm`, `policy-validation`, `image-promotion`,
`bypass-check`). Confirmed-good, keep as-is.

## Helm chart and file naming

- Chart names: `glidepath-<concern>` (`glidepath-catalog`,
  `glidepath-control-plane`, `glidepath-app`).
- Catalog Task/Pipeline/StepAction files: `<metadata.name>.yaml`, exactly - already the
  norm, keep it exact (a mismatch is a real "which file is this Task actually defined
  in" trap).
- Chart template subdirectories grouped by concern, not resource kind:
  `identity/`, `triggers/`, `env/`, `argocd/`, `governance/`, `sigstore/`, `broker/`,
  `dora-exporter/`, `hooks/`, `secretstore/`.
- Docs: kebab-case, descriptive noun-phrase (`catalog-versioning.md`,
  `secrets-management.md`) - already consistent.

## Labels and annotations

`hangar.io/*` is this platform's own label namespace. As of this pass, every resource
across all three charts (116 total: 43 catalog, 38 control-plane, 35 in a full-fixture
App render) carries a full, consistent label set via a per-chart `<chart>.labels`
named template (`templates/_helpers.tpl`) - not just the handful of resources that
happened to need a label for a real selector before now.

**Standard Kubernetes-recommended labels** (`app.kubernetes.io/*` + `helm.sh/chart`) -
generic tooling interop (kubectl, Lens, ArgoCD's own resource tree), not platform-
specific:

```yaml
app.kubernetes.io/name: <chart name>
app.kubernetes.io/instance: <helm release name>
app.kubernetes.io/version: <Chart.yaml appVersion>
app.kubernetes.io/managed-by: Helm
app.kubernetes.io/part-of: glidepath
helm.sh/chart: <chart name>-<chart version>
```

**`hangar.io/component`**: `catalog` | `control-plane` | `app` - which of the three
charts owns this resource. The single most useful selector for a cross-cutting audit,
e.g. `kubectl get role -A -l hangar.io/component=app`.

**`hangar.io/subcomponent`**: which concern *within* that chart - matches the template
subdirectory a resource's file lives in (`identity`, `triggers`, `env`, `argocd`,
`governance`, `broker`, `dora-exporter`, `sigstore`, `hooks`, `secretstore`, `grafana`). Lets you
narrow `hangar.io/component=control-plane` down to just
`hangar.io/subcomponent=sigstore`, for example.

**`hangar.io/app`** (app chart only, on every resource it creates): most valuable on
the resources that live in a *shared* namespace with other Applications' resources
(`argocd`'s AppProjects/Applications/ApplicationSets/Roles) - lets you
`kubectl get application -n argocd -l hangar.io/app=nodejs-demo-app` instead of relying
on name-pattern matching. Applied to every app-chart resource, not just the
shared-namespace ones, for consistency.

**`hangar.io/stub`**: `"true"` on catalog resources that are still genuinely stub
implementations - currently `governance-gate-stub` (Task), `governance-stub`
(StepAction), `governance-check` (Pipeline, though live-confirmed unreferenced by any
current onboarding trigger - a real, minor dead-code finding, not acted on here).
Reinforces this platform's existing "stub-ness must be structurally loud" principle
(previously only visible in trace span attributes and docs) at the resource-selection
level too: `kubectl get task -n platform-catalog -l hangar.io/stub=true` now answers
"which catalog gates are still fake" directly.

**Pre-existing `hangar.io/*` labels/annotations** (kept exactly as-is, already
consistent with this scheme): `hangar.io/catalog: "true"` (catalog-resolvable
resources), `hangar.io/dora-track: "true"` (Applications the DORA exporter watches),
`hangar.io/dora-pending`/`-app-namespace`/`-app`/`-flow-start-time`/`-baseline-started-at`
(DORA tracking state annotations on Applications), `hangar.io/stall-alerted` (dedup
marker), `hangar.io/ephemeral-env` (PR-namespace TTL sweep target marker),
`hangar.io/purpose` (free-text annotation, currently only on the ClusterSecretStore's
source namespace).

A real bug found and fixed while applying this broadly: two pre-existing resources
(`fulcio-server`'s Deployment, `dora-exporter`'s Service) already had their own
`metadata.labels` block for an unrelated reason (a plain `app: <name>` selector label) -
naively inserting a second `labels:` key produced invalid/duplicate-key YAML rather than
merging. Fixed by merging into one block in both cases; swept the whole of `charts/` for
the same class of bug afterward (both block-style and flow-style `labels: { ... }`) and
confirmed zero remaining occurrences before considering this done.
