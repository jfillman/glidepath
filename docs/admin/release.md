# Release stage: GitOps PR promotion to staging

The release stage promotes the exact image that already passed test/deploy-to-dev
(never rebuilt) to a `staging` environment, via a pull request against a dedicated
GitOps repo - gated by governance checks reported as independent GitHub Checks and a
required-reviewer branch-protection rule. See [ADR-0004](adr/0004-gitops-only-release.md)
for why release is built this way rather than a direct-deploy path.

## Flow

```
deploy (dev) succeeds
  -> dev.cdevents.service.deployed CDEvent, now carrying git-url/revision forward
  -> broker fires `release` PipelineRun
  -> release opens a PR against gitops-<app-name>, bumping the image tag in
     <cluster>/<env>/values.yaml (charts/glidepath-catalog/templates/tasks/
     open-release-pr.yaml) - this is where the automated part of the flow ends; the
     flow-root trace closes here (see docs/tracing.md)
  -> the release-guardrail GitHub Checks run on that PR, one per gate in
     .Values.releaseGuardrails (sast/image-scan/provenance/sbom real, itsm/qa/
     policy-validation/image-promotion still stubs - see
     [release-guardrails.md](release-guardrails.md)/[governance-stubs.md](governance-stubs.md)),
     each individually re-triggerable and independent of the others except
     `image-promotion`, which deliberately runs last and waits on the rest
  -> branch protection on the gitops repo requires every one of those checks + N human
     reviewers

     Once ArgoCD confirms this release's own sync outcome, `release-outcome-notify.yaml`
     also emits a structured release-log record (approvers, per-gate results, bypass) -
     see [release-log.md](release-log.md).

     `provenance` verifies a `gitsign` signature on the actual APP-repo commit that
     triggered this release (see docs/commit-signing.md) - not this gitops PR's own
     commits, which are machine-generated and never signed. **This means the app-repo
     commit that ends up promoted must itself be gitsign-signed, which a GitHub PR
     merge-button click always breaks** (merge/squash/rebase all produce a new,
     unsigned commit object) - see docs/commit-signing.md's "Merge strategy matters"
     section before assuming "review the app PR, click Merge" will work end to end.
  -> once merged, ArgoCD (syncPolicy.automated: prune+selfHeal) syncs the new manifest
     into <type>-<app-name>-staging - the release Pipeline never touches the cluster directly
```

## Performance: no clone, no schema re-validation, for the common (event-chained) case

Neither `deploy.yaml` (`deploy-manifests.yaml` does its own separate clone of the *app*
repo itself, via the GitHub App token broker, to commit `platform/envs/<env>.yaml`) nor
`release.yaml` (`open-release-pr.yaml` does its own separate clone of the *gitops* repo,
a different repo entirely) ever reads from the app repo's source tree through this
Pipeline's own `source` workspace. Both used to
unconditionally clone it and run full JSON-schema validation on `cicd.yaml` anyway,
purely so `notify-slack`'s finally step could read two fields off it - a full clone plus
a dynamically-provisioned PVC, on every single stage transition, for a workspace that
was otherwise dead weight.

Fixed: `cicd.yaml`'s already-validated content is now forwarded stage-to-stage through
the CDEvents chain instead (`customData.platform.config_json` - see docs/chaining.md),
ultimately sourced from `test`'s own `validate-config` (which always runs regardless,
needed for `resolve-agent-image`/`integration-test`). `deploy.yaml`/`release.yaml`'s
`source` workspace is `optional: true`, and `flow-triggers.yaml` skips binding it
entirely for the common event-chained case, which is what actually avoids the PVC
provisioning cost, not just a clone. `catalog/tasks/resolve-notify-config.yaml` is the
arbiter: pass through the inherited value, or (only for a git-rooted deploy/release,
where a real workspace is bound) clone and read `cicd.yaml` directly off it - there's no
separate `clone-repo` task any more. That last point isn't a style choice: Tekton
rejects a Pipeline at *admission* time if any task references an optional Pipeline
workspace through its own *required* Task-level workspace, regardless of `when`-gating -
confirmed live the hard way (the shared, hub-resolved `git-clone` catalog Task declares
its workspace required, so it can never coexist with an optional Pipeline workspace, no
matter how it's gated). See `resolve-notify-config.yaml`'s own header for the full story.

**Tradeoff, deliberate**: that git-rooted fallback read is *not* schema-validated the way
`validate-config` does it - `deploy.yaml`/`release.yaml` dropped `validate-config`
entirely, since nothing else in either pipeline ever consumed its output. This relies on
`cicd.yaml` having already been schema-validated earlier in the app's lifecycle
(`onboarding-resync` validates it when generating the `.tekton/` files in the first
place, and `build` validates it again for any flow that includes a `build` step) - a
malformed `cicd.yaml` reaching a git-rooted `deploy`/`release` with no earlier validation
in its own history is a real, if narrow, gap. Revisit if that ever proves to matter in
practice (every real git-rooted deploy/release use case today is tag-triggered - see
`resolve-image-ref.yaml`'s own header).

## How the GitHub App's private key stays out of Application namespaces

The GitHub App used for git operations (clone/push/PR-open) is the same App PaC already
uses - but its private key lives in a Secret in the `pipelines-as-code` namespace, which
an Application pipeline pod (running in its own namespace) can't mount directly
(Kubernetes Secrets are namespace-scoped). Rather than copy the key into every
Application's namespace (which would let one compromised Application's Task mint tokens
for every other Application's repos), the shared `token-review-interceptor` service -
already deployed, already TokenReview-authenticated for the CDEvents broker path - has a
second endpoint, `/github-installation-token`, that:

1. Verifies the caller's own Application identity via TokenReview (same mechanism as the
   broker path).
2. Checks the caller's Application namespace actually owns a PaC `Repository` CR that maps
   to the requested `gitops-<app-name>` repo by name - so an Application can only ever get a
   token for its own release repo, never another Application's.
3. Mints a GitHub App installation token scoped to **only that one repo** (via GitHub's
   `repositories` field on the installation-token exchange) and returns it. The
   private key itself never leaves `platform-system`.

`catalog/lib/github-app.sh` wraps this call for `open-release-pr.yaml`.

### Resilience to transient GitHub errors (2026-09-03)

A GitHub API 503 during `gitops-image-bump` surfaced two gaps, both now fixed across
every catalog Task that talks to `api.github.com`, a container registry, or the token
broker:

1. **No retry on transient failures.** Every such `curl` call now carries
   `--retry 3 --retry-connrefused --max-time 15` (the convention `cdevents.sh` already
   used for the CDEvents broker), so a 503/502/504/connection-refused/timeout is retried
   with backoff instead of failing the Task outright. `git clone`/`git push` against
   `github.com` (curl's flags don't reach those) get the same treatment via a small
   `with_retry` wrapper in `catalog/lib/retry.sh`.
2. **Retries weren't actually safe.** `bump-manifest-pr.yaml` and `open-release-pr.yaml`
   both named their PR branch `<prefix>-<revision>-$(date +%s)` - so even though each
   Task's own `pr-url` result already claimed "(or already-open, on retry)", a retried
   run (Tekton task retry, or a re-triggered PipelineRun) always looked like a brand new
   change and opened a duplicate PR. Both branch names are now deterministic and drop the
   timestamp: `bump-manifest-pr.yaml` uses `image-bump-<app-name>-<revision>`,
   `open-release-pr.yaml` uses `release-<env>-<revision>` (`<env>` included because a
   single revision can release to multiple upper envs off the same push - e.g. cicd.yaml
   declaring both a `staging` and a `prod` release step - and without it every env
   collapsed onto the same branch name, so whichever release step ran second just found
   the first env's PR "already open" and silently reused its URL instead of promoting its
   own env; real bug, live-confirmed on checkout-api, fixed 2026-09-04). Both Tasks check
   for an already-open PR from their exact branch before doing any work, reusing it
   instead of duplicating it.

   `deliver-onboarding-files.yaml`'s two onboarding-resync branches had the same
   timestamped-branch pattern, but a different fix: there's no single "revision" to key
   an onboarding-resync branch on (it regenerates whatever cicd.yaml/the platform's own
   templates currently look like, which can differ on every trigger), so skip-if-open
   isn't right - a genuinely new template change would go silently uncommitted forever
   behind a stale open PR. Instead both branches are now static
   (`onboarding-resync`, not timestamped), force-pushed with fresh content on every
   trigger, and only the PR-*creation* call is guarded against an already-open PR
   (GitHub's own create-PR call 422s on a duplicate head branch anyway) - so a repo has
   at most one open onboarding-resync PR at a time, updated in place rather than
   accumulating duplicates.

## Onboarding an Application's release stage (per app)

**2026-08-16: superseded for any app onboarded through `idp`** (Crossplane-based,
`airframe`'s `ApplicationEnvironment` composition) - that composition already
scaffolds `gitops-<app-name>` with the `<cluster>/<env>/values.yaml` layout
`open-release-pr.yaml` now requires (an `airframe-application` Helm values file, not a raw
Deployment manifest), and its own tenant-onboarding `ApplicationSet` already creates and
owns the ArgoCD `Application` generically for every env - steps 2 and 5 below (manually
pushing `deployment.yaml`, applying `release-application.yaml`) don't apply and would
actively conflict with idp's mechanism. `cicd.yaml`'s `deploy.upperEnvironments` must use
the `{ name: <env>, cluster: <cluster> }` object form, matching that
`ApplicationEnvironment`'s own `spec.cluster` - see `open-release-pr.yaml`'s own
2026-08-16 header for the full story. See idp's own docs for that onboarding path.

The steps below are the ORIGINAL, pre-idp procedure - kept accurate as a historical
record and because it's still what the platform's own remaining non-idp demo tenants
(`nodejs-demo-app`, `cicd-flow-test-app`) are on, but it no longer produces a working
release for any *new* app: `open-release-pr.yaml` hard-requires `cluster` now and writes
a Helm values file, not the raw `deployment.yaml` this procedure describes seeding.

This is all one-time setup per app, same spirit as onboarding the app repo itself (see
`docs/admin/onboarding-mechanics.md`) - not something a developer does per release.

1. **Plant the GitHub App credentials into Infisical**, once per cluster, not per
   Application (skip if already done) - **2026-08-19: no longer a `kubectl` copy of
   `pipelines-as-code-secret`.** `github-app-creds` in `platform-system` is now a real
   `ExternalSecret` (`charts/glidepath-control-plane/templates/secretstore/
   github-app-creds-external-secret.yaml`), synced from the control plane's own
   Infisical project (`platform-cicd-kind-dev`) - see
   [secrets-management.md](secrets-management.md). Read the GitHub App's id/private key
   off the GitHub App itself (App settings page - the `.pem` you download when
   generating a private key, plus the App ID shown there) and plant them as
   `github-application-id`/`github-private-key` keys in Infisical (via the UI/API, never
   through this chart or committed to this repo); the `ExternalSecret` picks them up
   automatically from there.

   **2026-08-23: `pipelines-as-code-secret` itself is now also synced from these same
   two Infisical keys**, no longer a one-time manual source to copy out of -
   `gitops-cluster-dev/50-platform-cicd/tekton-operator/
   pipelines-as-code-secret-external-secret.yaml`, see
   [secrets-management.md](secrets-management.md)'s "What this deliberately is not"
   section. A cluster rebuild no longer needs either Secret hand-recreated - just this
   one Infisical plant, once per cluster.

2. **Create the `gitops-<app-name>` repo on GitHub** (e.g. `gitops-nodejs-demo-app`) and
   push `<app-name>/staging/deployment.yaml` + `service.yaml` (adapted from the app's own
   dev manifests) to it - this is the one part of the gitops repo's content the platform
   has no way to generate for you (it doesn't know your Deployment's shape).

   The release-guardrail `.tekton/` files (one `pull-request-<gate>.yaml` per gate in
   `.Values.releaseGuardrails` - see [release-guardrails.md](release-guardrails.md),
   with `<APP_NAMESPACE>`/`<APP_NAME>` already substituted where a given gate needs
   them) do **not** need to be hand-copied - `deliver-onboarding-files.yaml`'s
   `deliver-gitops-repo-files` step delivers them via the same onboarding-resync PR
   mechanism that delivers the app repo's own `.tekton/` files (see
   [onboarding-mechanics.md](onboarding-mechanics.md) step 6), as long as `platformIdentity.gitopsRepoUrl` was
   given at install time and the `Repository` CR from step 4 below already exists. Confirmed
   live: a fresh gitops repo with nothing but the two manifests above gets every
   governance file opened as a PR automatically on the next onboarding-resync run.

3. **Install the PaC GitHub App on the new repo.** While there, check its permissions
   include `Contents: Read & write` and `Pull requests: Read & write` - PaC's own needs
   (webhook delivery, checks) may not already include these, since this App is now also
   doing git operations, not just receiving webhooks.

   **Do this even if the repo is private, and don't assume installing the App on the
   app repo already covers it** - a GitHub App installed with "selected repositories"
   only reaches repos explicitly added to it, one at a time, independent of visibility.
   Skipping this step for a private `gitops-<app-name>` repo makes
   `deliver-onboarding-files`'s push fail; the broker's `/github-installation-token`
   endpoint now surfaces this as an explicit "confirm the App is installed on `<repo>`"
   error (`catalog/lib/github-app.sh`) rather than an opaque clone/push failure - found
   live 2026-08-22 against `gitops-checkout-api`.

4. **Nothing to do here anymore** - as long as `platformIdentity.gitopsRepoUrl` is set,
   `charts/glidepath-app/templates/pipelines-as-code/repository.yaml` renders the
   `gitops-<app-name>` PaC `Repository` CR automatically (same file that renders the app
   repo's own Repository CR - see [onboarding-mechanics.md](onboarding-mechanics.md) step 1). Previously a
   manual `kubectl apply`, folded into the chart once step 2 above already needs
   `gitopsRepoUrl` to be set at install time regardless.

5. **Apply the ArgoCD template**, with `<APP_NAMESPACE>`, `<APP_NAME>`, `<GITOPS_REPO_URL>`
   substituted:
   ```
   sed -e 's#<APP_NAMESPACE>#app-nodejs-demo-app-cicd#g' -e 's#<APP_NAME>#nodejs-demo-app#g' \
       -e 's#<GITOPS_REPO_URL>#https://github.com/<org>/gitops-nodejs-demo-app#g' \
     charts/glidepath-app/templates/argocd/release-application.yaml | kubectl apply -f -
   ```

6. **Configure branch protection** on the gitops repo's `main` branch (GitHub UI or
   API - not IaC-managed by this platform, documented here so it isn't a tribal-
   knowledge step): require status checks matching every `name` currently in
   `.Values.releaseGuardrails` (`charts/glidepath-catalog/values.yaml` - today
   `sast`, `image-scan`, `provenance`, `sbom`, `itsm`, `qa`, `policy-validation`,
   `image-promotion`) to pass, and require **2 approving reviews**. Enable "Allow
   auto-merge" on the repo so a PR merges itself the moment checks + reviews are
   satisfied, without needing anyone to click Merge. This list changes whenever a gate
   is added/removed (see [release-guardrails.md](release-guardrails.md)'s "Branch
   protection" section) - it is not kept in sync automatically.

## Evolving the approval requirement

The required-reviewer count is a branch protection setting, not platform code - once
the governance checks are real (Phase 3) and trusted, drop
`required_pull_request_reviews.required_approving_review_count` to `0` on the gitops
repo directly. Nothing in the platform needs to change for this; the PR flow becomes
fully automatic (checks-gated only) the moment that setting changes.

## Governance checks, re-triggering, and break-glass

Every gitops-repo release PR carries one required GitHub Check per gate in
`.Values.releaseGuardrails` (see the PR body itself, which now documents this directly -
Phase 3 item 8.6, and [release-guardrails.md](release-guardrails.md) for the mechanism
and current gate list). `sast`/`image-scan`/`provenance`/`sbom` are real as of Phase 3
items 8.4/8.5/item 2/item 8.7 (see that item's own "cosign `--ca-roots` deprecation"
fallout note in [provenance-policy.md](provenance-policy.md) for a still-unresolved edge
case, not a stub); `itsm`/`qa`/`policy-validation`/`image-promotion` are still stubs.
All except `image-promotion` run independently of each other; `image-promotion`
deliberately waits on the rest (see that doc's "Two shapes" section).

**Re-running a failed check**: comment `/retest <check-name>` on the PR (e.g. `/retest
sast`) to re-run just that one, or `/retest` alone to re-run every gate in
`.Values.releaseGuardrails` - PaC's own native comment mechanism, no platform code
involved. Fix the underlying issue first if the failure is real; re-running only
re-evaluates the check, it does not bypass it.

**Each check also posts its own real PR comment** on top of the GitHub Check PaC already
reports natively (`catalog/tasks/comment-pr-check-result.yaml`, 2026-08-10) - failure
detail (a live pod-log excerpt), a gate-specific recommendation, and the exact `/retest
<gate>` command on failure; a short confirmation on success. It edits its own prior
comment on a gate in place (a hidden `<!-- platform-cicd:gate:<name> -->` marker) rather
than always appending, so re-verifying an unchanged commit updates the existing comment
instead of spamming a duplicate. **Real race, found live 2026-08-12**: since every
commit on a release PR gets independently re-verified, any two check runs for the *same*
gate landing close enough together (originally: `open-release-pr.yaml`'s own two commits,
back when it made two - see [multi-cluster.md](multi-cluster.md); now, e.g. a `/retest
<gate>` racing a webhook redelivery) can both read "no existing comment" before either
posts - two comments, not deduped. Fixed without a lock: after posting, re-list this
gate's own comments and collapse to the single earliest (lowest id) survivor - every
racing run computes the same decision off the same eventually-consistent list, so they
converge regardless of which one "wins" (a second run's delete of an already-deleted
duplicate is expected and tolerated).

**Update, 2026-08-31**: `open-release-pr.yaml` now pushes exactly one commit per release
PR - the second commit (outcome-tracking data, needing the PR's own URL) was found to be
re-triggering every governance gate a second time on every single release, for zero
informational gain. `pr-url`/`pr-created-at` are resolved a different way now (a
dev-cluster ConfigMap, read by `release-outcome-notify.yaml`'s `resolve-release-tracking`
Task - see multi-cluster.md); this dedup logic stays in place regardless, since the race
it guards against isn't specific to that removed second commit.

**Break-glass** (decided with the user, 2026-08-05): who can force a release through a
failing required check, and how it's made visible.

- **Who**: a dedicated GitHub team (e.g. `platform-admins`), not repo/org admins
  generally - narrower, purpose-specific access. This is a **manual GitHub
  configuration step**, not something this platform's code performs: create the team,
  add its intended members, then on the gitops repo's branch protection rule for `main`,
  enable "Allow specified actors to bypass required pull requests" (or the equivalent
  "Do not require approvals/status checks for administrators" toggle, depending on which
  GitHub plan/UI is in use) scoped to that team. Matches this platform's existing
  precedent for branch protection itself (`docs/release.md`'s own "Evolving the approval
  requirement" section above) - a manual, out-of-band GitHub setting, not platform IaC.
- **Audit**: GitHub's own merge/audit-log record is real and already visible on the PR,
  but a security-relevant bypass shouldn't depend on someone thinking to go check it -
  `charts/glidepath-catalog/templates/tasks/detect-bypass-merge.yaml` fires on every PR close (via the new
  `.tekton/pull-request-merged.yaml` trigger) and posts a real Slack alert (same
  `slack-webhook-url` Secret/design language as every other notification, but **not**
  gated on `notifications.slack.enabled` - a bypass alert isn't a routine preference)
  whenever a PR merged despite one of the four checks not showing `success`. A normal,
  fully-green merge produces no alert - this is deliberately quiet in the common case.
  Verified live against real GitHub data (not merely the code path): a real merged PR
  with all checks green correctly produced no alert; a real PR with a genuinely failing
  `provenance`, fed synthetically as `merged=true` (no actual bypass has happened yet
  on this repo), correctly detected it and posted a real, visible Slack message.
  **Still not fully verified** (a real PR close event with an actual bypass hasn't been
  exercised), but a real, related bug WAS found and fixed live 2026-08-12 while
  investigating something else: PaC evaluates a Pipeline's `on-cel-expression` against
  *every* webhook delivery for the repo once one is present, not just events shaped like
  what the expression expects - confirmed live via a genuine push webhook (a release
  PR's own merge fires one, alongside the `pull_request` event) throwing a real CEL "no
  such key: action" error against this file's bare `body.action == "closed"`, reported
  back as a false-alarm bot comment ("errors in your PipelineRun template") on the very
  PR that had just successfully merged. Fixed with `has(body.action) && body.action ==
  "closed"` - `has()` short-circuits the rest of the expression when the event isn't
  PR-shaped, same fix applied to all four governance-check files below (they had the
  identical bare-field pattern on `body.pull_request.*`). Doesn't confirm the intended
  bypass-detection behavior itself, but does confirm this Trigger's CEL now survives
  contact with real, mixed webhook traffic instead of erroring on it.

## Verification

- Push to the app repo, let it flow through build->test->deploy->release for real.
- A PR should appear on `gitops-<app-name>` with the correct image reference, and every
  governance check in `.Values.releaseGuardrails` should show up in the PR's checks list
  (each individually re-runnable, e.g. via `/retest sast` PR comment) - all independent
  of each other except `image-promotion`, which should only go green once the rest have.
- Security check: confirm `/github-installation-token` genuinely rejects a request for
  a repo the caller's Application doesn't own (test from `platform-cicd-demo`'s SA
  requesting a different, unrelated `gitops-*` repo name - should get a 403).
- After merge: `kubectl get application nodejs-demo-app-staging -n argocd` shows
  `Synced`/`Healthy`, and the running pod in `platform-cicd-demo-staging` is on the new
  image - confirmed via `kubectl get pods -n platform-cicd-demo-staging -o
  jsonpath='{.items[0].spec.containers[0].image}'` - with no direct `kubectl` from any
  Tekton Task in that path.
