# Onboarding mechanics

How a repo actually gets from "nothing" to "a real, ArgoCD-managed CI/CD Application" -
the operator-facing counterpart to [../user/install-guide.md](../user/install-guide.md).

`cicd.yaml` is the app chart's own values file: every resource the platform creates
for an Application conditionally renders off what `cicd.yaml` declares (see
`charts/glidepath-app/templates/_helpers.tpl`'s `glidepath-app.hasStage`).

Onboarding is ArgoCD-managed end to end. An `ApplicationSet`
(`charts/glidepath-control-plane/templates/argocd/tenant-onboarding-applicationset.yaml`)
generates one `Application` per tenant from a git `files` generator scanning
`tenants/*/identity.yaml`. Onboarding a new Application, or changing an existing one's
`platformIdentity`, is "add/edit a file in `tenants/`, get it reviewed" - never a manual
CLI command.

## One-time platform setup

The control plane and catalog charts are cluster-wide singletons, installed once per
cluster - see [installation.md](installation.md).

## Onboarding one Application

1. **Install the platform's GitHub App on the app's repo** - the one manual,
   GitHub-side prerequisite. The Pipelines-as-Code `Repository` CR itself is rendered
   by the chart (`charts/glidepath-app/templates/pipelines-as-code/repository.yaml`,
   gated on `platformIdentity.registerPipelinesAsCode`, default `true`) - not a separate
   manual step.

   **If `cicd.yaml` will declare a `release` stage, the App also needs installing on the
   `gitops-<app-name>` repo** ([release.md](release.md) step 3) - a second, easy-to-miss
   repo, not covered by installing on the app repo alone. This bites private gitops
   repos specifically: a GitHub App installed with "selected repositories" doesn't
   automatically gain access to a new repo regardless of its visibility, and
   `deliver-onboarding-files`'s push to that repo fails outright until the App is
   explicitly added there. Confirmed live 2026-08-22 against a real private
   `gitops-checkout-api` repo.

2. **The developer adds `cicd.yaml`** to their repo root. The `tenant-onboarding`
   `ApplicationSet` reads it live from there - a push to `main` takes effect on
   ArgoCD's next sync, with no separate copy to keep up to date.

3. **An operator adds `tenants/<app-name>/identity.yaml`** to this repo (via a
   reviewed PR, never the app repo) - the handful of infra-identity values that aren't
   pipeline configuration, so `schemas/cicd.schema.json` never carries them and a
   developer's own commit can never influence them (they flow into governance
   policy-config lookups, secret resolution, and which `gitops-<app>` repo a release
   PR targets):

   ```yaml
   platformIdentity:
     appName: <app-name>
     type: app  # or infra - see naming-conventions.md
     gitopsRepoUrl: https://github.com/<org>/gitops-<app-name>  # "" if no release stage
     appRepoUrl: https://github.com/<org>/<app-name>            # always required
     githubOwner: <org>
     catalogNamespace: platform-catalog
     # extraGitopsRepos: [https://github.com/<org>/gitops-cluster-<name>]  # optional
   ```

   All six top-level fields must be present (even as `""`) - the `ApplicationSet`'s own
   Go-template evaluation errors on a genuinely-missing key. `appRepoUrl` can't be
   `""` for any tenant: the `ApplicationSet`'s second source reads `cicd.yaml` live
   from exactly this URL. There's no `tenantNamespace` field - the execution namespace
   is *computed* from `type`+`appName` as `<type>-<app-name>-cicd`, so it can't drift
   from convention (see [naming-conventions.md](naming-conventions.md)).

   `extraGitopsRepos` is optional - full URLs of extra gitops repos (beyond
   `gitops-<app-name>`) this app's CI needs write access to, e.g. `gitops-image-bump`
   for an app whose deploy target isn't its own `gitops-<app-name>` repo (a
   hand-deployed singleton writing into a shared `gitops-cluster-*` repo instead - see
   `docs/backstage-design.md` in the `idp` repo for a real example). Rendered as an
   extra `Repository` CR grant by `glidepath-app`'s `repository.yaml`. It's a list,
   so - unlike the six scalar fields above - it can't be forwarded through the
   `ApplicationSet`'s Helm `valuesObject` (`goTemplate`'s per-field substitution can't
   emit array items); the `ApplicationSet`'s third source $refs this whole
   `identity.yaml` file as an extra `valueFiles` entry instead, specifically so
   list-shaped fields like this one reach the chart. A real bug where this field
   silently vanished (missing from `valuesObject`, no third source yet) was found and
   fixed 2026-08-29.

   `platformIdentity` isn't a key `schemas/cicd.schema.json` permits -
   `additionalProperties: false` rejects any `cicd.yaml` that tries to define it. This
   is enforced structurally, not just by convention: the `ApplicationSet`'s Helm
   `valuesObject` (built only from this identity file) takes precedence over
   `valueFiles` (the app repo's live `cicd.yaml`, and this identity file itself) for
   any overlapping key - verified directly against a live cluster, not just a template
   diff.

4. **Provision the target Deployment first.** `deploy-manifests.yaml` patches an
   existing `Deployment` named `<app-name>` in `<type>-<app-name>-<env>` for each
   environment `cicd.yaml` declares - it doesn't create the Deployment or its
   namespace. Apply a minimal Deployment/Service by hand before step 5, or the sync
   fails outright (`env/deploy-rbac.yaml` needs the namespace to already exist to grant
   namespaced RBAC in it). If `cicd.yaml` also declares `release`, pre-seed the gitops
   repo too - see [release.md](release.md).

5. **ArgoCD installs the app chart** - nothing to run by hand. Once step 3's identity
   file lands on this repo's default branch, the `ApplicationSet` creates/syncs a
   three-source `Application` named `<app-name>-cicd`: one source is this platform's
   own chart, one reads `cicd.yaml` live from `appRepoUrl`, and one reads this same
   `identity.yaml` live from the tenants repo (see `extraGitopsRepos` above). This
   provisions
   `pipeline-runner` + RBAC, only the stage-transition Triggers `cicd.yaml` actually
   declares, env-deploy RBAC per declared environment, the build-cache PVC, the
   release AppProject/Application/DORA RBAC (if `release` is declared), the
   ephemeral-envs `ApplicationSet` (if enabled), the governance policy ConfigMap, and
   an `ExternalSecret` for `registry-credentials`. Editing `cicd.yaml` and letting
   ArgoCD auto-sync is how an Application's shape changes later - `prune: true` removes
   whatever a stage removal makes unnecessary.

6. **`.tekton/` boilerplate delivery.** A push to the app repo's `cicd.yaml` fires
   `onboarding-resync.yaml` (a real Pipelines-as-Code CEL extension,
   `"cicd.yaml".pathChanged()`), which opens a PR against the app repo (and gitops
   repo, if any) with the generated `.tekton/*.yaml` files. Merge it once - these files
   are never hand-edited. `git status --porcelain` inside that delivery step skips
   opening a PR when nothing changed, so this is safe to fire often.

   **Bootstrapping this for a brand-new app is automatic**, not a step you do by hand:
   `.tekton/onboarding-resync.yaml` is itself one of the files this mechanism delivers,
   so on a repo where `.tekton/` doesn't exist yet, no `cicd.yaml` push (past or future)
   can trigger it - PaC only matches against `.tekton/*.yaml` files already committed at
   the pushed ref. `templates/hooks/onboarding-resync-bootstrap.yaml` (an ArgoCD
   `PostSync` hook on this Application, same shape as `charts/glidepath-pr-preview-notify`'s own
   hook) closes this gap: it fires on every sync, checks whether
   `.tekton/onboarding-resync.yaml` already exists on the app repo, and only if not,
   creates the same bootstrap `PipelineRun` this section used to have you run by hand
   (see git history for the old manual command). Past the first real delivery, PaC's own
   `cicd.yaml`-push resync owns `.tekton/` and this hook is a no-op on every later sync.

7. Push to `main`. Watch the Grafana dashboard.

## Keeping onboarding boilerplate in sync

Two things used to go stale silently: an Application's `cicd.yaml` changing (nothing
re-rendered the app chart), and the platform's own onboarding templates changing
(nothing re-delivered already-onboarded repos' copies). Both are solved the same way
now - `onboarding-resync.yaml` fires on any `cicd.yaml` push and re-delivers current
templates regardless of which side went stale. Templates live in a ConfigMap
(`charts/glidepath-app/templates/configmaps/onboarding-templates.yaml`, one per
Application, not baked into the toolbox image), so an edit takes effect on that
Application's next `helm upgrade` - though it does mean updating N already-onboarded
Applications individually to propagate a template change.

The app chart's own resources (Triggers, RBAC) re-render on a `cicd.yaml` change too,
for the same reason: the `ApplicationSet`'s generated `Application` reads `cicd.yaml`
live from the app repo, so one push reaches both halves without a manual step.

## A real deadlock this mechanism can fall into (found and fixed live)

`onboarding-resync`'s own trigger file - the thing that would deliver a fix - is
*itself* one of the files this mechanism regenerates. If a required param gets added
to `onboarding-resync.yaml`'s Pipeline (or `release.yaml`'s) after a tenant already
onboarded, that tenant's stale trigger file omits the new param, so the very
PipelineRun that would regenerate it fails Tekton admission (`ParameterMissing`) before
running - nothing can self-heal from that state. Fixed by making
`onboarding-resync.yaml`'s params optional wherever derivable (e.g. `app-type` from
`app-namespace`'s own naming convention). **Lesson for next time a required param is
added to a Pipeline a stale committed trigger file might invoke**: give it a derivable
default, or every already-onboarded tenant needs one manual resync PipelineRun before
the fix reaches it on its own.

**A quieter sibling of the same bug, found live 2026-08-23**: `gitops-repo-url` was
already optional (`default: ""`), so it never caused the hard deadlock above - but it
had the same underlying gap, no derivable fallback, just a softer failure mode: a
tenant whose committed trigger file ever got a wrong/stale value (found via a real
tenant, `checkout-api`, whose `.tekton/onboarding-resync.yaml` was bootstrapped by
copying a donor app's files and never got this one field corrected) silently skips
gitops-repo delivery on *every* resync, forever, with only a log line
(`"gitops repo: no gitops-repo-url param given (release stage not declared),
skipping"`) as a clue - easy to misread as a real failure when it's actually a graceful,
permanent no-op. Fixed the same way as `app-type`: `deliver-onboarding-files.yaml`'s
`deliver-app-repo-files` step now derives `gitops-repo-url` from the
`gitops-<app-name>` convention whenever it's empty AND `cicd.yaml` actually declares a
release stage (checked across every flow's `steps`, not just each flow's first step -
`release` is typically the *last* stage in a chain). Same self-heal shape as `app-type`:
fixes the committed trigger file for *next* time, not the current run's own delivery -
merge the resulting PR and push `cicd.yaml` once more to see gitops-repo delivery
actually happen. **Same lesson applies more broadly**: any param here without a
derivable fallback can go silently stale forever, not just hard-deadlock - worth a
derivation whenever one's safely possible, not only when a missing param would fail
admission outright.
