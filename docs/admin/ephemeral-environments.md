# PR-based ephemeral environments

Any PR against `nodejs-demo-app` labeled `preview` gets a real, running, isolated
environment - its own namespace, a full `airframe-application`-chart-rendered workload
(`Rollout`/`Service`/`ServiceAccount`/`NetworkPolicy`/`ServiceMonitor`/`RolloutWatch`),
running the exact image that PR's own build produced - torn down within the TTL sweep's
window after the PR closes or the label is removed. This is modeled on a real prior
production pattern (ArgoCD ApplicationSet's `pullRequest` generator, `gitops-main/charts/
cd-pipelines-user/templates/appset-ephemeral-envs-pr.yaml`, analyzed live), keeping the
generator mechanism itself (it was sound) while fixing two things that mechanism's
supporting pieces got wrong in production - see "What's different from the old system"
below.

**2026-08-24: moved off a raw Kustomize base (`k8s/ephemeral/`) onto the same
`airframe` `airframe-application` chart every other tier (release, lower-env)
deploys through** - see "Flow" and "Deploying through airframe-application, not raw Kustomize"
below for the current mechanism. Sections describing the old Kustomize-specific
mechanics have been removed; check this file's own git history if you need them.

## Flow

```
developer adds the `preview` label to a PR on nodejs-demo-app
  -> within ~180s, ArgoCD's ApplicationSet pullRequest generator (polling GitHub,
     label-gated) picks it up
  -> generates an Application named nodejs-demo-app-pr-<number>, with two sources:
     airframe's airframe-application chart (envName/rollout.image stamped per-PR
     via valuesObject) plus a ref-only source into nodejs-demo-app's own repo, pinned to
     that PR's head SHA, for platform/pr-env.yaml's values
  -> a separate, dedicated pull_request-triggered flow (cicd.yaml's own pipelines: entry
     for it) builds and pushes that PR's image, tagged with a bare 12-char sha (not the
     normal <version>-<short-sha> scheme - see "Image tagging: sha-only for PR builds"
     below) - nothing in the repo needs per-PR editing beyond that one cicd.yaml entry,
     added once at onboarding
  -> Application syncs into app-nodejs-demo-app-pr-<number>, created via
     CreateNamespace=true (see "Namespace cleanup: TTL sweep, not cascade-delete" below -
     this is a deliberate change from the old Kustomize-based design)
  -> developer: kubectl port-forward -n app-nodejs-demo-app-pr-<number> svc/nodejs-demo-app <port>:80
  -> PR closed, or `preview` label removed
  -> generator stops returning that PR -> ArgoCD deletes the generated Application ->
     resources-finalizer.argocd.argoproj.io cascade-deletes everything it tracks (the
     Rollout/Service/etc, NOT the namespace itself) -> the namespace sits empty until the
     TTL sweep removes it (up to 24h later)
```

## Deploying through airframe-application, not raw Kustomize

Originally this ApplicationSet sourced straight from the app repo's own `k8s/ephemeral/`
Kustomize base (`namespace.yaml`/`deployment.yaml`/`service.yaml`/`kustomization.yaml`).
Replaced 2026-08-24 with the same two-source `airframe-application`-chart pattern the
lower-env tier's own `lower-envs-applicationset.yaml` uses, so a PR preview environment
behaves like every other tier instead of a bespoke, Kustomize-only path:

- **Source 0**: `airframe`'s `charts/airframe-application`, pinned version, with a
  `helm.valuesObject` stamping `appName`/`cluster`/`envName` (`pr-<number>`) and
  `rollout.image.{repository,tag}` per-PR - these three fields can't be overridden by a
  developer's own values file, since `valuesObject` wins over `valueFiles`.
- **Source 1**: a `directory`-only, `ref: appsrc` source into the app's own repo, pinned
  to that PR's `head_sha` (not `main`), so `platform/pr-env.yaml` (see below) is read
  from the PR branch itself - editing that file inside a PR customizes that PR's own
  preview environment.

**`platform/pr-env.yaml`, not `platform/envs/pr.yaml`.** The lower-env tier's own
`lower-envs-applicationset.yaml` already watches `platform/envs/*.yaml` (a `git: files:`
generator) for the dev-tier lower env - a file at `platform/envs/pr.yaml` would also
match that glob and spawn a bogus static `pr` lower env on top of this feature's real
per-PR-numbered ones. `platform/pr-env.yaml`, one level up, avoids the collision. This is
a single, static file per app (unlike `platform/envs/<name>.yaml`, one file per lower
env) - a PR number isn't a fixed filename, so per-PR identity is stamped by the
ApplicationSet's own `valuesObject` instead of by which file matched.

`k8s/ephemeral/` is gone entirely from onboarded app repos - `platform/pr-env.yaml`
replaces it.

## Image tagging: sha-only for PR builds

`build-image.yaml` (the shared catalog Task every flow's build stage uses) normally tags
`<image-repo>:<version-from-package.json-or-pom.xml>-<7-char-sha>`. ArgoCD's
`pullRequest` generator has no way to read a PR's `package.json` at manifest-generation
time - it only exposes `.number`/`.head_sha`/labels - so it can't predict that tag, only
a tag derived from `.head_sha` alone.

Fixed with a new `pr-tag-only` param, threaded `build-image.yaml` <- `build.yaml` <-
`deliver-onboarding-files.yaml`'s generator: for a `pull_request`-triggered flow (the
same case that already appends `-pr` to the image repo), the generator also sets
`pr-tag-only: "true"`, and `build-image.yaml` tags the image as a bare 12-char sha with
no version prefix - exactly what `ephemeral-envs.yaml`'s ApplicationSet computes
independently via `{{ trunc 12 .head_sha }}`.

**A working PR image requires a `pull_request`-triggered flow to exist at all** - see
"Onboarding" step 1 below. Nothing generates one automatically; `cicd.yaml`'s `pipelines:`
map needs an explicit entry with `trigger.event: pull_request`.

## Namespace cleanup: TTL sweep, not cascade-delete

Real, deliberate trade-off, not an oversight: `airframe-application`, like the lower-env
tier, never renders a `Namespace` object of its own - it relies on
`syncOptions: [CreateNamespace=true]`. `CreateNamespace=true` is a sync-time convenience,
not something that adds the namespace to the Application's own tracked-resource set, so
`resources-finalizer.argocd.argoproj.io`'s cascade-delete removes the
`Rollout`/`Service`/etc. it actually tracks but leaves the now-empty namespace behind -
confirmed live, and the exact bug the old cd-pipelines system hit (see below) and never
fixed.

Accepted here instead of reverting to a tracked `Namespace` object (which the shared
`airframe-application` chart doesn't support): `ephemeral-envs.yaml`'s
`managedNamespaceMetadata` stamps `hangar.io/ephemeral-env: "true"` on every PR
namespace it creates, so `pr-namespace-ttl-sweep-cronjob.yaml`'s existing 24h sweep (see
below) becomes the real cleanup path for the empty namespace left behind, not just a
backstop for a stuck finalizer. Means a closed PR's namespace can sit empty for up to
24h rather than disappearing the instant the PR closes - a real, visible difference from
the old design, judged worth it for standardizing on the same chart every other tier
uses.

## What's different from the old system

Live analysis of the old system's `appset-ephemeral-envs-pr.yaml` and siblings found the
ApplicationSet `pullRequest` generator itself sound and worth porting as-is, but two
supporting pieces were real, confirmed failures in production, not just theoretical
risk - fixed here rather than repeated:

- **Cleanup was explicitly disabled.** Commit `c54ce8f5` in the old repo comments out the
  finalizer that would have cascade-deleted a PR's namespace, while the comment directly
  above it still describes the now-disabled behavior. Every PR-based ephemeral namespace
  that system ever created was orphaned forever - there was no TTL backstop either (the
  one that existed was gated to branch-based envs only). Here: the finalizer is verified
  live (above) and there's a TTL backstop regardless (below) - not a single point of
  failure this time.
- **RBAC used `cluster-admin`** on the pipeline ServiceAccount managing ephemeral envs (a
  narrower ClusterRole existed in the same chart but was dead code, never bound to
  anything). Here: no Tekton pipeline ServiceAccount needs any new RBAC at all for this
  feature - ArgoCD's own already-trusted `applicationset-controller` does all
  namespace/Application creation. The only new RBAC is the token-refresher CronJob's
  narrowly-scoped access to exactly one named Secret, in the app's own `-cicd` namespace
  (see below - this used to be a cross-namespace grant into `argocd`, moved same-namespace
  once the ApplicationSet itself moved there too).

Also different: images are tagged per-PR-per-SHA (see "Image tagging: sha-only for PR
builds" above), so the ApplicationSet template's `rollout.image.tag` valuesObject
override points at a real, already-pushed image, not a shared mutable tag or a
GitHub-Actions-workflow-file-commit trick like the old system used.

## Credential for the ApplicationSet generator

ArgoCD's `pullRequest.github.tokenRef` reads from a static Secret - the generator's own
controller polls GitHub independently, not through a live per-call exchange like
`open-release-pr.yaml` uses for the release stage. Rather than a new long-lived PAT,
`pr-token-refresher-cronjob.yaml` runs every 20 minutes (comfortably inside a GitHub App
installation token's 1-hour lifetime) as a dedicated `pr-env-token-refresher`
ServiceAccount in the Application's own namespace, calls the same
`/github-installation-token` endpoint built for the release stage, and writes the result
into `<app-name>-pr-generator-token` - the only thing it's allowed to touch there
(`create` unscoped since the Secret doesn't exist yet the first time, `get`/`update`/
`patch` scoped by `resourceNames` after that).

**The ApplicationSet and this Secret both live in the app's own `<type>-<app-name>-cicd`
namespace, not the ArgoCD instance's own control-plane namespace.** Originally the
ApplicationSet (like every other Application/ApplicationSet on the instance managing it)
was pinned to that instance's own namespace, which forced the Secret there too, which in
turn meant the refresher CronJob (running in the app's own namespace) needed a
cross-namespace RBAC grant just to write one Secret it didn't otherwise have any business
touching. ArgoCD's ApplicationSet-in-any-namespace feature removes that:
`argocd-apps-install`'s `argocd-cmd-params-cm` sets `application.namespaces` and
`applicationsetcontroller.namespaces` to the `*-cicd` glob (see
`gitops-cluster-dev/01-argocd-platform/argocd-apps-install`), and the per-tenant
`AppProject` (`templates/argocd/appproject.yaml`, rendered into `argocd-apps` specifically
for this reason) lists its own namespace in `sourceNamespaces` - together these let the
ApplicationSet, the Secret, and the refresher's RBAC all live in the one namespace the
chart already owns, no cross-namespace grant needed.

This is deliberately the `argocd-apps` instance, not `argocd-platform`: PR-based ephemeral
environments are app-owner/PR-driven work, which matches `argocd-apps`'s stated role
(`idp/docs/gitops-strategy.md` §2 - "watches per-app Applications generated by an
ApplicationSet"), not the cluster-admin-scoped `argocd-platform`. An earlier version of
this briefly lived on `argocd-platform` instead (2026-08-23, chasing an unrelated
SCM-providers bug); moved once it was clear nothing else on that instance needed the
wider watch - the release stage's own Applications (`release-application.yaml`) live
directly in `argocd-platform`'s namespace and were never affected either way.

This also turns on ArgoCD's `tokenRef` strict mode
(`applicationsetcontroller.enable.tokenref.strict.mode`) cluster-wide, which is why the
Secret needs the `argocd.argoproj.io/secret-type: scm-creds` label (set by the refresher
CronJob at creation time) - without it, an ApplicationSet living outside `argocd` could
otherwise read any Secret in its own namespace via `tokenRef`, not just ones meant for
it. Confirmed live: before the label was in place, the applicationset-controller
rejected the old (pre-migration) Secret outright with `secret must have label
"argocd.argoproj.io/secret-type"="scm-creds"`, not a silent fallback.

This required one real change to `token-review-interceptor`: `verifyAppOwnsRepo`
(then still named `verifyTenantOwnsGitOpsRepo`) only authorized an Application to mint a
token for its own `gitops-<app-name>` repo. Extended to also match the Application's app
repo directly (the generator needs to list PRs on `nodejs-demo-app` itself, not a gitops
repo) - authorized the same way, via the Application's own `Repository` CR, just matching
the app-repo name directly instead of requiring the `gitops-` prefix. Verified live post-rebuild: app-repo
token request succeeds, the existing gitops-repo path still works unchanged, and a
request for an unrelated repo still gets rejected.

## TTL backstop (now the real cleanup path, not just a backstop)

`pr-namespace-ttl-sweep-cronjob.yaml` is a single shared, platform-level CronJob (applied
once, not per Application) in `platform-system`, running every 30 minutes: lists every
namespace labeled `hangar.io/ephemeral-env=true` across all Applications and deletes any
older than `TTL_HOURS` (24) by `metadata.creationTimestamp`. Originally built purely as a
backstop to the finalizer (a namespace surviving 24h despite the finalizer firing is far
more likely to be something stuck than a still-legitimately-open PR, so no live GitHub
check is needed here, just age + the label) - since the 2026-08-24 move to
`airframe-application` (see "Namespace cleanup: TTL sweep, not cascade-delete" above), this is
now the mechanism that actually removes every PR namespace, not just the rare stuck one.
RBAC is cluster-scoped by necessity (`Namespace` has no namespaced form) but deliberately
narrow: `list`/`get`/`delete` on `namespaces` only.

The label this sweep selects on (`hangar.io/ephemeral-env: "true"`) is stamped by
`ephemeral-envs.yaml`'s own `managedNamespaceMetadata` at sync time, not by a manifest in
the app's own repo (that was true only under the old Kustomize-based design, where
`k8s/ephemeral/namespace.yaml` set it). The old cd-pipelines system had an equivalent
sweep for its branch-based ephemeral envs but never extended it to PR-based ones - this
platform's sweep has covered both since day one, specifically so that gap can't recur.

## A real gap this surfaced: newly-created GHCR packages default to private

The very first push to a new `image-repo` name (e.g. `nodejs-demo-app-pr`, the first time
any PR ever builds) auto-creates that GHCR package as **private**, regardless of the
linked repo's own visibility - confirmed live via a real `403 Forbidden` fetching an
anonymous pull token, even though the sibling `nodejs-demo-app` package (already public)
pulls with no pull secret anywhere. This is a one-time, per-image-repo-name event, not a
per-build or per-PR one - GHCR doesn't reset visibility on subsequent pushes to an
already-existing package. Fix: flip visibility to public once, right after the first-ever
build of a new app's `-pr` image (see step 6 below) - a one-time onboarding step, same
category as installing the PaC App or configuring branch protection, not new platform
code. (Automating this via the GitHub Packages API is possible but wasn't worth the added
`packages:write` App-permission scope and per-push API call for something that only ever
needs to happen once per app.)

## Onboarding an Application for ephemeral environments (per app)

One-time setup per app, same spirit as the release stage's onboarding steps.

1. **Push `nodejs-demo-app`'s new `platform/pr-env.yaml`** (see "Deploying through
   airframe-application, not raw Kustomize" above for its shape) - no PR needed unless the
   repo has branch protection configured (it doesn't currently, unlike
   `gitops-nodejs-demo-app`).

1a. **Nothing to add here anymore.** `ephemeralEnvironments.pullRequest.enabled: true` is
   now the whole switch - `deliver-onboarding-files.yaml`'s onboarding-resync mechanism
   synthesizes a `pull_request`-triggered, build-only flow automatically (base branch
   reused from whichever `push`-triggered flow already exists, falling back to `main`;
   labels reused from `ephemeralEnvironments.pullRequest.labels`, defaulting to
   `["preview"]`) whenever no explicit `pipelines.pr-build` is declared, and regenerates
   `.tekton/flow-pr-build.yaml` from it. Declare `pipelines.pr-build` explicitly only to
   override the default shape:
   ```yaml
   pipelines:
     pr-build:
       trigger: { source: git, event: pull_request, branch: main, labels: ["preview"] }
       steps:
         - stage: build
   ```
   Before this synthesis existed (fixed 2026-08-24), this flow was required, not
   optional - see "Image tagging: sha-only for PR builds" above. Without it,
   `ephemeralEnvironments.pullRequest`'s own ApplicationSet had nothing that ever
   produced the image it expects, and every preview pod sat in `ImagePullBackOff` forever
   (found live 2026-08-24 - the same incident that motivated automating this instead of
   just documenting it more clearly). The onboarding-resync mechanism still only reacts
   to a real `cicd.yaml` diff landing on `main`, and only updates `main`'s own `.tekton/`
   copy, so a long-lived feature/PR branch needs `main` merged back into it before its own
   `.tekton/flow-pr-build.yaml` picks up any later fix to this generator (confirmed live,
   cost real time to work out: pushes to an open PR's branch alone never re-triggered it).

2. **Apply the ApplicationSet + AppProject template**, with `<APP_NAMESPACE>`, `<APP_NAME>`,
   `<APP_REPO_URL>`, `<GITHUB_OWNER>` substituted:
   ```
   sed -e 's#<APP_NAMESPACE>#platform-cicd-demo#g' -e 's#<APP_NAME>#nodejs-demo-app#g' \
       -e 's#<APP_REPO_URL>#https://github.com/jfillman/nodejs-demo-app#g' \
       -e 's#<GITHUB_OWNER>#jfillman#g' \
     charts/glidepath-app/templates/argocd/ephemeral-envs.yaml | kubectl apply -f -
   ```
   This renders a second `AppProject` of the same name into `argocd-apps` alongside the
   release stage's own `AppProject` in `argocd` (`templates/argocd/appproject.yaml` now
   renders one per instance, conditionally, when each stage is enabled) - not a competing
   copy of the same object, since each ArgoCD instance only ever looks for an `AppProject`
   inside its own control-plane namespace.

3. **Apply the token-refresher CronJob**, same substitutions:
   ```
   sed -e 's#<APP_NAMESPACE>#app-nodejs-demo-app-cicd#g' -e 's#<APP_NAME>#nodejs-demo-app#g' \
       -e 's#<GITHUB_OWNER>#jfillman#g' \
     charts/glidepath-app/templates/argocd/ephemeral-envs.yaml | kubectl apply -f -
   ```

4. **Apply the TTL sweep CronJob** - once per cluster, not per Application:
   ```
   kubectl apply -f charts/glidepath-control-plane/templates/argocd/pr-namespace-ttl-sweep-cronjob.yaml
   ```

5. **Bootstrap the generator's token Secret once**, rather than waiting up to 20 minutes
   for the CronJob's first scheduled run:
   ```
   kubectl create job --from=cronjob/nodejs-demo-app-pr-token-refresher \
     nodejs-demo-app-pr-token-refresher-bootstrap -n platform-cicd-demo
   ```

6. **After the first real PR build completes**, flip the new `<app-name>-pr` GHCR
   package's visibility to public (Package settings -> Change visibility) - see "A real
   gap this surfaced: newly-created GHCR packages default to private" above. One-time,
   not needed again for this app.

## PR comment: `charts/glidepath-pr-preview-notify`

An ArgoCD `PostSync` hook Job, sourced as a third `sources:` entry on
`ephemeral-envs.yaml`'s per-PR `Application` (not a Tekton Task - the build stage
finishes before ArgoCD has even attempted to deploy, so it can't know whether the sync
will succeed; not a conditional resource inside the shared `airframe-application` chart -
that chart is used by every tier, and nothing outside this one `ApplicationSet`
template ever references `glidepath-pr-preview-notify`, which is what actually guarantees this
can't fire for a regular env). Posts/edits one PR comment (hidden-marker dedup, same
pattern as `comment-pr-check-result.yaml`) with the namespace, the ArgoCD Application
link, and a preview URL - or, since this platform has no ingress/DNS yet, a
`kubectl port-forward` fallback instead. Two values
(`platformIdentity.previewIngressBaseDomain`/`argocdUIBaseURL`, both empty by default)
switch the comment over to real clickable links with no template change once real
ingress exists.

Needs the broker's `verifyAppOwnsRepo` to recognize a PR namespace as authorized for its
own app's repo (`platform/broker/cmd/token-review-interceptor/main.go`) - a PR namespace
has no `Repository` CR of its own, only the app's shared `-cicd` namespace does.

## Access

This platform's clusters have no ingress/DNS by default (an already-documented gap), so
there's no clickable preview URL to post as a PR comment - access is
`kubectl port-forward` into the PR's namespace, same as every other environment. Unlike
the old Kustomize-based design, the Service name is NOT per-PR-suffixed - only the
destination namespace differs between PRs (`airframe-application` doesn't rename resources
per environment, it deploys the same fixed name into a different namespace each time):
```
kubectl port-forward -n app-nodejs-demo-app-pr-<number> svc/nodejs-demo-app 8080:80
```

## Verification

- Label a real PR `preview` on `nodejs-demo-app`. Within ~180s (the generator's
  `requeueAfterSeconds`), `kubectl get application.argoproj.io -n app-nodejs-demo-app-cicd`
  should show `nodejs-demo-app-pr-<number>` (generated Applications land in the same
  namespace as their ApplicationSet - not `argocd`, see "Credential for the
  ApplicationSet generator" above), and `kubectl get ns app-nodejs-demo-app-pr-<number>`
  should exist with a running pod on that PR's own uniquely-tagged image.
- `kubectl port-forward` into it and confirm the PR's actual code is running (not just
  any deploy).
- Close the PR (or remove the label) and confirm the `Application` is gone within the
  poll interval, not just `Terminating`. The namespace itself is NOT expected to
  disappear immediately - see "Namespace cleanup: TTL sweep, not cascade-delete" above -
  confirm instead that it's empty (no `Rollout`/`Service`/etc left in it) and that it
  actually gets swept within the TTL window, not orphaned forever.
- Security check: from `platform-cicd-demo`'s SA, request a token for a repo the Application
  doesn't own via `/github-installation-token` - should still be rejected (403), same as
  the release-stage check.
