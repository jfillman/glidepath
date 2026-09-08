# ADR-0007: Testkube CE in one shared namespace, not one per tenant

## Context

The `test` stage needed a real test-execution backend - Phase 1 only ever shelled out
to an app's own `./integration-test.sh` (`run-testworkflow.yaml`'s own header
called this out as deferred scope from the start). Testkube was the natural choice
(the old platform this one replaces used it), and the first design ran each
Application's TestWorkflows inside that Application's own `<type>-<appName>-cicd`
namespace, so a workflow could read that namespace's `app-secrets` directly with zero
new secret plumbing - the same trust boundary `pipeline-runner` already lives in.

That design didn't survive contact with the real chart. Live on kind-dev, 2026-08-24:
Testkube's `executionNamespaces`/`multinamespace` config - the feature that lets a
workflow run outside Testkube's own install namespace - is gated in Testkube's own
source as Pro/Enterprise-only. `cmd/api-server/main.go`'s own comment reads
`// Pro edition only (tcl protected code)`, immediately above a hard `os.Exit` whenever
that config is set outside a licensed Control Plane connection. The api-server
crash-looped on exactly this check the moment the first design was applied. Public docs
implied otherwise; the source code didn't. This document tracks the design that
actually shipped, not the one first attempted - see this repo's git history on
`gitops-cluster-dev`'s `50-platform-cicd/testkube/` for the abandoned per-namespace
attempt.

## Decision

Testkube CE (standalone agent, no Control Plane, genuinely free) installs into one
shared `testkube` namespace on the cluster
(`gitops-cluster-dev/50-platform-cicd/testkube/application.yaml`). Every onboarded
Application's TestWorkflows run there, not in that Application's own `-cicd` namespace.

Application secrets reach a TestWorkflow a different way: `run-testworkflow.yaml`
re-materializes the triggering namespace's own `app-secrets` into a per-Application
placeholder Secret (`<appName>-app-secrets`) inside the shared `testkube` namespace,
immediately before each run, and blanks it again immediately after - shrinking the
window a same-namespace TestWorkflow from a different Application could reference it.
The workflow's own YAML (`platform/<name>.yaml` in the app repo) references it via a
literal placeholder, `secretKeyRef.name: __APP_SECRETS_NAME__`, substituted for the
real name at apply time - the workflow file itself never hardcodes or needs to know
this Application's own name.

RBAC for the one shared namespace splits two ways, deliberately not identically:

- **Secrets** (`glidepath-app`'s `templates/testkube/rbac-and-secret.yaml`):
  resourceNames-restricted to exactly this Application's own placeholder Secret, and
  crucially `get/update/patch` only, never `create` - the placeholder is pre-seeded by
  this same chart. Kubernetes RBAC can restrict `get`/`update`/`patch` to one named
  object but has no equivalent for `create` (there's no existing object yet to check a
  name against), so granting `create` on secrets in a namespace every tenant shares
  would let any one tenant's `pipeline-runner` mint a secret under another tenant's
  naming convention. Pre-seeding closes that off entirely.
- **TestWorkflow/TestWorkflowExecution** (`glidepath-control-plane`'s
  `templates/testkube/rbac.yaml`, one shared Role, bound per-tenant by each
  Application's own chart): deliberately NOT resourceNames-restricted. Names are
  developer-chosen and self-service - a file dropped in the app repo's own `platform/`
  folder, no operator involved - so a fixed name allow-list isn't possible without
  reintroducing an operator step for every new test name, defeating the point. Accepted,
  lower-severity residual risk: a compromised or careless pipeline could
  create/overwrite another tenant's TestWorkflow object name. `metadata.name` is always
  force-rewritten to `<trusted-app-name>-<name>` before applying (app-name comes from
  the PipelineRun's own platformIdentity-derived param, not anything a developer's
  commit controls) as a correctness backstop, not an RBAC one - it stops a well-behaved
  pipeline from colliding by accident, not a compromised one from colliding on purpose.

`pipeline-runner` (`glidepath-app`'s `templates/identity/pipeline-runner.yaml`)
gained one new in-namespace rule too: `get` on its own `app-secrets`, resourceNames-
restricted, needed to read the values it forwards - previously that Secret only ever
reached a Task via a volume mount the Tekton controller set up, never an API read by
the pipeline-runner identity itself.

## Consequences

- Testkube stays genuinely free/open-source (no Control Plane, no license, no seat
  cost) - the tradeoff for that is a real, structural one: any TestWorkflow in the
  shared namespace could in principle reference any other tenant's placeholder Secret
  by name if it chose to, since Kubernetes doesn't restrict which Secret names a pod
  spec may mount by the pod's own identity, only who may read/write the Secret object
  via the API. The blank-before/blank-after materialization narrows the exposure window
  to the run itself; it doesn't close the gap outright. Acceptable for this platform's
  actual threat model (one owner's own Applications, not mutually hostile tenants) -
  revisit if that ever stops being true.
- A future upgrade to Testkube Pro/Enterprise (a real Control Plane connection) would
  make the original per-namespace design possible again - this decision is reversible,
  not a permanent ceiling, just the honest floor of the free tier as of Testkube 2.12.2.
- `glidepath-app` now renders resources outside its own `<type>-<appName>-cicd`
  namespace for the first time (into the shared `testkube` namespace) - a deliberate,
  narrow exception to that chart's otherwise-universal "only ever touches its own
  namespace" rule, documented at the point it happens
  (`templates/testkube/rbac-and-secret.yaml`'s own header), not silently.
- `platform/<name>.yaml` becomes a new, self-service, developer-owned convention in
  every app repo (alongside `cicd.yaml`), on the same footing as `cicd.yaml` -
  never regenerated by onboarding boilerplate, always hand-authored.
