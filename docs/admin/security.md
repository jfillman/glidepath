# Security model

A single place to see the platform's security posture end to end. Each section links
to the doc with the full depth - this page is the map, not the territory.

Every rule here is also in Hangar's [business rules register](https://github.com/jfillman/hangar/blob/main/docs/policy/business-rules.md), with the rules of every other component.
A change to a rule updates that page too.

## Threat model in one sentence

Many tenants (Applications) share one cluster and one control plane; a compromised or
buggy component belonging to one tenant, or to the platform itself, should never be
able to affect another tenant or escalate to cluster-wide control.

## No cluster-admin, no broad impersonation, anywhere

Every tenant gets namespace-scoped RBAC only. The shared CDEvents broker - the one
component every tenant's traffic passes through - holds no elevated cluster role:
`kubectl auth can-i --as=system:serviceaccount:<broker-ns>:<broker-sa> '*' '*'` is an
emphatic no. Where the broker needs to hand off to a tenant's own pipeline identity, it
does so via Kubernetes' own `impersonate` verb, granted per-Application as a single
namespaced `Role`/`RoleBinding` scoped to exactly that Application's `pipeline-trigger`
ServiceAccount (the one account that may create a PipelineRun, see
[Pipeline identities](#pipeline-identities)) - never a cluster-wide impersonation grant. See
[chaining.md](chaining.md) and [ADR-0002](adr/0002-cdevents-broker-tokenreview.md).

## Authentication without a platform-minted credential

The broker authenticates every caller via the Kubernetes `TokenReview` API against
that pod's own cluster-issued, audience-bound, short-lived ServiceAccount token - never
a credential the platform itself mints, stores, or rotates. Compromising the broker
doesn't hand out a reusable secret, since every trust decision is delegated to the
Kubernetes API server itself. See [chaining.md](chaining.md).

## Namespace isolation and its real limit

Every Application gets its own CI-control-plane namespace (`<type>-<app>-cicd`) and
its own per-environment namespace (`<type>-<app>-<env>`) - RBAC never spans them. The
CEL-filtered `Trigger` on the broker checks the calling namespace against the CDEvent's
declared source before matching, which is the actual cross-tenant trust boundary - not
NetworkPolicy. NetworkPolicy (default-deny + allow-from-same-namespace) is
defense-in-depth on top of that, and **only works if the cluster's CNI enforces it**
(Calico/Cilium) - a cluster running kindnet has this layer silently disabled. Know
which case your cluster is in before treating it as a security reference; see
[installation.md](installation.md).

## Supply chain: build to release

- **Image signing**: Tekton Chains signs every built image keylessly via a
  cluster-local Fulcio instance, with an independent trust root per cluster (never
  shared or copied between clusters) - see [image-signing.md](image-signing.md) and
  [ADR-0006](adr/0006-cluster-agnostic-bootstrap.md).
- **Provenance/attestation**: SLSA-shaped provenance is generated and verified before
  promotion - see [provenance-policy.md](provenance-policy.md).
- **Commit signing**: the `provenance` governance gate verifies the actual app-repo
  commit being promoted (not the machine-generated gitops PR commit) - see
  [commit-signing.md](commit-signing.md). Two paths:
  - **gitsign** (always on): an allowed person's short-lived Sigstore signature on that
    exact commit.
  - **GitHub merge** (2026-10-20, off unless an admin turns it on per app): GitHub's
    web-flow key signed the commit as the merge of a PR into the default branch, and an
    allowed login pressed merge. Optional strict mode adds a gitsign signature on every
    PR commit; optional required approvals add a second person.

  | Claim | gitsign | GitHub merge |
  |---|---|---|
  | Commit unmodified since signing | yes (Sigstore cert, Rekor) | yes (GitHub's pinned key) |
  | Who signed | an allowed person | GitHub's servers |
  | Who caused it | the signer | the PR's `merged_by`, an allowed login |
  | Who wrote the code | the signer | the PR's authors; gitsign-verified in strict mode |

  The GitHub-merge path trusts GitHub's key plus GitHub's record of who merged - the same
  GitHub the platform already trusts for hosting, review and webhooks - and **weakens
  non-repudiation: a stolen GitHub session can merge**. Strict mode and approvals narrow
  that; nothing removes it. Web-editor, API, App and update-branch commits carry the same
  key and are refused, because no PR records them as its merge.
- **The release-signing policy is admin-owned**: who may sign, whether a GitHub merge
  counts and who may merge live in the catalog chart's `releaseSigning` values (per
  cluster, in the cluster repo), never in the app's `cicd.yaml`, which the gated team
  controls. `cicd.yaml` can only tighten (strict mode, more approvals). Before 2026-10-20
  the signer list was in `cicd.yaml`, so a team could add itself to the list that gated it.
- **An agent's `cicd.yaml` change is held to the contract in CI, not only by the agent's own gate**
  (`cicd-validate`, 2026-10-31): every app-repo PR that changes `cicd.yaml` runs `glidepath validate`
  before it merges, and a PR by the agents' App also meets the `GP-SCOPE` rules (no human-only field,
  no gate loosened, no Flight environment touched) against the merge base. Clearance decides the same
  rules before the PR exists; the check is what holds whoever opened it. See [contract.md](contract.md).
- **Governance gates are real, not theater**: `sast` (Semgrep), `imageScan` (Trivy),
  `policyCheck` (gitsign), `sbom` (cosign attestation) all do genuine enforcement
  today, and any gate still awaiting a real backend is structurally marked as a stub -
  never silently reported as a pass. See [governance-stubs.md](governance-stubs.md)
  and [ADR-0003](adr/0003-governance-stubs.md).
- **A release is checked against the target cluster's admission policies before the merge**
  (`policy-validation`, 2026-10-26): the release's environments are rendered and evaluated offline
  (Kyverno CLI) against the policies in the target cluster's GitOps repo; a Deny/Enforce violation fails
  the check. The gate holds no credential to the target cluster or its repo: the broker reads the repo
  with a `contents: read` token it never hands out (the repo also holds generated passwords, and an app's
  pipeline namespace runs the app's own build scripts) and returns only the policy documents. See
  [policy-validation.md](policy-validation.md) and
  [ADR-0028](adr/0028-policy-validation-offline-against-target-cluster-policies.md).

## Release: no direct-mutation path exists

`release` cannot mutate a target cluster even if fully compromised - it can only open
a pull request against a `gitops-<app>` repo. Human review plus branch protection plus
ArgoCD's own sync are the only path from "PR opened" to "cluster changed." See
[release.md](release.md) and [ADR-0004](adr/0004-gitops-only-release.md).

### What changes on a rollback, and what does not

A rollback is an ordinary release of an earlier image (ADR-0021 phase 4,
[release-state.md](release-state.md)). The gates recompute its eligibility themselves (the image is one of the
environment's last five releases that reached healthy) rather than trusting the PR. On an eligible rollback the
*content* gates (`sast`, `imageScan`, `sbom`) run as advisory: they report but do not block, because that image
already ran there. The *integrity* gates (provenance, commit and image signatures) still block.

### No workload, no release

An environment whose human values say it runs no service (`rollout.enabled: false`, or the older `rollout: null`)
is refused by `open-release-pr` (Flight) and `deploy-manifests` (Ground) before any PR or commit. The files are
layered the way Argo CD layers them. The airframe chart's release-tracking guard also fails the render if a release
is tracked where no workload runs. See [release.md](release.md#an-environment-that-runs-no-workload-is-refused-2026-10-10).

### Environment Applications converge on git, including deletions

Every environment Application (Ground `lower-envs`, Flight `tenant-onboarding`) syncs automatically with self-heal
**and prune**, so a resource the chart stops rendering is deleted. Argo CD's default `allowEmpty: false` refuses a
sync that would delete everything, and a chart render failure stops the sync before any prune. The ApplicationSets
set `preserveResourcesOnDeletion`, so replacing a generator never deletes a running environment.

## Portal actions (Tower): owner-scoped, delegated, audited

Tower (the Backstage portal) can open the same PRs a release does, and run a few Argo CD operations. None of it is a
second path around the gates above:

- **Every write needs the app's owning team**, checked by Backstage's permission framework on the app's catalog
  entity. Operations that change a Flight environment's running state outside a reviewed PR (Argo CD sync and force
  sync) need an admin. Whether an environment is Ground or Flight is read from `cicd.yaml` on the server.
- **No Kubernetes write permissions.** Sync, force sync, terminate and Argo Rollouts actions (abort, pause, resume,
  retry, restart, promote) run through Argo CD as one `backstage` account. Its write roles cover app projects
  only, enforced by deny rules for the platform projects. Argo CD checks the resource belongs to the Application.
- **Skipping canary analysis on Flight** (promote, promote full) is allowed to owners but audited `critical` and
  announced to everyone, like any break-glass bypass.
- **Every write is audited** (`tower-write` events), refusals included.
- **No pod exec.** A time-boxed, two-person, recorded break-glass design is proposed, not built.

The full table of who may do what is in Tower's [security model](https://github.com/jfillman/tower/blob/main/docs/security.md).

## Secrets

Application secrets flow through a `ClusterSecretStore`/`ExternalSecret` model, never
through values files or platform-minted long-lived credentials - see
[secrets-management.md](secrets-management.md) and [app-secrets.md](app-secrets.md).
The GitHub App private key used for git operations never leaves the `pipelines-as-code`
namespace: a shared, TokenReview-authenticated service mints a per-repo-scoped
installation token on request instead of copying the key into every Application's
namespace - see [release.md](release.md#how-the-github-apps-private-key-stays-out-of-application-namespaces).

## Cloud deploys: no stored AWS key

A Lambda or ECS deploy assumes an IAM role made for that one function or service (`glidepath-deploy-<functionName>`,
`glidepath-deploy-ecs-<service>`) by trading a one-hour Kubernetes token for one hour of AWS access. Only the
`deploy-aws-lambda` and `deploy-aws-ecs` steps and the Lambda target's bootstrap Job run as the ServiceAccount the
roles trust (`glidepath-cloud-deployer`); every other step, including the ones that run the app's own code, has no
AWS access. Admission policies on the dev cluster decide who may run as that ServiceAccount by who created the object, not
by labels. There is no stored-key fallback: a step that can't assume its role fails. Azure deploys still use a stored
secret (known gap 43). See [cloud-deploy-identity.md](cloud-deploy-identity.md).

## Multi-cluster: no credential ever crosses the boundary

The dev cluster never holds a credential for any staging/prod cluster, at any point -
each additional cluster gets its own ArgoCD instance, and sync outcomes flow back to
dev as an authenticated event, never as a push in the other direction. See
[multi-cluster.md](multi-cluster.md) and
[ADR-0005](adr/0005-multicluster-per-cluster-argocd.md).

## Pipeline identities

An Application's `-cicd` namespace runs three kinds of code, and they do not share a credential. The trust model, plainly:

| Code | Runs as | Can reach the Kubernetes API? |
|---|---|---|
| The **repository's own code**: `build-source` (`build.sh`), `unit-test` (`test.sh`), `build-python-wheel-artifact`, `build-go-binary-artifact`, and the dependency installs those run | `source-runner` | **No.** No Role, no RoleBinding, `automountServiceAccountToken: false`, no projected token. |
| The **catalog's own tasks** (preflight, gates, release, deploy, notify, ...), whose code the platform ships | `pipeline-runner` | Namespace-scoped, least RBAC (below). Cannot create a run. |
| The **triggers** that chain the next stage, and the one-time onboarding bootstrap Job | `pipeline-trigger` | `create` on `pipelineruns` in this namespace, nothing else. No task runs as it. |

Why it matters. A pull request to the app repository can change `build.sh`, `test.sh`, a lockfile or a build backend, and that
code runs in the pipeline before anything is merged. When it ran as `pipeline-runner`, with the ServiceAccount token mounted, it
could write or delete the `release-tracking-*` ConfigMaps (the release records the Clearance gate reads), create a TaskRun or
PipelineRun of its own with any image, any Secret of the namespace mounted and a projected token for any audience (the
`clearance` one included, which Clearance trusts as the registrar `app-*-cicd/pipeline-runner`), and so register or attest a
change as the pipeline. Now the code that comes from the repository has no Kubernetes credential at all: it can build and test
and write to its workspace, which is what it needs.

What each part does.

- **`source-runner`** is chosen per task in the run's `taskRunSpecs`. The generated `.tekton/` build run
  (`deliver-onboarding-files`) carries the five entries (`build-source`, `unit-test`, `build-image` and the two artifact builds), a test run (generated, or chained by the Trigger) carries
  `run-integration-script`, the Task that runs `./integration-test.sh` (it used to be a step of `run-testworkflow`, in the pod that
  reads `app-secrets` and writes the Testkube namespace). It has an `imagePullSecrets` reference so its pod can pull the toolbox image;
  nothing else.
- **`pipeline-runner`** lost `create` on `pipelineruns` and `taskruns` (it keeps get/list/watch: `stage-debrief` and
  `comment-pr-check-result` read their own run). Its other grants are unchanged in this release: `app-secrets` `get` by name, and
  ConfigMaps get/list/create/patch/delete in its own namespace (see "Known, accepted gaps" below).
- **`pipeline-trigger`** is what Tekton Triggers creates the next PipelineRun as (the broker impersonates it, and only it). The
  run it creates still executes as `pipeline-runner`.
- **A pull request's `.tekton/` file is source too.** Pipelines-as-Code creates the PipelineRun from the file in the PR as its
  own controller identity, so no account's RBAC applies to that creation. RBAC cannot say "this run must be one of the catalog's",
  so two ValidatingAdmissionPolicies in `glidepath-control-plane` (`templates/admission/cicd-run-shape.yaml`) do, for runs created
  by the Pipelines-as-Code controller in a `(app|infra)-*-cicd` namespace only:
  - `cicd-run-shape` (always Deny): `tekton.dev/v1` only, a cluster-resolver `pipelineRef` to one of the Pipelines in
    `cicdRunAdmission.pipelines` (the 18 PR-check and post-merge Pipelines the onboarding templates deliver, and the five flow
    stages), only the parameters each takes, shaped by `paramPatterns` and tied to what Pipelines-as-Code recorded (see below), no
    inline `pipelineSpec`, run-wide account `pipeline-runner`, one `taskRunSpecs` entry per task and only for `source-runner` or
    `glidepath-cloud-deployer` (which has its own policy), a `podTemplate` of `hostAliases` only, workspaces `emptyDir` or
    `volumeClaimTemplate` only. Also on UPDATE. `cicd-run-no-taskruns` refuses a TaskRun from the PaC identity.
  - `cicd-run-source-isolation`: a run of a Pipeline that runs the repository's code (`build`: `build-source`, `unit-test`, `build-image`
    and the two artifact builds; `test`: `run-integration-script`; the list is `cicdRunAdmission.sourceTasks` and the policy's message is
    rendered from it) must give those tasks to `source-runner`. A PR that deletes those lines
    from its `.tekton` file is refused, not obeyed. `cicdRunAdmission.sourceIsolationActions` sets what a refusal does.
  Every expression is `has()`-guarded and the match conditions cannot raise, so a CEL error cannot refuse requests cluster-wide.
- **Tests.** `charts/glidepath-app/tests/pipeline_least_privilege_test.py` renders the charts and asserts each account, its
  automount, its effective RBAC, and that `source-runner` can do nothing (a negative test, run verb by verb);
  `charts/glidepath-control-plane/tests/cicd_run_admission_test.py` evaluates the policies' CEL over the platform's own templates
  (admitted) and over each way a PR could reach beyond the catalog (refused). Both run in `pipeline-identities-ci`.

Forks. Pipelines-as-Code runs a pull request from a fork only after an owner or collaborator comments `/ok-to-test`
(`remember-ok-to-test` is off, so every new commit needs it again). The policies apply to those runs as to any other.

### What remains possible for someone who can open a pull request

Read this before relying on the section above. Two kinds of person, because the platform gives them different things.

**A. Someone who can open a pull request** (a branch in the application repository, or a fork after an owner's `/ok-to-test`). Their code
runs in the build and test stages before anything is merged.

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path.** One unaudited lead. | `build.sh`, `test.sh`, `integration-test.sh`, installs, build backends and `Containerfile` `RUN` lines all run as `source-runner` (no token). The lead: a credentialed task that opens a repository-chosen path (a symlink to its own token) and prints or publishes it. Closed for `attest-api-spec`, `build-artifact`'s publish, `sast-scan`, the TestWorkflow file and the chart renders; `stage-preflight` reading `cicd.yaml` was not audited. |
| **(b) write or delete a release record** | **No known path.** | It needs the token, so it is only as closed as (a). |
| **(c) read a Secret** | **Yes: the registry push credential.** Nothing else known. | A `RUN` line in the `Containerfile`: `build-image` runs BuildKit with `--oci-worker-no-process-sandbox` in the container that mounts `registry-credentials`. |
| **(d) push an image to another application's repository, or a base image** | **Yes, through (c).** Not through parameters. | Read the credential from a `RUN` line and push with it: the credential is the shared Ground registry one, not scoped per app. `image-repo` itself is now bound to the app's own repository, so the platform's own push cannot be redirected. |

**B. Someone with push rights to the application repository** (can push `main` where it is not protected, any branch, and `v*` tags). They
get everything in A, and:

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path for the platform's service accounts.** Code in the Testkube namespace is a separate account, not audited here. | A `test` flow applies the commit's `glidepath/<name>.yaml` (now checked: one TestWorkflow, no service-account choice, no host/secret/projected/PVC volume, no `envFrom`, secrets only via the placeholder) and runs it as Testkube's executor in the shared `testkube` namespace. What the executor's own account can do there is Testkube's, and is not constrained by this chart. |
| **(b) write a release record** | **Yes, by releasing.** Not arbitrary content. | Push a `v1.2.3` tag, or `main`: the release flow's own task writes `release-tracking-*` for that commit, with that commit's `cicd.yaml` as input. That is a release of a commit nobody reviewed; it is gated downstream (the release PR, Clearance), not here. Forging a record's contents directly needs the token. |
| **(c) read a Secret** | **Yes.** | The registry credential as in A. This application's own `app-secrets` through a TestWorkflow, by design. Another application's secret copy: the workflow cannot name it (checked at apply; and `vpol-testkube-secret-usage` in the cluster policies). |
| **(d) push to another application's images** | **Yes, through the registry credential** (as in A). | As in A. |

What each parameter on a `.tekton` file can now do: nothing beyond what the platform's Pipelines do with the values it validates. Images
the pipelines RUN are not chosen by the repository: `build.agent` is an enum mapped by the catalog to fixed images, the toolbox,
Semgrep, BuildKit and validator images are catalog values. Fields that name something to PUSH to: `image-repo` (bound to the app);
cloud deploy targets in `cicd.yaml` name functions and services, and the web-identity role they assume is created per target by Airframe and
limited by AWS, not by this chart.

What this release does close, and how that was checked:

- **The repository's scripts, installs and `RUN` lines** (`build.sh`, `test.sh`, `integration-test.sh`, dependency installs, build backends) run
  as `source-runner` with no Kubernetes credential. That is the only thing they no longer have. Two of those steps still sit next to a
  registry credential: `build-image`'s build-and-push step runs the `Containerfile` `RUN` lines with `registry-credentials` mounted at
  `/home/user/.docker` (open, see A(c)), and `build-artifact`'s pod mounts it for its separate `publish` step (the build step does not
  mount it; `publish` validates the path it pushes). The other build steps and `run-integration-script` mount no registry credential;
  `run-integration-script` shares nothing with `run-testworkflow`'s pod.
- **A pull request's `.tekton` file** can no longer widen a run: it must start one of the Pipelines the platform itself starts
  from a file (the list is rendered from the onboarding templates and the flow generator, and a test compares them), pass only the
  parameters that Pipeline takes, with values of the expected shape, and the repository, revision, pull request and namespace
  parameters must equal what Pipelines-as-Code itself recorded for the event. Pipelines that deploy, release or test start only from a
  push to a protected branch or to a tag of the shape `v<major>.<minor>.<patch>`, decided on the full ref Pipelines-as-Code records
  (`source-branch`), so a branch named `refs/tags/v1.0.0` is not a tag. `image-repo` must be this application's own repository
  (`<registry>/<owner>/<app>` or `-pr`), so a build cannot push into another app's repository or cache; `onboarding-resync` may
  target only `gitops-<app>` under the repository's own owner, and only from a push; a task may run as `source-runner` or
  `glidepath-cloud-deployer` only where the Pipeline's task list says so. It cannot name another ServiceAccount, add a pod template, embed a Pipeline, or start a
  TaskRun. This depends on Pipelines-as-Code writing its own annotations over anything the file sets; that is how its source reads
  and how a run looked live, but it was not exercised against a hostile file, so the upgrade note asks for it to be.
- **Creating a run** needs `pipeline-trigger`, which no task runs as.
- **`helm template` in credentialed pods** (`policy-validation-gate`, `clearance-gate`): a chart with a symlink leaving its directory,
  or a symlinked value file, is refused before helm runs, and helm's error text is no longer printed from those pods (it can carry file
  content, e.g. `{{ fail (.Files.Get ...) }}`); the log says to run helm locally. Not done: rendering in a pod with no token and
  handing the output over as data, which would remove the class. `clearance-gate.yaml` edits are the include of the helper, a
  `chart_safe` call inside `render()`, helm's stderr to `.helm-err`, and the two `FAIL` lines.
- **A re-run from a commit comment** (`/retest` on a pushed commit) is recorded by Pipelines-as-Code with an event type its source
  sets from the comment (not `push`; the exact string was not confirmed). A privileged Pipeline then fails the push-only rule and is
  refused: the safe direction. Re-run those from the portal (glidepath-api rerun, once the cluster policy allows it) or push again.

**Ref protection is a name check here, not a verification.** The policy cannot ask the git host whether a branch needs review or who may
push a tag. Anyone who can push `main` (where it is not protected) or a `v1.2.3` tag can start the deploy, test, release and
gitops-image-bump flows for that commit as `pipeline-runner`, with that commit's `cicd.yaml` as input: they can write
`release-tracking-*` records, and a `test` run applies the commit's `glidepath/<name>.yaml` into the Testkube namespace with the app's
Secret copied in. Closing it needs a host-side rule (a ruleset requiring review on the default branch, and a tag ruleset limiting who
may create `v*` tags) and a platform check that the ref is covered; the check is not built. On a plan without rulesets (this lab's
free plan) it is "Off here" and cannot be turned on.

What is not closed, in the order to do it:

1. **`build-image` and the registry credential** (A(c), A(d), B(c), B(d)): the push credential and the daemon share a container, and `RUN` lines run
   without a process sandbox. `build-image` now runs as `source-runner`, so the pod holds no Kubernetes token, but the credential
   is still reachable. The fix is architectural and was not attempted here: build with `--output type=oci` (no credential in the
   build container) and push from a separate step or task, or run the daemon with its process sandbox on. Until then a pull
   request author can obtain a credential that can push to this application's image repository.
2. **Workspace trust in credentialed tasks.** `sast-scan` now scans a copy without `.git` or symlinks, and `attest-api-spec` and
   `build-artifact`'s publish step refuse symlinks and traversal. Any other task that runs git or a tool against the shared
   workspace after the build tasks have written to it, while holding a credential, needs the same care. Enumerated: `stage-preflight`
   runs before the repository's code and clones fresh; `build-image` reads the workspace as the BuildKit context and has no token
   now; no other catalog task runs git there (`deliver-onboarding-files` clones into its own directory). `build-artifact` still
   builds and publishes in one pod (different containers; only `publish` mounts the credential and it now validates the path);
   splitting it into two tasks is not done.
3. **`pipeline-runner` still writes every `release-tracking-*` ConfigMap, namespace-wide, and still reads `app-secrets`**, for every
   catalog task. Only the release tasks (`open-release-pr`, `mark-release-merged`, `itsm-change`, `itsm-gate`, `clearance-gate`,
   `comment-pr-check-result`, `resolve-release-tracking`) should write them. Splitting `pipeline-runner` into a release-record
   writer, a gate account and a read-mostly account is the change that makes (b) need a catalog-task compromise, not a token.
4. **The Clearance audience is still minted for `pipeline-runner`.** Clearance trusts `app-*-cicd/pipeline-runner` as registrar
   (`CLEARANCE_VERIFIERS`). A dedicated `clearance-gate` ServiceAccount, plus a policy that only that account may hold a token
   of the `clearance` audience (the shape of the catalog-token policy Autopilot already has), needs Clearance to trust it first.
   Migration order: (a) in Clearance's `CLEARANCE_VERIFIERS`, add `app-*-cicd/clearance-gate` and `infra-*-cicd/clearance-gate`
   next to the existing patterns (both trusted); (b) Glidepath adds the `clearance-gate` account and runs the `clearance-gate`
   task (and the agent-gate tasks that use the same audience) as it through `taskRunSpecs`; (c) confirm in Clearance's records that
   every registration now comes from the new principal; (d) remove `app-*-cicd/pipeline-runner` from the verifiers, and add the
   audience policy.
5. **A TestWorkflow from the repository** (`glidepath/<name>.yaml`) runs in the shared Testkube namespace under Testkube's executor, with the
   app's own `app-secrets` materialised. The file is checked before it is applied (see B above), but the allowlist is of fields I
   know of: Testkube adds fields over time, and a field this check does not name is allowed. What the executor's account can do in that
   namespace is not constrained by this chart.
6. **A rerun started through glidepath-api** copies the original run's `taskRunSpecs`. The policy that holds glidepath-api to a
   shape (`tower-identity-writes`, in the cluster repository, not here) says "no `taskRunSpecs`", so a rerun of a build or test
   run is refused until it allows entries of exactly `{pipelineTaskName, serviceAccountName: source-runner}` for the tasks in
   `cicdRunAdmission.sourceTasks`. A new push or `/retest` is not affected.

### Upgrade note

Order matters because a build run created from a `.tekton` file written before this release has no `taskRunSpecs`.

1. Release `glidepath-app` and `glidepath-catalog` together. The app chart adds `source-runner` and `pipeline-trigger`, moves the
   Triggers and the bootstrap Job to `pipeline-trigger` and removes run creation from `pipeline-runner`; the catalog's onboarding
   task then writes the new `.tekton/` build file. Triggers fire as `pipeline-trigger` from the moment the chart syncs.
2. Release `glidepath-control-plane` with `cicdRunAdmission.sourceIsolationActions: [Warn, Audit]`. `cicd-run-shape` is Deny from
   the start (every file the current onboarding delivers satisfies it, which the policy test checks). A not-yet-resynced build file is let through and warned.
3. Merge the onboarding-resync PR in every app repository (the one that rewrites `.tekton/`). Until it merges that app's build
   still runs its repository code as `pipeline-runner`; it has lost run creation, but not the ConfigMap grant. While any app is in
   this state `sourceIsolationActions` is not Deny, the binding carries the annotation `glidepath.io/enforcement: NOT ENFORCING` and
   (with prometheus-operator) the alert `CicdRunSourceIsolationNotEnforcing` fires after 3 days. **It must not be left on.**
   Also, before the first `[Deny]` of `cicd-run-shape`, check one real file: on a branch, add a bogus `git-revision` to a copy of
   a check's `.tekton` file and confirm the run is refused (the policy compares it with what Pipelines-as-Code records).
   Apps whose flows trigger on a branch other than `main` need `cicdRunAdmission.protectedBranches` set to include it, or their
   deploy, test and release flows are refused.
4. When no build run carries a source-isolation warning, set `sourceIsolationActions: [Deny]` (the default for a new install).
5. Ask the owner of the cluster repository to let glidepath-api reruns carry the `source-runner` entries (item 6 above).

Rolling back is the reverse: a build run is only refused by `cicd-run-source-isolation`, so set its actions to `[Audit]` first.

## Build isolation

Image builds run rootless BuildKit as UID 1000: no privileged container, no added
capabilities, no host namespaces or mounts, and no seccomp or AppArmor override, so a
build pod meets Pod Security Standards **`baseline`**. It does not meet `restricted`:
rootlesskit must create a user namespace, which `allowPrivilegeEscalation: false` and the
RuntimeDefault seccomp profile both block (tested 2026-10-17, also with `hostUsers: false`).
Until 2026-10-17 builds ran kaniko as **root** in the container, and these docs claimed
`restricted`; that was never true. Since 2026-10-17 every `-cicd` namespace the
tenant-onboarding ApplicationSet creates **enforces** Pod Security `baseline` (plus `warn`
and `audit` at the same level), set through `managedNamespaceMetadata` in
`charts/glidepath-control-plane/templates/argocd/tenant-onboarding-applicationset.yaml`.
A build or check pod that asks for `privileged`, host namespaces, host paths, added
capabilities or an `Unconfined` seccomp/AppArmor profile is refused at admission. Enforce
went on only after build, deploy, release, test, onboarding-resync and preview runs had
come back clean under `warn`/`audit`. Pods outside `-cicd` (the deployed app's own env
namespaces) are not covered by this label. See [rootless-builds.md](rootless-builds.md) and
[ADR-0026](adr/0026-buildkit-image-builds.md).

## Known, accepted gaps

- **NetworkPolicy is a no-op on a non-enforcing CNI** (see above) - accepted for a
  shared dev cluster, not for anything beyond it.
- **The broker's TokenReview interceptor runs plain HTTP internally**, not HTTPS - a
  deliberate tradeoff after a real TLS integration attempt hit a hard wall (see
  [chaining.md](chaining.md)); TokenReview-verified identity, not transport
  encryption, was always the actual trust boundary for this never-leaves-the-cluster
  call.
- **`pipeline-runner` is still one account for every catalog task**: it writes the release records and reads `app-secrets`
  for all of them. The repository's code no longer shares it; the catalog's own tasks still do (see "What this does not yet do").
- **Branch protection depends on the GitHub plan.** On this lab's private gitops repositories (free plan) branch
  protection is not available (checked 2026-10-10: the API answers "Upgrade to GitHub Pro"), so a release PR can be
  merged with failing checks by anyone with write access. Such a merge is a break-glass bypass: the PR keeps its
  failed checks visible. "Human review plus branch protection" above holds only where the plan provides it.
- **A GitHub-merged release rests on a GitHub session.** With the GitHub-merge path on,
  whoever holds an allowed merger's GitHub session can release a PR's code (in `merger`
  mode, code nobody gitsign-signed). Accepted per app by the admin who turns it on;
  strict mode and required approvals narrow it ([commit-signing.md](commit-signing.md)).
- **Governance gates that haven't landed yet** are visible as structurally-marked
  stubs, not hidden - see [governance-stubs.md](governance-stubs.md) for current
  status of each.
