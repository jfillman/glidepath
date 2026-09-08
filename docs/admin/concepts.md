# Concepts and terminology

"Tenant" was used throughout this platform's early history in a multi-tenant-SaaS
sense, but auditing every real usage (72 files, both prose and actual Tekton
parameter names) found **zero** place where it meant anything other than "this one
Application's own execution context." There was never a real distinct concept behind
the word; it was "Application" wearing a different name. This doc retires "tenant" as
a working term in platform-cicd and defines what replaces it.

## Application (App)

The one unit everything in this platform is scoped to. An Application has, always in a
strict 1:1 relationship:

- One source code repository (`platformIdentity.appRepoUrl`)
- One optional GitOps repository (`platformIdentity.gitopsRepoUrl`, only needed if
  `pipeline:` declares a `release` stage) - by convention named `gitops-<app-name>`
- One CI/CD execution namespace (the **App namespace**, see below)
- One `pipeline-runner` identity (a ServiceAccount + RBAC, scoped to exactly that one
  namespace)
- One `cicd.yaml`

There is no current concept of one higher-level entity (a "tenant," a "team," an "org")
owning *multiple* Applications. If Dream IDP ever wants that - a real grouping of several
Applications under one team for billing/RBAC/reporting purposes - that is a **new,
higher-level concept introduced there**, layered on top of Applications, not something
retrofitted into platform-cicd's namespace-per-Application model. Don't reintroduce
"tenant" to platform-cicd to anticipate it.

## Type

Every Application has a `type` - `app` (a regular application) or `infra` (a shared/
platform-adjacent service onboarded with its own pipeline, e.g. a future shared database
operator) - more types added as real cases appear, not invented speculatively. `type`
classifies *what kind* of Application this is; it does not describe something bigger
than an Application. An `infra`-type onboarding still gets the full, standard 1:1:1:1
relationship above - same chart, same mechanism, different blast-radius expectations.

## Namespace pattern: `<type>-<app-name>-<env>`

One flat pattern, not a hierarchy - every namespace this platform creates for an
Application is `<type>-<app-name>-<env>`, where `env` is just whichever environment that
particular namespace represents. The **App namespace** (where the Application's
pipelines execute) is the special case `env = cicd`: `<type>-<app-name>-cicd`. Deploy/
staging/PR namespaces are **siblings** of the App namespace under this same pattern, not
nested beneath it - `env = dev`/`staging`/`pr-<number>` produce
`<type>-<app-name>-dev`, `<type>-<app-name>-staging`, `<type>-<app-name>-pr-<number>`,
**not** `<type>-<app-name>-cicd-dev`. A deploy namespace has nothing to do with "cicd"
conceptually - it's where the Application *runs*, not where its pipeline runs - so
baking "cicd" into its name would be a real naming smell, not just redundant.

`charts/glidepath-app`'s `glidepath-app.envNamespace` helper computes any of
these from `platformIdentity.type` + `platformIdentity.appName` + a given `env` value -
nothing is a separately-set, independently-typed field that could drift from the
convention.

## App identity

The `pipeline-runner` ServiceAccount (plus its Role/RoleBinding) created in an
Application's own namespace - the only identity that ever creates PipelineRuns for that
Application, and the identity the shared broker is narrowly, individually authorized to
impersonate (never a cluster-wide grant). One per Application, matching the 1:1 model
above - never shared across Applications.

## Where this shows up

- **Helm**: `charts/glidepath-app` (renamed from `platform-cicd-tenant`) -
  `platformIdentity: {appName, type, gitopsRepoUrl, appRepoUrl, githubOwner}`. No
  `tenantNamespace` field anymore - see the computed App namespace above.
- **Catalog Tekton params**: every Pipeline/Task parameter that used to be named
  `tenant` (it carried the namespace value) is now `app-namespace` - a real, functional
  rename across catalog Pipeline/Task YAML, not just a doc/comment change. Kept distinct
  from the pre-existing `app-name` parameter (the human-readable name, e.g.
  `nodejs-demo-app`) - the two params mean different things and both still exist.
  `deploy-manifests.yaml` in particular no longer derives the deploy-target namespace by
  suffixing `app-namespace` with `-<env>` (that reproduced the old, now-corrected
  base-plus-suffix bug) - it takes a separate `app-type` param and builds
  `<app-type>-<app-name>-<env>` directly, the flat peer pattern described above.
- **Labels**: `hangar.io/app` is the only identity label now - `hangar.io/tenant`
  is dropped (it was always redundant with `hangar.io/app`, since the two values were
  always identical in practice). See [naming-conventions.md](naming-conventions.md).
- **This is a chart-level rename, not yet a live one**: nothing has been `helm install`ed
  against the real cluster with these charts yet (see the Phase 3 item 7 status note in
  the build plan), so the currently-live catalog in `platform-catalog` still uses the old
  `tenant` param name, and the real `nodejs-demo-app`/`gitops-nodejs-demo-app` repos'
  `.tekton/*.yaml` files still pass `{ name: tenant, ... }` to match it. This rename
  takes effect for real the next time the catalog chart is actually deployed - don't
  hand-edit those real repos' `.tekton/` files to the new param name before that happens,
  or they'll stop matching what's actually live.
