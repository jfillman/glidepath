# Catalog versioning, testing, and safe deployment

Before Phase 3 item 7, the shared catalog (`catalog/tasks|pipelines|stepactions`, now
`charts/glidepath-catalog`) had zero versioning: every catalog Pipeline resolves its
Tasks via Tekton's `resolver: cluster` by bare name against whatever happened to be
`kubectl apply`'d into the `platform-catalog` namespace at that moment - no git tags, no
CI, no staging, no rollback path. Given every real Application's pipeline run resolves the
catalog live, a bad edit took effect for every Application immediately, with no way back short
of hand-reverting and re-applying.

## What changed: the catalog is now a real Helm release

`charts/glidepath-catalog` wraps the same Task/Pipeline/StepAction YAML that always
lived under `catalog/` (a "nearly free" conversion - every file was already flat,
unparameterized, valid standalone YAML). Every real cluster's `glidepath-catalog`
Application tracks `main` with `selfHeal: true` (see
`gitops-cluster-dev/50-platform-cicd/glidepath-catalog/application.yaml`) - a merge
to `main` is a live production change, immediately. Rollback is `git revert` the merge
commit, not `helm rollback` - matching [ADR-0004](adr/0004-gitops-only-release.md)'s
"promotion is ordinary git history" principle rather than a side-channel release
history only `helm` knows about.

This is **Option A**: one live copy in `platform-catalog`, shared by every Application -
a revert affects everyone at once, there's no per-app pinning. That's a real
limitation, accepted deliberately for now (see "Option B" below for the alternative and
why it's not built yet).

## Testing a change before it reaches every Application

Since the chart needs zero real templating, a **second, temporary ArgoCD Application
tracking your feature branch instead of `main`** gives a genuine, live staging target
with zero OCI-bundle machinery. Copy `glidepath-catalog/application.yaml`, and in
the copy:

- rename it (e.g. `glidepath-catalog-canary`)
- set `spec.source.targetRevision` to your branch, not `main`
- set `spec.destination.namespace` to `platform-catalog-canary`
- drop `syncPolicy.automated` (sync by hand while iterating)

Apply that Application to the cluster, point one dedicated canary Application's
`platformIdentity.catalogNamespace` (see the app chart's `values.yaml`) at
`platform-catalog-canary` instead of the default `platform-catalog`, and run a real
build→test→deploy through it. Once you're confident, merge your branch to `main` - the
real `glidepath-catalog` Application's own `selfHeal` picks it up automatically, no
separate promote step. Delete the temporary canary Application when done.

## What CI actually checks (and what it can't)

`.github/workflows/catalog-ci.yaml` runs on every PR touching the catalog chart:
`helm lint`, a full `helm template` render (catches YAML/Go-template syntax errors - the
most common real mistake when hand-editing Tekton YAML, including the PaC-template-
variable-collision class of bug hit live while building this chart - see that file's own
`{{ repo_url }}` escaping note), and a structural sanity check (every rendered resource
has a name, an expected `kind`, no stray unrendered template syntax).

What CI **cannot** do: the platform's clusters are local, with no public endpoint, so
GitHub-hosted runners can't reach them for a real `kubectl apply --dry-run=server` or an
actual canary pipeline run. That verification is real and still required before
merging - it's what the temporary canary Application above is for. Don't treat a green
CI run alone as "safe to merge" - it means the YAML is well-formed, not that a real
pipeline still works.

## Option B: Tekton bundle resolver (target state, not built yet)

Tekton's own idiomatic mechanism for exactly this problem is the **bundle resolver**
(OCI-packaged, tag-versioned Task/Pipeline bundles) - already proven working in this
codebase for `git-clone` via the Hub resolver, which is bundle-backed under the hood.
It gives genuine per-app/per-pipeline version pinning (each `.tekton/*.yaml`
references an explicit `bundle: ghcr.io/.../platform-catalog:v1.2.0`), and staging
becomes as simple as a different OCI tag rather than a whole second namespace.

This isn't just lower priority than Option A - it's **sequence-blocked**. Migrating every
onboarding-template `.tekton/*.yaml`'s resolver shape (`cluster`+namespace →
`bundle`+OCI ref) across every already-onboarded Application is exactly the re-sync/staleness
problem [onboarding-mechanics.md](onboarding-mechanics.md#keeping-onboarding-boilerplate-in-sync) solves -
attempting B before that mechanism existed would mean hand-editing every Application's
boilerplate again, reproducing the manual-toil problem this whole effort exists to kill.
Now that the re-sync mechanism exists, Option B is a real, buildable next step - not
attempted in this pass.
