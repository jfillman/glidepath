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
- **The proposer of a release is the author of the change, never the release bot**: Clearance's tier-2 rule is "one
  owner, not the proposer", and a release PR's own author is the pipeline's GitHub App. `clearance-gate` registers the
  source commits' author and the weakest provenance of the source PRs, resolved from the pipeline's release record (not yet authenticated: see the caveat in clearance-gate.md) and
  accepted only for a PR whose content is exactly what the record says the pipeline wrote; an unresolvable source is
  registered `unknown` (Clearance denies it), never `person`. See [clearance-gate.md](clearance-gate.md#who-proposed-a-release).
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

### Reissue: a release PR replaced after it was closed unmerged

`POST /v1/apps/{app}/releases/{chainId}/reissue` (glidepath-api, T2) is a write path with the same authority as promote and no
more: `promote` on `environments` (the `glidepath-api-releaser` role), a delegate naming the person, the app's own
`promote-release` template, service account `pipeline-runner`, the platform's PR-opening GitHub App, and a human merge.

- **Who:** the same callers as promote. Tower's route is owner-gated (`tower.release.reissue`, owners and admins); the API
  authorizes the delegate's write again by the person it names.
- **What it can start:** only a release of the **record's own** image and commit, at its own environment and cluster. The body is
  ignored, so no caller chooses an image, a commit, an environment or a chain. It starts a promotion, never a rollback, so no
  gate is made advisory (the content gates run as for any promotion, and a rollback-eligible image gets no relaxation from it).
- **An image that failed its gates:** the new PR is a new PR with every release gate running from scratch (sast, image-scan,
  provenance, commit signing, the values and policy gates, Clearance) against the current gitops `main`; nothing is carried over
  from the closed PR, and a failing gate fails the new PR exactly as it would have the first one. The image must also still exist
  and verify: `resolve-image-ref` and the provenance gate read it again.
- **A gate skipped:** none is skipped or relaxed; the release kind is `promote`.
- **An environment the caller does not own:** the environment is the record's, the app's `releaser` role is per namespace, and the
  environment must still be a Flight environment of the app's current `cicd.yaml`.
- **Replaying an old release over a newer one:** refused when any other release of the app to that environment and cluster was
  opened after this one, or merged after this one was opened, or when any release PR for the environment is open now.
- **A forged record:** a release record is written by `pipeline-runner`, whose token repository code can reach (see
  "Pipeline identities"). So nothing in a record is trusted: the image must be `<repo>/<app>:<tag>` whose tag's sha is the
  recorded 40-character commit, the commit must resolve in the app's repository, the source URL must be the app's repository, the
  PR must be one of the app's gitops PRs opened from `release-<env>-<sha8>`, and its state is read from the git host. What a forger
  could still cause is a promotion of a commit and image of the app's own repository: the same thing anyone with the release
  verb can already start with promote, behind the same gates and a human merge.
- **Clearance:** the new PR is a new registration and a new hold; the closed PR's change stays as it was (its PR never merged, so
  its hold never resolved into a release). Nothing is reused from it.
- **Ref protection:** the run carries the origin stamp (`origin-ref`, `origin-sha`) the API sets for promote, from the record's
  commit and the configured protected branch, so `verify-ref-protection` checks the same claim against the git host. The new PR
  is opened from a fresh branch state (one commit, never reopened, never force-pushed from that PR's point of view), so the
  review-evidence baseline rules hold. The branch name is the same per-commit name as the first attempt's; `open-release-pr`
  already recreates it from `main` and force-pushes when no PR is open, which only ever touches the closed PR's head.
- **Once per release:** the run's name derives from the closed release's chain id, so a second reissue (another key) is refused.
- **Audit:** the API's audit line names the caller, the person, the closed chain, the environment, the run and whether the link
  was stamped. The new record's `reissueOf` and the closed record's `reissuedBy` are the durable join.

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

An Application's `-cicd` namespace runs four kinds of code, and they do not share a credential. The trust model, plainly:

| Code | Runs as | Can reach the Kubernetes API? |
|---|---|---|
| The **repository's own code**: `build-source` (`build.sh`), `unit-test` (`test.sh`), `build-image` (the `Containerfile` `RUN` lines), `build-python-wheel-artifact`, `build-go-binary-artifact`, and the dependency installs those run | `source-runner` | **No.** No Role, no RoleBinding, `automountServiceAccountToken: false`, no projected token. `build-image` mounts **no registry push credential** (see [Image build and push](#image-build-and-push)). |
| The **image push**: `push-image`, which holds the registry push credential and runs nothing from the repository | `image-pusher` | **No.** Same as `source-runner`: no Role, no token. It is a separate account so the admission policy can say which task may have it. |
| The **catalog's own tasks** (preflight, gates, release, deploy, notify, ...), whose code the platform ships | `pipeline-runner` | Namespace-scoped, least RBAC (below). Cannot create a run. |
| The **triggers** that chain the next stage, and the one-time onboarding bootstrap Job | `pipeline-trigger` | `create` on `pipelineruns` in this namespace, nothing else. No task runs as it. |
| The **release record writes**, the **Clearance-token tasks** and the **Testkube test task**, split from `pipeline-runner` (pipeline trust T4; tasks mapped in step 3, `pipeline-runner` keeps its old rights until step 6; see "Split task accounts" below) | `release-record-writer`, `clearance-gate`, `test-runner` | Namespace-scoped, one narrow Role each. None can create a run or a pod, request a token or delete anything. |

Why it matters. A pull request to the app repository can change `build.sh`, `test.sh`, a lockfile or a build backend, and that
code runs in the pipeline before anything is merged. When it ran as `pipeline-runner`, with the ServiceAccount token mounted, it
could write or delete the `release-tracking-*` ConfigMaps (the release records the Clearance gate reads), create a TaskRun or
PipelineRun of its own with any image, any Secret of the namespace mounted and a projected token for any audience (the
`clearance` one included, which Clearance trusts as the registrar `app-*-cicd/pipeline-runner`), and so register or attest a
change as the pipeline. Now the code that comes from the repository has no Kubernetes credential at all: it can build and test
and write to its workspace, which is what it needs.

What each part does.

- **`source-runner`** is chosen per task in the run's `taskRunSpecs`. The generated `.tekton/` build run
  (`deliver-onboarding-files`) carries six entries (`build-source`, `unit-test`, `build-image` and the two artifact builds as `source-runner`, and `push-image` as `image-pusher`), a test run (generated, or chained by the Trigger) carries
  `run-integration-script`, the Task that runs `./integration-test.sh` (it used to be a step of `run-testworkflow`, in the pod that
  reads `app-secrets` and writes the Testkube namespace). It has an `imagePullSecrets` reference so its pod can pull the toolbox image;
  nothing else.
- **`pipeline-runner`** lost `create` on `pipelineruns` and `taskruns` (it keeps get/list/watch: `stage-debrief` and
  `comment-pr-check-result` read their own run). Its other grants are unchanged in this release: `app-secrets` `get` by name, and
  ConfigMaps get/list/create/patch/delete in its own namespace (see "Known, accepted gaps" below).
- **Split task accounts (pipeline trust T4, in progress).** `pipeline-runner` still holds the release-record write, the
  `app-secrets` read and the Testkube grants, so one flawed catalog task reaches all of them until step 6 removes them. The
  glidepath-app chart renders three accounts that take those powers apart (`templates/identity/task-accounts.yaml`, step 2), and
  since step 3 the writer, Clearance-token and test tasks run as them (the next paragraph). `pipeline-runner` has lost nothing yet:
  a task a run does not map still works, and the policy that would refuse the old mapping is a later step. Each holds exactly the rights below and the
  chart test (`charts/glidepath-app/tests/pipeline_least_privilege_test.py`) fails if a chart change binds one more to any of them.
  None of the three can create a PipelineRun, TaskRun or pod, request a ServiceAccount token, impersonate, delete, or read a Secret
  beyond what is listed.

  | Account | Holds | Will run |
  |---|---|---|
  | `release-record-writer` | ConfigMaps `get`, `list`, `create`, `patch` in the `-cicd` namespace (no `delete`, no `update`); `get` on PipelineRuns (`open-release-pr` reads its own run's lineage annotations, `record-gate-verdict` its tasks timeout and pull request label); `get`, `list` on TaskRuns (`record-gate-verdict`: which of its run's tasks failed); `get` on the `cluster-registry` ConfigMap of `platform-system` (only for an app with a cluster-mapped environment). No Secret. No `clearance` token. | `open-release-pr`, `mark-release-merged`, `itsm-change`, `itsm-gate`, and `join-release`, a small Task carved out of `clearance-gate` (the `clearanceId` join). The second carve-out, `record-gate-verdict` (the gate-ledger write that `comment-pr-check-result` made), is a finally task of every gate Pipeline: see "Task mapping, step 3" below. |
  | `clearance-gate` | `get` on PipelineRuns; ConfigMaps `get`, `create`, `patch` (gate reports `gate-report-<run>`). No Secret. | The four Tasks that mount the projected token for the `clearance` audience: `clearance-gate`, `agent-gates`, `agent-definitions`, `agent-release`. Clearance trusts the pod's ServiceAccount principal, so this account needs `app-*-cicd/clearance-gate` in Clearance's verifier list before any task runs as it. |
  | `test-runner` | `get` on `app-secrets` and `dex-check-ride-credentials`; the app's own Testkube placeholder Secret and the `testkube-testworkflow-runner` Role (only for an app with a `test` stage); `get`, `list`, `watch` on this namespace's runs, pods, logs and events. No ConfigMap, no `clearance` token. | `run-testworkflow` only. |

  **Task mapping (step 3).** The mapping is a `taskRunSpecs` entry per task (`serviceAccountName` only), in every run the platform
  starts. For the runs started by a Trigger it is in the Trigger templates of the glidepath-app chart (`promote-release`,
  `rollback-release`, the flow Triggers for the `release` and `test` stages, and the release-outcome and release-rollback Triggers); for the
  runs Pipelines-as-Code starts it is in the onboarding templates, which render each app's `.tekton/` files (the generated flow files for the
  `release` and `test` stages included). Tekton fixes the account per pod, so a step cannot change it. All other tasks run as `pipeline-runner`.

  | Task (Pipeline task name) | Runs as | Where mapped |
  |---|---|---|
  | `open-release-pr` (Pipeline `release`) | `release-record-writer` | Trigger templates (flow `release` stage, promote, rollback); generated flow file for a release-rooted flow |
  | `mark-release-merged`, `itsm-implement` (`bypass-merge-check`) | `release-record-writer` | `pull-request-merged.yaml` |
  | `verify-change` or `verify-change-advisory` (`itsm-check`) | `release-record-writer` | `pull-request-itsm.yaml` (also the pin-PR gate files) |
  | `itsm-close` (`release-outcome-notify`), `itsm-close-rolled-back` (`release-rollback-notify`) | `release-record-writer` | `release-outcome-trigger.yaml`, `release-rollback-trigger.yaml` (chart value `itsm.enabled`) |
  | `join-release` (`clearance-gate-check`, a finally task) | `release-record-writer` | `pull-request-clearance-gate.yaml` |
  | `fetch`, `register` or `register-advisory`, `render-check` (`clearance-gate-check`) | `clearance-gate` | `pull-request-clearance-gate.yaml` |
  | `agent-gates` (`agent-gates-check`), `review` (`agent-definitions-check`), `release` (`agent-release`) | `clearance-gate` | `pull-request-agent-gates.yaml` (app and gitops repo), `pull-request-agent-definitions.yaml`, `push-agent-release.yaml` |
  | `run-testworkflow` (`test`) | `test-runner` | Trigger template and generated flow file for the `test` stage |

  Which tasks a Pipeline has depends on the catalog, and Tekton refuses a run whose `taskRunSpecs` names a task its Pipeline lacks.
  Three things vary: the Clearance gate's `register` task is `register` where the gate is `required` and `register-advisory` plus
  `render-check` where it is not; the change-management gate's task is `verify-change` or `verify-change-advisory`, and the
  `itsm-implement`, `itsm-close` and `itsm-close-rolled-back` tasks exist only where the catalog has a change-management provider
  (`itsm.provider`). The onboarding task (`deliver-onboarding-files`) fills in the right names from what the catalog renders
  (`task_names`, from the same helpers the Pipelines use); the glidepath-app chart cannot see the catalog, so the two Triggers that
  start the itsm close tasks need the chart value `itsm.enabled: true` where the catalog has a provider (default `false`, which leaves
  those two tasks on `pipeline-runner`; set it wrongly to `true` and every release-outcome and rollback notification run is refused).
  The chart tests (`pipeline_least_privilege_test.py`, `cicd_run_admission_test.py`) render every Trigger template and every onboarding
  template for each catalog variant and compare the mapping with the tasks the catalog Pipeline has. A `glidepath-api` rerun copies the
  run's `taskRunSpecs` (item 6 of "What is not closed"), so the mapping survives a rerun; the cluster policy that holds `glidepath-api`
  to a shape must therefore also allow the three accounts on these tasks, as it must for `source-runner`.

  **`join-release` and the ledger.** The Clearance join moved: `clearance-gate` no longer writes the release record. Its decide step
  hands the joins to a `joins` result and the `join-release` finally task of `clearance-gate-check` writes `clearanceId` and
  `clearanceTree` as `release-record-writer`. It is a finally task, not a task after `register`, because a holding or denied change
  fails `register` and a task after it would be skipped (a held change is the one whose join is most wanted).

  The gate-ledger write (`gate.<sha12>.<gate>`, with the record's `resourceVersion`) moved the same way, but a Pipeline cannot hand it
  the verdict: Tekton cannot order two finally tasks nor pass a result from one finally task to another, and the verdict and the
  pull request number used to exist only inside `comment-pr-check-result`. So every gate Pipeline has a finally task
  `record-gate-verdict`, run as `release-record-writer`, that works out the verdict itself from what a finally task can read:
  `$(tasks.status)`, the run's TaskRuns (a failed `-advisory` TaskRun alone is not a failed gate, ADR-0021), its PipelineRun's tasks
  timeout (a green run past it, or within a minute of it, is judged as the promotion step judges it) and the
  `pipelinesascode.tekton.dev/pull-request` label (else the numeric `pr-number`; neither: nothing is recorded). The decision is the
  same text as `comment-pr-check-result`'s, and `tests/gate_ledger_verdict_test.sh` holds the two equal. The ledger values stay
  `success` and `failure`. The promotion decision (the `promotion-ready` step of `comment-pr-check-result`) no longer writes; it
  waits up to `ledger-wait-seconds` (default 90, polling every 3 s) for **this gate's own** entry for the commit, then reads the others'.
  That restores the write-then-read order a Pipeline cannot express: of two gates finishing together, at least one sees both
  entries. Failure modes: the label is missing and `pr-number` is not a number (a `/retest` comment on a PaC that does not set the
  label): nothing is written, the step waits the full bound, logs `is not in the release record`, goes on with its own verdict and reads
  the other gates from the ledger and their check runs, so a release can then wait for the next gate or `/retest`; the writer is
  slower than the bound: the same, and the entry lands late for the next gate to read. A written entry for a PR that is already
  merged or closed, or for an older commit, is harmless (the key carries the commit, and only an open release PR with a record has one).

  RBAC cannot narrow `create` by name nor a ConfigMap write to keys, so until the admission policies of the next steps
  (`release-record-writes`, `gate-report-writes`) exist, a record write is limited by RBAC to `release-record-writer` only in
  intent: `pipeline-runner` can still write one. `cicdRunAdmission.recordWriterTasks`, `clearanceTasks` and `testRunnerTasks`
  list, per Pipeline, the tasks `cicd-run-shape` lets a `.tekton` file give to each account (a `taskRunSpecs` entry). They only
  widen what that policy admits (nothing is refused that was admitted before, in any `shapeActions`); the policy that would
  require the mapping is not part of step 3. They name Pipeline task names, so `register`, `register-advisory`, `render-check` and
  `verify-change`, `verify-change-advisory` are all listed. The platform-started Pipelines (`release-outcome-notify`,
  `release-rollback-notify`) are not PaC-startable and not listed.
- **`pipeline-trigger`** is what Tekton Triggers creates the next PipelineRun as (the broker impersonates it, and only it). The
  run it creates still executes as `pipeline-runner`.
- **A pull request's `.tekton/` file is source too.** Pipelines-as-Code creates the PipelineRun from the file in the PR as its
  own controller identity, so no account's RBAC applies to that creation. RBAC cannot say "this run must be one of the catalog's",
  so ValidatingAdmissionPolicies in `glidepath-control-plane` (`templates/admission/cicd-run-shape.yaml`) do, for runs created
  by the Pipelines-as-Code controller in a `(app|infra)-*-cicd` namespace only:
  - `cicd-run-shape` (always Deny): `tekton.dev/v1` only, a cluster-resolver `pipelineRef` to one of the Pipelines in
    `cicdRunAdmission.pipelines` (the 18 PR-check and post-merge Pipelines the onboarding templates deliver, and the five flow
    stages), only the parameters each takes, shaped by `paramPatterns` and tied to what Pipelines-as-Code recorded (see below), no
    inline `pipelineSpec`, run-wide account `pipeline-runner`, one `taskRunSpecs` entry per task and only for `source-runner`, `image-pusher`
    (only `push-image`, `cicdRunAdmission.pusherTasks`), `glidepath-cloud-deployer` (which has its own policy) or one of the split
    accounts on the tasks `cicdRunAdmission.recordWriterTasks`, `clearanceTasks` and `testRunnerTasks` list, a `podTemplate` of
    `hostAliases` only, workspaces `emptyDir` or `volumeClaimTemplate` only; a build that a push did not start (a pull request, a
    comment re-run) must pass `pr-tag-only=true`, and `pr-tag-only=true` needs the `-pr` image repository (this is what keeps a pull
    request from being treated as a main build: see Cache below). Also on UPDATE. `cicd-run-no-taskruns` refuses a TaskRun from the PaC identity.
  - `cicd-run-source-isolation`: a run of a Pipeline that runs the repository's code (`build`: `build-source`, `unit-test`, `build-image`
    and the two artifact builds; `test`: `run-integration-script`; the list is `cicdRunAdmission.sourceTasks` and the policy's message is
    rendered from it) must give those tasks to `source-runner`, and `push-image` (`cicdRunAdmission.pusherTasks`) to `image-pusher`. A PR that
    deletes those lines from its `.tekton` file is refused, not obeyed. `cicdRunAdmission.sourceIsolationActions` sets what a refusal does.
  Every expression is `has()`-guarded and the match conditions cannot raise, so a CEL error cannot refuse requests cluster-wide.
- **A gitops repository's `.tekton/` files are read from its default branch.** `pipelinesAsCode.gitopsRepoProvenance: default_branch`
  (control plane: the same key; Apron: `glidepath.gitopsRepoProvenance`) sets `spec.settings.pipelinerun_provenance` on the
  `gitops-<app>` Repository CR, and Pipelines-as-Code then builds every run for that repository from `.tekton/` at the default branch's
  head at the time of the event (it resolves the branch with the GitHub API on every pull request, push and `/retest`), not from the pull
  request's head. A pull request to a gitops repository can no longer change the pipeline that judges it, and a release pull request
  opened before a resync is judged by the current files from then on: no reissue is needed, but a run that started before the resync
  merged (finished or still running) used the old files and keeps that result, so `/retest` those checks to run them with the new
  files. The run still describes the pull request:
  the sha, source-branch, event-type and pull-request annotations, and the `{{ revision }}` and `{{ pull_request_number }}` the files
  pass, are the event's, so `cicd-run-shape` and `verify-ref-protection` judge exactly what they did. Only the definition moves. What
  it costs: a resync pull request's own checks run the files on `main`, not the ones it delivers (the delivered files take effect for
  every open pull request when it merges, so merge a resync when no gate run is in flight). The app repository is not changed (its
  `.tekton/` build file is meant to change in a pull request, and `cicd-run-shape` bounds what it may do). `extraGitopsRepos` are not
  changed either. Unset keeps today's behaviour (`source`).

#### Limits of reading pipelines from the default branch

Reading `.tekton/` from `main` has three limits. Each has a way out. The first way out needs a GitHub plan that has branch
protection on private repositories (Pro or Team), or public repositories.

- **No `.tekton/` on main: a pull request starts no checks, and nothing fails.** With no pipeline files on `main`, Pipelines-as-Code
  has nothing to run, so the PR shows no Glidepath check at all and looks clean. The way out is branch protection (or a ruleset) on
  `main` that requires the gate checks by name, pinned to the Pipelines-as-Code GitHub App, so a missing check blocks the merge.
  On a plan without branch protection for private repositories (GitHub answers 403 "Upgrade to GitHub Pro or make this repository
  public"), the limit is only mitigated. The onboarding resync delivers `.tekton/` to `main`, but it does not restore a removed one by
  itself: it runs only when a push to the app repository's `main` changes `cicd.yaml` (or `.tekton/flow-pr-build.yaml`), so to
  bring the files back, commit a change to `cicd.yaml` there (a comment-only edit is enough) and merge the resync pull request
  (see "Triggering a resync" in [onboarding-mechanics.md](onboarding-mechanics.md)). And someone has to watch for pull requests
  that carry no Glidepath checks.
- **A broken `.tekton/` file on main fails or blocks every PR in that repository, including the PR that fixes it.** The fix is
  judged by the broken files. The way out is an admin merge of the fix, or setting `gitopsRepoProvenance: source` for that cluster
  for one sync, so the fix is judged by its own files, then putting it back. With the setting switched, the fix PR does not
  get a fresh check by itself, because its runs already failed against the broken files: comment `/retest` on it after the
  sync, so the runs are built from its own files. An admin merge needs no retest.
- **A resync PR's checks run main's old files, not the new ones it delivers, so its green result does not test them.** The way out
  is to try new boilerplate on one scratch repository first, merge a resync when no gate run is in flight, and `/retest` the runs
  that started before it merged.

Two more rules for the required list. A required check with a path filter never reports on a PR that filter skips, and that PR
is blocked forever, so only gates that always run belong in the required list. And a skipped or neutral required check counts as
passing on GitHub, so a gate that can end skipped does not hold a merge.
- **The policies must compile under the API server.** cel-python (the behaviour tests) is more forgiving than the API server: it does not type-check map literals (every value of one must have the same type) or reject an invalid string escape (a regex backslash needs a raw string, `r'...'`). `hack/ci/vap-compile` (CI job `vap-compile`) compiles every rendered `ValidatingAdmissionPolicy` with the API server's own CEL compiler, for the default values and for a values file that sets every optional list.
- **Tests.** `charts/glidepath-app/tests/pipeline_least_privilege_test.py` renders the charts and asserts each account, its
  automount, its effective RBAC, and that `source-runner` can do nothing (a negative test, run verb by verb);
  `charts/glidepath-control-plane/tests/cicd_run_admission_test.py` evaluates the policies' CEL over the platform's own templates
  (admitted) and over each way a PR could reach beyond the catalog (refused). Both run in `pipeline-identities-ci`.

Forks. Pipelines-as-Code runs a pull request from a fork only after an owner or collaborator comments `/ok-to-test`
(`remember-ok-to-test` is off, so every new commit needs it again). The policies apply to those runs as to any other.

### Image build and push

The `Containerfile` `RUN` lines are the repository's code, and rootless BuildKit runs them with `--oci-worker-no-process-sandbox` in
the container that runs the build. Until this change that container also mounted the registry push credential, so a pull request
author could read it from a `RUN` line. The credential is one shared one: the platform secret store's key
`registry-credentials-dockerconfigjson`, copied by a `ClusterExternalSecret` into **every** namespace labelled
`hangar.io/managed-secrets` (19 namespaces on the lab's dev cluster: each application's `-cicd` and `-dev` namespaces and `testkube`). It is not scoped
per application, so it could push to other applications' repositories, base images and build caches. Now the build and the push
are two tasks in two pods:

| | `build-image` | `push-image` |
|---|---|---|
| Runs | the repository's `RUN` lines (BuildKit, rootless, no process sandbox) | the catalog's own script (toolbox image: `oras`, `jq`, `sha256sum`) and nothing from the repository |
| Account | `source-runner` (no token, no RBAC) | `image-pusher` (no token, no RBAC) |
| Secrets mounted | **none** of the push credential. At most the optional read-only pull credential `build-pull-credentials` | `registry-credentials` (the push credential), at `/var/run/registry-credentials` |
| Workspace | read-write | **read-only**; only `.glidepath-out/` is read, as data |
| Output | an OCI layout in `.glidepath-out/image` (and, on a main build, the cache in `.glidepath-out/cache`). Pushes nothing | the image pushed to the repository named by the run's `image-repo` parameter; the digest it computed and the registry confirmed |

**How the layout is validated** (`charts/glidepath-catalog/files/lib/oci-layout.sh`, called before anything is pushed). The layout is
read as data: `stat`, `wc`, `sha256sum` and `jq` over size-capped JSON; nothing is executed, sourced or extracted. Refused unless:

- the directory is not a symlink and holds only `oci-layout`, `index.json`, `blobs/sha256/<64 hex>` (regular files) and an empty
  `ingest/`: no symlink, device, socket or fifo, no other name (so no `..` and no separators);
- every JSON document a consumer interprets (the layout marker, index, image index, manifests, image configs) has only keys from an
  **exact-name allowlist** at each level, and repeats no key. This closes a parser differential: `jq` is case-sensitive and keeps the
  last duplicate, while `oras` and registries decode with Go's `encoding/json`, which matches keys case-insensitively. A manifest with
  `"layers": [good]` and `"Layers": [evil]` would pass a `jq` check while a consumer read the other list; here `Layers` (or any other
  case or Unicode spelling of an allowed name) is an unknown key and refuses. Duplicates are found with `jq --stream`, the token-level
  view that does not collapse them: every leaf path must be unique, no path may be both a leaf and a container, and every non-empty
  container must close under a unique parent path (tested with repeated scalars, arrays, objects, mixed types and an escaped spelling of
  the same key);
- shapes are checked before values are read: `layers` and `manifests` are arrays of objects, descriptors have string `mediaType` and `digest` and a non-negative integer **number** `size`, `platform` is an object of strings, annotations are string-to-string and may not name a ref (`org.opencontainers.image.ref.name`, `io.containerd.image.name`: the push is by `layout@digest`, so an alias has no legitimate use; the one exception is a tag-shaped `org.opencontainers.image.ref.name` on the single descriptor of `index.json`, which BuildKit's OCI exporter always writes and which is never read); every JSON file is exactly one document with no BOM, no NUL byte and nothing after it; the exit status of every `jq` call is checked, so an element after a bad one cannot go unvalidated;
- the image **config** blob is the author's own image configuration and nothing in the pipeline interprets it beyond `os`/`architecture`: it is **not** validated in depth (only its top-level, `config`, `rootfs` and `history` keys are allowlisted, and a repeated key anywhere in it is refused);
- names and digests are matched as whole strings (a blob name or digest with a newline never passes), and no component of the path from the
  workspace root to the layout is a symlink;
- `index.json` has exactly one descriptor; every descriptor is a `sha256` digest, a size equal to the file's, content that hashes to the
  digest, and no `urls`; the JSON documents are at most 1 MiB (a cache config 64 MiB);
- media types are on an allowlist: OCI index and manifest, the OCI image config, OCI tar/tar+gzip/tar+zstd layers (a cache's config is the
  BuildKit cache config); Docker types, foreign layers and anything else are refused; layers at most 512 per image manifest (8192 for a cache manifest: a `mode=max` cache keeps a layer per step of every stage and BuildKit documents no cap) and the distinct
  blobs at most `imagePush.maxImageBytes` / `maxCacheBytes` (8 GiB each by default);
- the platform set is exactly the one `cicd.yaml` asked for (`build.platforms`, read from the preflight task's validated config, not
  from the layout): one platform is a single manifest; several are one index, never nested, each entry `linux`, none repeated, none
  extra (an attestation manifest, `unknown/unknown`, is refused), each image config's `os/architecture` equal to its entry.

`oras cp --from-oci-layout <layout>@<digest> <repository>:<tag>` then copies it. The **target comes only from the run's parameters**:
`image-repo` (bound to the application's own repository by `cicd-run-shape`) and a tag built from the commit and the version string
the build resolved, which `push-image` checks as an image tag (it is read from the repository's `package.json`, `pom.xml` and so on, so it
is untrusted). Nothing in the workspace names a registry, repository or tag. The digest `push-image` reports is the one it computed from
the validated layout and `oras resolve` then returned for the pushed tag; a mismatch fails the task. **Everything downstream** takes `repo@sha256:<digest>` (the `image-pinned-ref` result), never the tag, so a tag re-pointed after the push cannot change what is scanned, described or attested (the
image scan, SBOM, API-spec attestation, the artifact builds' version, the Pipeline's `IMAGE_URL`/`IMAGE_DIGEST` for Chains and
Conforma, the build notifications) takes the image reference, repository and digest from `push-image`. The tag form (`image-ref`) is kept for display and for the `artifact.published` event that chains into later flows; the tasks outside the build Pipeline that read an image (`resolve-image-ref`, `qa-gate`, `verify-image-provenance`, `verify-sast-attestation`, `open-release-pin-pr`, `api-compat-gate`, `deploy-*`) are not part of the build run: they start from a release record or a chained event, and `resolve-image-ref` resolves the tag to a digest with `oras resolve` (and pins it when `promotion.pinDigest` is on). A tag re-pointed between a build and those later flows is the registry-immutability question below, not something this change closes. The only result of `build-image`
is `version`, which `push-image` validates and re-reports; a test fails if anything else reads a `build-image` result.

**Tag overwrite and reruns.** A tag that exists is never overwritten. Before pushing a main build's tag (`<version>-<sha7>`),
`push-image` resolves it. If it is already published, the run **pushes nothing and does not fail**: the published image is authoritative,
its digest is what every later task uses (`repo@digest`), the result `image` says `reused (tag already published)`, both digests (published
and this run's) are logged, and the freshly built layout is discarded. This is what makes a rerun of a main build for the same commit
work (BuildKit builds are not reproducible, so the rebuilt digest differs). Who published the existing tag may be anyone holding the
shared push credential, or whoever could start a build of that commit (a `v*` tag push, until slice T3 verifies ref protection); reuse
means the pipeline then scans, describes (SBOM), attests the API spec of, and releases **that** image, which is the image that should be
gated. The build provenance is the other side: Tekton Chains attests whatever digest the Pipeline's `IMAGE_URL`/`IMAGE_DIGEST` results
name as built by the run, and a reused image was not built by it, so for a reused image those two results are **empty** and this run
attests no build of it. The release gates verify the provenance attached to the digest (`verify-image-provenance` fails when there is
none from the build Pipeline), which is the original run's; a reused digest with no original provenance therefore fails that gate rather
than being vouched for here. The cache is not the image and may still be published. A pull request's tag (`<app>-pr`, the sha12) is
replaced by each push or rerun of the PR: it lives in the -pr repository, is deployed to an ephemeral environment by that tag and is never
released, so the newest build is the one wanted. The check is not a lock (a push can land between the resolve and the copy, and a registry
error reads as "absent"), and a seven-character sha prefix is what the tag has always carried (not changed: other tasks and ephemeral
environments reconstruct it). **Registry-side tag immutability is the real control**; where the registry offers it, turn it on for the
application repositories.

**Pull credential.** BuildKit still has to pull base images and import the cache. Anonymous pulls work for public bases; a private base
image, a private cache package or a rate-limited registry needs a credential, and the build pod is where repository code can read it.
So it is a separate, **read-only** one: a `kubernetes.io/dockerconfigjson` Secret named `buildPullCredential.secretName` (catalog
value, default `build-pull-credentials`) in each `-cicd` namespace, mounted optionally and read-only at `/home/user/.docker`. The platform
operator supplies it: create a registry token that can read packages and nothing else, store it in the platform secret store, and set
`glidepath-control-plane` `buildPullCredential.remoteKey` to its key (the chart then renders the `ClusterExternalSecret`; both charts
refuse the push credential's name or key). This chart creates no secret value. **The chart only refuses the push credential by name** (`registry-credentials`, and the push credential's remote key); a name check guarantees nothing about what the token in the Secret can do. Every pull request author in every application can read whatever this credential can read: a classic personal access token with `read:packages` reads every private package of its owner, not only the base images. Use a registry-scoped read-only token limited to the base-image repositories (and the cache packages), and keep the default, which is no pull credential, unless a private base image needs one. Without it the build pulls anonymously: public bases work;
a private base image fails the build with a pull error; a private cache package fails to import, which BuildKit logs and treats as a cold
cache. (Before this change a private base on the same registry worked because the push credential was mounted.)

**Cache.** Importing the registry cache is a pull. Exporting it is a push, so it never happens in the build pod:

- A **main build** (`pr-tag-only=false`) imports `<repo>-buildcache:latest` and exports its cache to an OCI layout in the workspace;
  `push-image` pushes the image first, then validates the cache and publishes it to `<repo>-buildcache:latest`. A cache that is absent,
  fails validation or fails to upload is **skipped with a warning and never fails the task** (the `cache` result says `published` or
  `skipped: <why>`; it is never pushed if it failed validation): a real `mode=max` cache can be large and unusual, and the image must not
  depend on it.
- A **pull request's build** (`pr-tag-only=true`, `image-repo` ending `-pr`) imports the **main** cache and **exports nothing**: it can
  read the cache a main build wrote and can never write one. `push-image` does not even look at a cache layout in the workspace for such
  a build, and refuses `pr-tag-only=true` with a repository that is not the `-pr` one. The admission policy makes `pr-tag-only` follow the
  event (a build that a push did not start must say `true`), so a pull request cannot claim to be a main build.
- Cost: a pull request's layers that differ from main are rebuilt on every push to the pull request, instead of being reused from that
  pull request's own previous build; a Containerfile whose changed stage is the first expensive one rebuilds everything after it. A main
  build is as fast as before for the build step; the cache layout is written to the workspace (the volume, `build.sourceVolume.size`,
  must hold the source, the image layout and the cache layout together: raise it for a large image; the default is small) and uploaded by
  `push-image` (only blobs the registry lacks are sent). The extra pod adds roughly the pod start (a few seconds) and the validation
  hashing of the layout.

**Multi-arch** is unchanged: one `buildctl` invocation builds every platform (QEMU for foreign-platform `RUN` steps, `$BUILDPLATFORM`
stages natively; [rootless-builds.md](rootless-builds.md)) and writes one OCI index; `push-image` pushes that index and every
platform manifest by digest. No remote builder exists.

**What per-application credential scoping needs (not built here).** With one shared credential, `push-image` can still push anywhere
the credential reaches: a compromise of that trusted step (the catalog's script, the `oras` and `jq` in the toolbox image, the registry
client), not repository code. To remove that, the platform needs either (a) a push credential **per application repository** (one
secret-store key per application, a `ClusterExternalSecret` selecting that application's namespaces by label, and the registry's own
per-repository permission, which a classic GitHub personal token does not offer, so a GitHub App installation token minted per run, or a registry with
repository-scoped tokens) or (b) registry-side **repository-scoped tokens** minted per push by the
broker from the run's identity. Either way the pull credential should stay a separate, read-only, platform-wide one. Until then the
shared credential's reach is "every repository the credential's owner can write", and it also sits in the other credentialed
catalog tasks' pods (`generate-sbom`, `attest-api-spec`, `build-artifact`'s publish step, `image-scan`, the release and verify tasks), none
of which runs repository code.

### Rendering is not done where the credentials are

`helm template` of a chart the repository chooses (`deploy.chart.path` in the repository, or a `repoURL` the developer controls) is
code-execution surface: Helm follows symlinks inside a chart, template functions read files (`.Files.Get`) and can fail with their
content, a render can be made very large or never end. It used to run in the pod of `policy-validation-gate` and `clearance-gate`,
which hold a Kubernetes token (`pipeline-runner`) and, for `clearance-gate`, a projected token Clearance trusts. It now runs in its own
task, **`render-manifests`, as `source-runner`**: no Role, no mounted token, no Secret volume, no projected token. The two gates read
what it wrote as data.

**Where it renders now, and under which account**

| Pipeline | Task (pipeline task name) | Account | What it holds | What the repository controls |
|---|---|---|---|---|
| `policy-validation-check`, `clearance-gate-check`, `values-check` | `fetch` (the gate Task, phase `fetch`; its `publish` step) | `pipeline-runner` | token, GitHub installation token, policy-bundle token (and the Clearance token in `clearance-gate`) | fetches repository files and charts as bytes; renders nothing |
| same | `render` (`render-manifests`) | **`source-runner`** | nothing: two workspaces, `inputs` read-only | the chart, the value files, every template |
| same | `evaluate` / `register` (the gate Task, phase `decide`) | `pipeline-runner` | as above | reads the render as data (below) |

The control plane's admission policy (`cicd-run-source-isolation`, `cicdRunAdmission.sourceTasks`) lists `render` for the three Pipelines, so a
`.tekton` file that runs it as `pipeline-runner`, or any account but `source-runner`, is refused; the Pipelines are the catalog's, so the
task cannot be skipped. The onboarding templates (`pull-request-policy-validation.yaml`, `pull-request-clearance-gate.yaml`) carry the
`taskRunSpecs` entry and the workspace.

**How the three tasks share data without trusting each other.** One volume per run (a `volumeClaimTemplate` in the template), used by
subPath: `inputs/` (what `fetch` published for the render: the checkouts, the charts, the plan, the observability facts; the render sees it
read-only), `rendered/` (what the render wrote; the gate sees it read-only), `trusted/` (the gate's own facts: the PR, the policy bundle,
clearance.yaml, the provenance verdict; **never mounted in the render task**). The gate's `publish` step empties the volume first (a retry, or a
volume cloned from another run, leaves nothing) and writes a `PUBLISHED` marker naming this run; the decide phase refuses a volume without
it (so a run whose workspace was swapped for an `emptyDir` fails, it does not pass as "nothing to check"). The tasks run strictly in
order, `fetch` then `render` then the decide phase (`render-check`, the advisory copy's companion, runs after `register-advisory`, not
beside it): nothing else writes the volume between the render and the read, and a test checks that no two tasks that bind it can run
together.

**What the render does.** The same helm invocation the gates always ran (release name, namespace, value files in the same order, the
injected values last), so the objects and the tree digest are unchanged. `HELM_PLUGINS` is an empty directory (no plugin), no
post-renderer, no `--set-file`, no `--dependency-update` (a chart's remote dependencies are **not** fetched at render time, and never were:
`helm template` does not do that; a chart must carry its `charts/`), no cluster (there is no token, and `helm template` answers `lookup`
with nothing). `--skip-tests` is deliberately not used: Argo CD's own render does not skip test hooks, and dropping one would change the
digest Clearance matches. A CPU-time limit and a file-size limit apply per render (`renderManifests.cpuSeconds`, `maxFileMiB`: Helm can
exit 0 with the output cut at the limit, so a file at the limit is refused), and a limit on the environments per run. #249's symlink
refusal stays (a symlink that leaves the chart, a symlinked value file). Helm's error text is printed to the render task's log, which holds
no credential. Network: none is needed and none is used; the Task is labelled `glidepath.io/network-egress=none` for a cluster
NetworkPolicy to select (this chart ships no NetworkPolicy, so the pod has the cluster's default egress; it still has nothing to send).

**What the gates do with the output, and must never do.** The output is attacker-controlled data: a pull request author could always make
the chart render anything. What this removes is the ability to read the gate pod's credentials or to run code beside them. So the gates
(`files/lib/rendered-ingest.py`, the `ingest` step) read it with a YAML parser that builds only plain data: every file is opened without
following a symlink and must be a plain file; every file digest in the `MANIFEST` is recomputed from the bytes the gate read (a missing,
extra or changed file refuses the tree); a tag other than YAML's core ones (`!!python/object`), a recursive anchor, nesting over 64 levels,
over 5000 documents, over 32 MiB a file, or over 1,000,000 nodes **after aliases are expanded** (an alias bomb) is refused. Nothing is
`source`d, `eval`ed or executed from it; none of its strings reaches a shell unquoted (object names are only ever data in JSON the gate
builds with `jq` or Python); nothing from it is printed wholesale (a log line carries a validated environment id and counts; the PR comment's
resource and message columns are cut to 200 and 300 characters with control characters, markup, mentions, links and table characters
removed). The **tree digest** Clearance registers is computed by the gate (`clearance-gate`'s object diff) from the bytes it verified and parsed,
with the same function as before: `tests/tokenless_render_test.py` renders a fixture chart through the new path and requires the same tree
and object digests as the old code produced (`tests/fixtures/render/golden.json`), and `tests/clearance_gate_parity_test.sh` still
requires the recorded Argo CD plugin digests.

**Policies the gate evaluates are the platform's.** `policy-validation-gate` evaluates the cluster's admission policies, read from the
cluster's GitOps repository through the broker's `/policy-bundle`, against the rendered objects. It loads no policy or exception from the
application's repository (the repository chooses only the cluster name in a path, which selects among the platform's configured bundles; an
unknown cluster fails the check). `clearance-gate` sends the repository's `clearance.yaml` to Clearance, which accepts it only when it
tightens the platform baseline (DG-POLICY), it is parsed as data in the fetch pod.

**Where repository content is rendered or interpreted in a pod that holds a credential** (the audit behind this section; the Argo CD
plugin that renders in Argo CD is Clearance's repository and out of scope here, it is named because it is the other renderer of the same
chart):

| # | Task / step | Account, what is mounted | What the repository controls | State |
|---|---|---|---|---|
| 1 | `policy-validation-gate` render | was `pipeline-runner`, token | chart, values, templates | **moved** to `render-manifests` as `source-runner` |
| 2 | `clearance-gate` render | was `pipeline-runner`, token (Clearance token in the same pod, other steps) | same | **moved** |
| 3 | `validate-values` `own-chart` (`values-check`) | was `pipeline-runner`, token | the same chart and values | **moved**: `render-manifests` profile `values`; the step now reads the render as data and makes the Namespace and Rollout checks |
| 4 | `validate-values` `validate` (the validator image on the platform's default chart) | `pipeline-runner`, token | values files only (the chart is the platform's) | data into a platform chart; not repository code |
| 5 | `policy-validation-gate` evaluate (Kyverno) | `pipeline-runner` | the rendered objects (data); policies are the platform's | reads data; findings escaped (this change) |
| 6 | `clearance-gate`, `policy-validation-gate` and `validate-values` fetch steps: a `yq` program with the environment name (a directory name in the gitops repository) interpolated into its text (`select(.name == "${env}")`) | `pipeline-runner`, projected tokens | the environment name | **was exploitable** (a reviewer proved it with yq 4.53.3: a directory name that closes the string can `load()` a mounted token into the resolved chart `repoURL`, and `fetch_repo` then fetches from that URL in the pod that holds the tokens). Fixed: names are checked against the platform's name rule before any use (the run is refused otherwise), passed to `yq` with `strenv()`, and a chart source is checked against the allowed hosts before any fetch |
| 7 | `run-testworkflow` (`kubectl apply` of the repository's TestWorkflow) | `pipeline-runner`, registry credential | the whole manifest, checked by a field allowlist | documented above (B(a)); **follow-up 2** is to apply it from a rendered, validated copy |
| 8 | `deliver-onboarding-files` (`yq` of `cicd.yaml`, generating `.tekton/`) | `pipeline-runner`, GitHub installation token | `cicd.yaml` values placed into generated files by shell expansion (never re-evaluated) | data; a hostile-file check is part of rollout (T6) |
| 9 | `deploy-manifests`, `open-release-pin-pr`, `bump-manifest-pr`, `run-testworkflow`, the release-pin helper (`yq -i`) | `pipeline-runner`, GitHub installation token | image reference, digest, workflow name: build results and cicd.yaml | fixed: values go in with `strenv()`; names checked to a pattern first |
| 10 | `deploy-aws-lambda`, `deploy-aws-ecs` (cloud credentials, web identity) | `glidepath-cloud-deployer` | function and service names from `cicd.yaml` | no template rendering; `jq`/`aws` with fixed programs |
| 11 | `sast-scan` (Semgrep over the checkout), `image-scan`, `generate-sbom` | `pipeline-runner`, `app-secrets` / registry credential | the repository files and image content as scan input | read as data; Semgrep scans a copy with no `.git` and no symlink |
| 12 | `promote` `source "${W}/repo.env"` | `pipeline-runner` | nothing: the file is written by the task itself with `printf %q` | not repository-controlled |
| 13 | Argo CD config management plugin (Clearance's repository) | n/a | renders the same chart in Argo CD | **out of scope**, named |

Nothing in the catalog runs `kustomize build`, `jsonnet`, `envsubst` or `eval`. Follow-ups: **(2)** `run-testworkflow`: apply a validated copy of
the TestWorkflow, not the repository's file. **(3)** `glidepath-api` reruns of the three gate runs copy the `taskRunSpecs` entry for `render` (item 6 of "What
is not closed", the same cluster policy change). `tests/tokenless_render_test.py` fails if a Pipeline's credentialed task runs `helm template`, and
`tests/program_text_test.py` fails if a yq or jq program anywhere in the catalog is a double-quoted string with a variable in it.

**Program text and fetch targets (the audit).** Every `yq`/`jq` program in every task and library script was checked. Those that took a
variable in the program text were fixed (the three fetch steps, `bump-manifest-pr`, `deploy-manifests`, the release-pin helper, `run-testworkflow`, and the
Rekor lookups of `qa-gate`, `verify-image-provenance` and `verify-sast-attestation`). Those that remain are named in `SAFE_PROGRAMS` with the reason (read_field and flag
take literal field paths; `clearance-gate`'s `filter` is one of two constants; the OCI layout library's helpers are called with literal expressions). The `yq` in
the toolbox is 4.44.3, which has no `--security-disable-file-ops` / `--security-disable-env-ops`; with a toolbox that has them (4.47 and later) they
should be added to every `yq` call that does not need `strenv()`. Until then the control is that no program text comes from the repository.
Fetch targets: the three fetch steps check a chart `repoURL` against `chartSources` (https git hosts, OCI registries; the default chart's host is always
allowed) and the revision, path and chart name against plain-character patterns before any `git fetch` or `oras pull`; `fetch_repo` sends the GitHub
installation token only to `github.com` and only for a `github.com/<owner>/<repo>` URL, and sends nothing to any other allowed host. Every other `git clone` or
`git fetch` in the catalog has a fixed `github.com` host with owner and repository names from parameters the admission policy binds to what Pipelines-as-Code recorded.

**Credentials never travel in a URL, an argument or a log excerpt.** `git fetch https://x-access-token:<token>@github.com/...` put the installation token in a
command's arguments, `with_retry` echoed those arguments on a failure, a repository's own configuration can make a fetch fail every time (a revision that does not
exist), and `comment-pr-check-result` posts the tail of the failed pod's log into the pull request. Now: every git call that needs the token gets it through
`git_auth` (an HTTP header in the environment, `http.https://<gitHost>/.extraheader`, so git sends it only to the catalog's `gitHost`, with `http.followRedirects=false`, no
credential helper, no prompt), never in a URL or argument (`.git/config` no longer holds it either); `with_retry` prints a label (`WITH_RETRY_LABEL`, or the command word),
never its arguments (`catalog/lib/retry.sh` is baked into the toolbox image: the toolbox must be rebuilt and `toolboxImage` bumped for this part; the change to the tasks
does not depend on it, because no argument carries a credential any more); and every place a log excerpt leaves the cluster (the PR comment, the Slack and Backstage
notifications) passes it through `redact_secrets` first (URL userinfo, `ghs_`/`ghp_`/`github_pat_` tokens, bearer and basic credentials, JWTs, `Authorization` lines).
`tests/credentials_test.py` fails if a catalog script or the app chart builds a URL with a credential in it, if a failing git command shows the token or its arguments, if
a log excerpt is not redacted before it is cut, and `tests/post_comment_test.sh` runs the real comment step with a fake token in the pod's log. What was found: the
platform has no git host setting other than the new `gitHost` (default `github.com`); the GitHub API calls, the broker and the owner/repository parsing are GitHub's
and stay as they are. A `curl -H "Authorization: Bearer ..."` on a command line is visible only inside its pod and was left.

**Environment directories.** Only a path of the environment shape is an environment: `<cluster>/<env>/values.yaml` or `release.yaml` (or `<cluster>/base.yaml`) under a
cluster directory whose name is a plain name (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`). Any other changed path (a root `values.yaml`, `docs.v1/...`, `Docs/...`, a deeper path)
is ignored as it always was; a path of the shape with an invalid environment name (`hangar-prod/Prod/values.yaml`) refuses the run without printing the name.
A chart spec in `cicd.yaml` the resolver cannot read (`chart: "str"`) refuses the run too: it used to leave the resolved chart empty, so the gate rendered the default
chart while Argo CD applied another.

**An open availability defect (not changed here).** The digest function (`json.dumps(sort_keys=True)`) raises `TypeError` when a rendered mapping has both a YAML 1.1
non-string key and string keys: for example a ConfigMap `data:` with `on:` or `yes:` (read as boolean `true`) beside `name:`. The check then goes red for a chart Argo CD would apply. It
did so before this change and the digest function was not touched, because Clearance's Argo CD plugin computes the same digest and the two have to change in step.

**Parser differentials that remain open.** Policy evaluation (Kyverno), the Clearance object diff and the tree digest all rest on how the gate parses the
rendered YAML (PyYAML's SafeLoader, YAML 1.1), and the cluster applies what Argo CD's own render and the API server make of the same text. Known
differences: a duplicate key in a mapping is last-wins here (the API server's strict decoding refuses it); YAML 1.1 scalars (`yes`, `on`, `012`, `0x1F`,
`1:30`, `1.10` without quotes) are read here as booleans and numbers, and as strings or other numbers by a YAML 1.2 reader; documents that are not mappings, or that lack
a `kind` or a `metadata` mapping, are skipped by the gate's diff and digest (the tree digest and the object list do not include them) while another reader may not skip them; a
mapping that mixes a YAML 1.1 boolean key (`yes:`) with string keys makes the digest function raise, which fails the check (it did before this change). None of these is closed here. A
parser that agreed with the cluster's would be needed, or a refusal of any document the two could read differently; both are open.

### What remains possible for someone who can open a pull request

Read this before relying on the section above. Two kinds of person, because the platform gives them different things.

**A. Someone who can open a pull request** (a branch in the application repository, or a fork after an owner's `/ok-to-test`). Their code
runs in the build and test stages before anything is merged.

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path.** One unaudited lead. | `build.sh`, `test.sh`, `integration-test.sh`, installs, build backends and `Containerfile` `RUN` lines all run as `source-runner` (no token). The lead: a credentialed task that opens a repository-chosen path (a symlink to its own token) and prints or publishes it. Closed for `attest-api-spec`, `build-artifact`'s publish, `sast-scan` and the TestWorkflow file. **Chart renders** no longer happen in a credentialed pod for the two release gates (`render-manifests` as `source-runner`; see "Rendering is not done where the credentials are"); `values-check`'s own-chart render moved the same way. `stage-preflight` reading `cicd.yaml` was not audited. |
| **(b) write or delete a release record** | **No known path.** | It needs the token, so it is only as closed as (a). |
| **(c) read a Secret** | **Only the optional read-only pull credential** (`build-pull-credentials`, if the operator supplied one). **Not the registry push credential.** Nothing else known. | A `RUN` line in the `Containerfile` runs in `build-image`, which mounts no push credential and runs as `source-runner`. What it can read is the pull credential, which can only read packages. |
| **(d) push an image to another application's repository, or a base image** | **No known path for repository code.** Not through parameters; not through a cache. | The push credential is only in `push-image`'s pod, which runs none of the repository's code and pushes what the validated layout and the run's parameters say (`image-repo` is bound to the app). A pull request writes no cache, so it cannot poison the cache a main build imports. What stays possible: its own `-pr` image (by design), and a compromise of the trusted push step (see "Image build and push"). |

**B. Someone with push rights to the application repository** (can push `main` where it is not protected, any branch, and `v*` tags). They
get everything in A, and (with `refProtection.mode: required`, the rows that start a flow need a pull request someone else approved for every commit
since the last release: a direct push, a force push or an administrator's bypass is `unreviewed`; with `off` the rows are as written):

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path for the platform's service accounts.** Code in the Testkube namespace is a separate account, not audited here. | A `test` flow applies the commit's `glidepath/<name>.yaml` (now checked: one TestWorkflow, no service-account choice, no host/secret/projected/PVC volume, no `envFrom`, secrets only via the placeholder) and runs it as Testkube's executor in the shared `testkube` namespace. What the executor's own account can do there is Testkube's, and is not constrained by this chart. |
| **(b) write a release record** | **Yes, by releasing.** Not arbitrary content. | Push a `v1.2.3` tag, or `main`: the release flow's own task writes `release-tracking-*` for that commit, with that commit's `cicd.yaml` as input. With ref protection `required`, a commit nobody reviewed is refused here (see Ref protection); with `off`, it is a release of a commit nobody reviewed, gated downstream (the release PR, Clearance), not here. Forging a record's contents directly needs the token. |
| **(c) read a Secret** | **Yes.** | Not the registry push credential (as in A). This application's own `app-secrets` through a TestWorkflow, by design. Another application's secret copy: the workflow cannot name it (checked at apply; and `vpol-testkube-secret-usage` in the cluster policies). |
| **(d) push to another application's images** | **No known path for repository code** (as in A). | As in A. A push to `main` runs the main build, which publishes the main cache (the one pull requests read): code that reaches `main` is already reviewed or is the owner's, which is the ref-protection caveat below. |

What each parameter on a `.tekton` file can now do: nothing beyond what the platform's Pipelines do with the values it validates. Images
the pipelines RUN are not chosen by the repository: `build.agent` is an enum mapped by the catalog to fixed images, the toolbox,
Semgrep, BuildKit and validator images are catalog values. Fields that name something to PUSH to: `image-repo` (bound to the app);
cloud deploy targets in `cicd.yaml` name functions and services, and the web-identity role they assume is created per target by Airframe and
limited by AWS, not by this chart.

What this release does close, and how that was checked:

- **The repository's scripts, installs and `RUN` lines** (`build.sh`, `test.sh`, `integration-test.sh`, dependency installs, build backends) run
  as `source-runner` with no Kubernetes credential, and the image build no longer shares a pod with the registry push credential
  (see "Image build and push"). `build-artifact`'s pod still mounts the credential for its separate `publish` step (the build step
  does not mount it; `publish` validates the path it pushes). The other build steps and `run-integration-script` mount no registry
  credential; `run-integration-script` shares nothing with `run-testworkflow`'s pod.
- **The image build and the push credential**: `build-image` pushes nothing and mounts no push credential; `push-image`, a separate
  pod as `image-pusher`, validates the OCI layout (symlinks, traversal, sizes, media types, platforms, digests) and pushes it to the
  application's own repository, and every downstream task takes the digest from it. A pull request's build writes no cache. Checked
  by `charts/glidepath-catalog/tests/image_push_split_test.py` (rendered accounts and mounts, hostile layouts, the push step against a fake
  `oras`, the cache rules) and the two admission and identity tests. Not run against a real BuildKit or registry yet: nothing runs on a
  cluster until a Glidepath release (the upgrade note says what to check first).
- **A pull request's `.tekton` file** can no longer widen a run: it must start one of the Pipelines the platform itself starts
  from a file (the list is rendered from the onboarding templates and the flow generator, and a test compares them), pass only the
  parameters that Pipeline takes, with values of the expected shape, and the repository, revision, pull request and namespace
  parameters must equal what Pipelines-as-Code itself recorded for the event. Pipelines that deploy, release or test start only from a
  push to a protected branch or to a tag of the shape `v<major>.<minor>.<patch>`, decided on the full ref Pipelines-as-Code records
  (`source-branch`), so a branch named `refs/tags/v1.0.0` is not a tag. A push records its ref in two spellings, and both are
  accepted for a protected branch: the full ref (`refs/heads/main`, a real push) and the bare name (`main`), which Pipelines-as-Code
  v0.52.0 records for a re-run of a push's check from the git host (check_run / check_suite `rerequested`, `event-type: push`). The
  bare name must equal a `protectedBranches` entry exactly; a bare tag name is never a tag (a tag has no head branch, so such a re-run
  cannot exist). A run that Pipelines-as-Code starts from a comment (`retest-comment`, `incoming`) is not a push and so does not start a
  push-only Pipeline. `image-repo` must be this application's own repository
  (`<registry>/<owner>/<app>` or `-pr`), so a build cannot push into another app's repository or cache; `onboarding-resync` may
  target only `gitops-<app>` under the repository's own owner, and only from a push; a task may run as `source-runner` or
  `glidepath-cloud-deployer` only where the Pipeline's task list says so. It cannot name another ServiceAccount, add a pod template, embed a Pipeline, or start a
  TaskRun. This depends on Pipelines-as-Code writing its own annotations over anything the file sets; that is how its source reads
  and how a run looked live, but it was not exercised against a hostile file, so the upgrade note asks for it to be.
- **Creating a run** needs `pipeline-trigger`, which no task runs as.
- **`helm template` for the two release gates** (`policy-validation-gate`, `clearance-gate`) no longer runs in a pod that holds a token: it
  runs in `render-manifests` as `source-runner`, and the gates read the output as data, bounded and unexecuted ("Rendering is not done where
  the credentials are"). The #249 stop-gap (symlink refusal, no helm stderr in the credentialed pod) is kept as defence in depth in the
  render task. `values-check`'s own-chart render moved too. `clearance-gate.yaml` edits are listed in the pull request.
- **A re-run from a commit comment** (`/retest` on a pushed commit) is recorded by Pipelines-as-Code with an event type its source
  sets from the comment (not `push`; the exact string was not confirmed). A privileged Pipeline then fails the push-only rule and is
  refused: the safe direction. Re-run those from the portal (glidepath-api rerun; see "Runs glidepath-api starts" under Ref protection) or push again.

**Ref protection is two layers.** The admission policy's name check is the first: a push to a configured protected branch name or a
tag of the shape `v<major>.<minor>.<patch>`. It cannot ask the git host whether that branch needs review or who may push the tag, so on
its own anyone who can push `main` (where it is not protected) or a `v1.2.3` tag can start the deploy, test, release and gitops-image-bump
flows, and a main or tag build, for that commit as `pipeline-runner`. The second is [Ref protection](#ref-protection), below: a trusted
first task that asks whether every commit being released was reviewed. It is on by default and is "Off here" only where a cluster sets it off.

What is not closed, in the order to do it:

1. **One shared registry credential** (A(d), B(d), residual): repository code can no longer reach it, but the credential is still one
   credential for every application's repositories, base images and caches, held by the catalog's trusted tasks. A compromise of
   `push-image` (or of another credentialed catalog task) can push anywhere it reaches. Per-application scoping is in "Image build and
   push"; it needs a credential per application repository, or repository-scoped tokens; it is not built.
2. **Workspace trust in credentialed tasks.** `sast-scan` now scans a copy without `.git` or symlinks, and `attest-api-spec` and
   `build-artifact`'s publish step refuse symlinks and traversal. Any other task that runs git or a tool against the shared
   workspace after the build tasks have written to it, while holding a credential, needs the same care. Enumerated: `stage-preflight`
   runs before the repository's code and clones fresh; `build-image` reads the workspace as the BuildKit context and has no token or push credential now, and `push-image` reads only the validated
   OCI layout from a read-only mount; no other catalog task runs git there (`deliver-onboarding-files` clones into its own directory). `build-artifact` still
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
   `cicdRunAdmission.sourceTasks`. A new push or `/retest` is not affected. Since pipeline trust T4 step 3 the same holds for the
   split accounts: a rerun of a `release`, `test`, `bypass-merge-check`, `itsm-check`, `clearance-gate-check`, `agent-*` or
   outcome/rollback run carries entries for `release-record-writer`, `clearance-gate` or `test-runner`, and the policy must allow
   exactly those (task lists: `cicdRunAdmission.recordWriterTasks`, `clearanceTasks`, `testRunnerTasks`, plus the two platform-started
   Pipelines' `itsm-close` and `itsm-close-rolled-back`, which are not in those lists) before such a rerun is admitted.

### Ref protection

*Pipeline trust slices T3 and T3b.* Every privileged Pipeline (`release`, `deploy`, `test`, `gitops-image-bump`) and the `build` starts with the catalog
task `verify-ref-protection`, before anything else, and refuses the run unless **every commit it releases was reviewed**. The
cluster-resolved Pipelines cannot be changed by a `.tekton` file (the admission policy requires the catalog's cluster resolver and refuses an inline
`pipelineSpec`), so the file cannot drop the task or reorder it; every other task of those Pipelines runs after it (a chart test
checks the order for each Pipeline), so nothing pushes an image, deploys or writes a release record first. It runs no repository code, has no
workspace and takes no parameter a file can set: it reads the run's own Pipelines-as-Code annotations (`event-type`, `source-branch`, `sha`,
`repo-url`) and the run's namespace, and calls [glidepath-api](api.md#ref-protection) with its own ServiceAccount token (the app's
`pipeline-runner` may `get` the `refprotection` resource, nothing else). A task that fails the check leaves its reasons in the task log and in
its results, and the run ends red.

**What `protected` means (T3b).** Not "the host's settings say reviews are required": **review evidence for every commit of the release**, read from
GitHub's authenticated facts with the read-only token the platform already mints (contents, pull requests, metadata). A ruleset's bypass list is shown
only to a token that may administer the repository, which the platform does not hold and will not be asked to; and a setting says what is meant to
happen, not what did. The rules, with the reason for each:

| Rule | Why |
|---|---|
| **The range.** The commits after the newest release of the app that its **gitops repository** corroborates **for the target the run is releasing to** (`env` and `cluster`, passed by the guard from the run's own parameters), up to the commit being released, at most 250. A release is identified by facts read **at its merge**, never by its title, body or labels (anyone with write or triage rights on the gitops repository can edit those, even after a merge): a pull request merged into the default branch that changed exactly one release manifest, `<cluster>/<env>/release.yaml` or `values.yaml`, of that target; opened (as GitHub shows it) by one of the platform's accounts (`api.pipelineLogins`); with exactly one commit, its head, whose author and committer GitHub did not resolve to a person (a real release commit carries the unlinked identity `release@glidepath.invalid` and is **unsigned**: read from jfillman/gitops-backstage#303) or resolved to a pipeline login; that commit's trailers `X-App-Repo` (this repository) and `X-App-Commit`; and the manifest **as merged** (the file at the pull request's merge commit) setting an image tag that ends in that commit's sha. Newest merged first: the first that GitHub shows is an ancestor of the commit **and whose own commit has review evidence** starts the range. A commit at or before a release (a rollback, a repeat) is checked alone. **A run with no single target** (the build, and any Pipeline without `env`) uses the strictest reading: the oldest of the environments' newest releases. "The history shows a release" means any merged pull request among the 30 newest that changed a release manifest of the target, **whether or not it passes the checks above**; the history showing none is a first release, the commit alone. | A release carries everything pushed since the last one, not just its tip. The gitops repository is the app's own (the endpoint only answers for the app's repositories), and a dev release does not start a prod range. |
| **The baseline never comes from a release record.** The `release-tracking-*` ConfigMaps are written, and can be deleted, by the pipeline's own ServiceAccount (`pipeline-runner`) until slice T4, so a forged one could name a reviewed commit after an unreviewed direct push and hide it, and deleting them all would turn a released app into a "first release". They are not read at all (their `refProtection` text is a display hint). The gitops history is what the pipeline's token cannot write: a pull request there needs someone to merge it. **If the gitops history shows any change of a release manifest of the target but none can start the range** (unreadable, none opened by a configured login, none corroborated, none an ancestor with its own evidence, or more merged pull requests than the 30 read with none of them a release) **the answer is `unverifiable`, never a first release.** | Closes the forged-forward and deleted-records cases. |
| **What stays open (the adoption boundary).** The baseline's own range is not re-examined: unreviewed commits older than the newest corroborated release of the target are accepted history. That includes every release made while `refProtection.mode` was `off` or before this feature: when the mode becomes `required` they are adopted as history, and only the newest one's own commit must still pass the evidence check to serve as a baseline. The attacker cannot move the baseline forward without making a real release through the gate. **What the pull-request facts do not prove:** a release commit is unsigned and carries an unlinked identity, so a person who can push to a release pull request's branch and merge it, with the platform's account still the opener, and who sets the unlinked identity, is indistinguishable from the pipeline; a person-linked commit is refused. What bounds that is the gitops pull request's own gates (a person must merge it, and `verify-commit-signature` and the approvals run on it). A source repository whose app has no gitops repository (cloud deploys that pin on the app repository) has no history to read: every check is the commit alone. | Said plainly, not hidden. |
| **A merged pull request into the protected branch of this repository.** Each commit must be the merge commit of, one of the commits of, or one of the commits a rebase merge landed from, a pull request that GitHub reports merged, whose base branch is the protected branch and whose base repository is this one (`commits/{sha}/pulls`, paged to 300, then the pull request itself is read). A pull request merged into a side branch that was then fast-forwarded into the protected branch, or into a fork, does not count. A rebase-landed commit counts only inside the contiguous single-parent run of at most (the pull request's commit count) commits ending at its merge commit. | A branch name check alone passes a commit that was reviewed somewhere else. |
| **The merge commit is GitHub's, or the landing is the reviewed content.** An approval covers the pull request's head. A pull request's merge commit that is not that head must be one GitHub made: committer `web-flow` and a verified signature (`commit.verification.verified`), which covers merge commits and squashes (that signature already says GitHub made the commit from the head through its merge button, so no content comparison is added for them). One pushed by hand, a child of the approved head with changes on top or a merge with extra content, has neither and is refused; the head pushed as it is (a fast-forward) is exactly what was reviewed. **A rebase merge** lands the pull request's commits as new commits committed by the merger and carrying no signature, so they count only as a run whose **content** is the pull request's: (a) exactly the pull request's commit count of single-parent commits, each the first parent's child of the next, ending at its `merge_commit_sha`, and none of the pull request's own commits on the branch under it; (b) the files that `compare/{commit before the run}...{tip}` lists equal, file by file (name, status, previous name, resulting blob sha), the files `compare/{that commit}...{reviewed head}` lists (GitHub compares from the merge base). Either list that may have been cut at GitHub's 300-file cap is `unverifiable` for that pull request, never partial. Any difference means the run is not that pull request's landing: its commits have no pull request, `unreviewed`, with "rebase-merged pull request whose landed content differs from its reviewed head: re-run the review or use a merge commit or squash". | Without it a person with push rights lands unreviewed changes on top of a reviewed head and the pull request still shows as merged. **Refused on purpose:** repositories that merge by pushing a merge commit made locally; a rebase over a base that changed the same file as the pull request (the rebase's own merge produces a blob nobody reviewed, so it cannot be told from a tampered one: re-run the review or merge with GitHub's merge commit or squash); a rebase that landed fewer commits than the pull request has. GitHub's [About pull request merges](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/incorporating-changes-from-a-pull-request/about-pull-request-merges) says rebase and merge adds the commits individually and that it "drops commits that were empty to begin with", so the number landed **can be fewer** than the pull request's count (it says nothing about commits that become empty or patches already applied); the exact-count rule then refuses those pull requests (the run reaches back over a commit that is not the pull request's, whose changes differ). A rebase-merged pull request costs **3 more GitHub reads**: the branch history from the merge commit (`commits?sha=…&per_page=count+1`) and the two compares (1 page each for up to 100 files, up to 3). |
| **An approving review that counts.** At least one reviewer whose latest decisive review (approved, changes requested, dismissed; a comment changes nothing) is *approved*, on the pull request's **final head** (the review's `commit_id` equals the head: no push after the last approval), not dismissed, and the reviewer **may push to the repository now** (GitHub's `authorCanPushToRepository`), is not a bot (`type` Bot, a `[bot]` login, or a login in `api.pipelineLogins`), not the pull request's opener, and not the author or committer *login* of any commit in it (GitHub's `web-flow` committer is ignored: it only says the web editor was used). Logins compared without case. | The approval must be an independent person's, of the code that merged. GitHub records an approving review from **anyone who can see the pull request** (any account on a public repository), and such a review does not count toward its own branch protection; so each review is read through GraphQL with `authorCanPushToRepository`, GitHub's own fact for "this author may push", and only an approval with `true` counts (recorded as `approverCanPush`). It was checked live against a public repository: it returns `true` for a reviewer whose `authorAssociation` is CONTRIBUTOR (so the association says too little) and `false` for a bot (testdata/real_reviews_graphql.json). REST's collaborator permission needs push access for the caller (GitHub answered a user token without it `Must have push access to view collaborator permission`), so it is not used, and there is no `author_association` fallback. **Not verified:** that the platform's App token (pull requests: read) is answered the same; if GraphQL answers an error, or does not say, the answer is `unverifiable` (fail closed), never an approval. A second account of the author that appears as a commit author is excluded; a bot cannot approve for a person. |
| **Commits GitHub cannot link to an account.** Identity is only what GitHub authenticates: a commit's author name and email are whatever its pusher set, so a commit with no linked author (or a committer GitHub cannot link, other than the author) is counted, and then the approver must also not be the account that **merged** the pull request. | We cannot show an unknown author is not the approver. The merger is the one account, besides the opener, that acted on it; excluding it is the conservative rule that does not refuse a normal review-then-merge when every author is linked. It does not stop a colluding second person, nor a second account whose commits are unlinked: no check of GitHub's facts can. |
| **A commit with no such pull request** (direct push, force push, an administrator's bypass) makes the verdict `unreviewed`; each commit is named with why. | This is the point of the change. |

**Known limits, not built:** a co-author named only in a commit trailer (`Co-authored-by`) is not a commit author or committer account and can approve; another reviewer's `CHANGES_REQUESTED` does not block a different reviewer's approval (GitHub's own rule is that an outstanding change request blocks a merge); one pull request with approvers who are all colluding people passes.

The merger may be the author (merging one's own pull request after someone else approved it is fine); the record keeps the openers, mergers and
approvers. There is **no override in this version**: the answer carries an empty `accepted` list, the place where an owner-declared baseline (commits
accepted without review, and by whom) will go.

**Verdicts:** `protected`, `unreviewed` (some commit lacks evidence; they are listed), `unverifiable` (GitHub did not show enough: a failed read, a
range of more than 250 commits, a baseline that cannot be used, a ref that is not a protected branch; a response never read in full is never a yes). A
failure to answer at all (the broker, a transport error, a rate limit past the bounded retries) is a 502. Only `protected` lets a run start. `unavailable`
(the host's "not on this plan") no longer exists as a verdict: review evidence needs no rulesets, so a repository on a plan without them is judged on its
reviews.

**The rulesets and branch protection are still read, as a note.** The answer's `configuration` section (and the `[ruleset note: ...]` in the guard's
summary line) says whether the host's settings would require reviewed pull requests, block force pushes and deletion, and who may bypass them, as far as
the read-only token is shown them. It never decides the verdict and a failure to read it is reported in it, not as a failure.

For a **tag**, the tagged commit must be on a configured protected branch (the first one holding it) and the range is read against that branch.

With `required`, a main or tag build that fails the check does not run `build-image`, so it pushes nothing to the main image repository and
writes nothing to the build cache (no task of the build runs before the check; with the build/push split of glidepath#250, `push-image`
runs after `build-image`, which runs after `preflight`, which runs after the check, so it is covered the same way and nothing in this change
depends on that split). A build for a pull request or a side branch passes as "not a push" / "side ref": it writes no main image.
**Chained stages are verified too, against their origin.** `test`, `deploy`, `release` and `gitops-image-bump` are usually started by a Trigger from the
previous stage's CDEvent, not by Pipelines-as-Code, so they have no push annotations of their own. Every stage's `stage-debrief` puts the ref and
sha the chain started from into its domain event (`origin-ref`, `origin-sha`: from that run's own Pipelines-as-Code annotations, or, for a chained
run, the stamp it was given), and the Trigger stamps them on the run it creates as `glidepath.io/origin-ref` and `glidepath.io/origin-sha`. The chained
Pipeline's `verify-ref-protection` verifies *that* ref and sha through glidepath-api exactly as for a push, and refuses a chained run that has no origin,
whose origin sha is not the revision it was started for, or whose origin is not protected. A build of a side branch therefore cannot start a
chain that runs: the next stage asks about the side branch and is refused. The admission policy refuses any `glidepath.io/` annotation on a run a
`.tekton` file creates, so a file cannot stamp an origin. Nothing chains off a refused `test` run either: a guard failure makes the run Failed, its
`testcaserun.finished` carries outcome Failed, and the from-test Trigger requires `Succeeded` (chart tests check the Trigger filter, the
stamp and the debrief). Events emitted by a run started before this release have no origin: their chained stage is refused, so drain in-flight
chains before enabling `required`.

**Runs glidepath-api starts carry a stamped origin too.** None of them has Pipelines-as-Code annotations, so each is stamped by its trusted
creator, never by the caller's input, and its guard verifies the stamp against the git host:

- **Rerun** (`POST /v1/apps/{app}/pipelineruns/{name}/rerun`): the API copies the origin of the run it repeats and nothing else from its annotations: the
  `source-branch` and `sha` Pipelines-as-Code recorded for a push (so a rerun of a main build is verified as main at that commit), the stamp a chained run
  carries, or `glidepath.io/origin-event` for a pull request run (a rerun of a PR build is "not a push"; a privileged Pipeline refuses it). A rerun of
  a rerun keeps the origin; a request cannot change it.
- **Promote and rollback**: the API stamps the commit the release is built from (the source environment's release file, or, for a rollback, the target
  release record's `gitRevision`) and the platform's configured protected branch (`refs/heads/<first of PROTECTED_BRANCHES>`). The stamp is a claim, and
  the guard checks it against the host: the commit must be on the protected branch (`sha` an ancestor of the branch head, or its head) and have review
  evidence. A rollback to an older release is checked as that commit alone (it is at or before a release): an older commit of the protected branch
  still needs its own approved pull request. A commit that never reached the protected branch (a tag-only commit, say) is refused. A first deploy from an image tag is stamped the same way.
- **Tower's own promote path** (the Backstage backend creating the run from the `promote-release` template itself) carries no stamp and is refused
  under `required`; promote through glidepath-api.
- **Owed outside this repository:** the cluster policy that holds glidepath-api's run creations to a shape (`glidepath-api-pipelinerun-shape`, in the cluster
  repositories) must allow the `glidepath.io/origin-ref`, `origin-sha` and `origin-event` annotations; until it does, a rerun, promote or rollback is refused
  at creation. Not verified here.
- **Promotion lineage annotations** (`glidepath.io/origin-chain-id`, `origin-chain-slug`, `origin-trace-id`, `origin-traceparent`,
  `origin-finished-at`) are information, not a gate, and follow the same rule: only glidepath-api sets them, from the build's own run
  (the image repository and commit it resolved itself, [api.md](api.md#promotion-lineage)), and `cicd-run-shape` refuses every `glidepath.io/` annotation
  on a run a `.tekton` file creates, so a repository cannot claim another flow's lineage in a release record or join another flow's trace. The same cluster
  policy must list them; until it does, glidepath-api starts the run without them (the answer says `origin unknown`) instead of refusing it. They name a
  build for a person to follow; nothing authorises a release on them.

**A forged chain event replays, at most, an older reviewed commit.** A chained stage verifies the origin the event named. Someone who could forge a
chain event (the broker authenticates the sending namespace, so that is a compromised task of the same app) cannot name an unreviewed commit, but can name
an older commit of the protected branch together with an `image-ref`: the image reference travels in the event and is not bound to the origin sha. What
binds the image to the commit downstream is the release PR's gates (provenance: signature and SLSA provenance of the image by the platform's identity) and
Clearance's gate; this change does not add a check that the image's source commit equals the origin sha. That is open.

**The setting.** `refProtection.mode` in the catalog values: `required` (the default) or `off`. Nothing else is accepted, and no
hostname or plan is detected in code: only this setting decides. A cluster on a host plan without rulesets (this lab: GitHub Free) sets
`off` explicitly in its own values; the task then asks nothing and records `ref protection: Off here (...)` in its result
`ref-protection-summary`, and the release Pipeline writes that line into the release record as `refProtection`, so the gap is in the evidence.
A verified run records the proof the same way (`verified`, or `origin-verified` for a run verified from its stamped origin), for example
`ref protection: review evidence: 7 commits in 3 pull requests, all approved by someone other than their authors on the final head (main, range 1a2b3c4..5d6e7f8) [ruleset note: protected: ...]`.
The line is a record for people: it is not read back to choose a baseline (see "The range").

**What `off` leaves open.** It is the name check only, so the two tables above go back to what they said before this slice (and the record says "Off here", not
verified):

- *B(b), write a release record*: **Yes, by releasing.** Someone with push rights pushes `main` (when unprotected) or a `v1.2.3` tag and the release
  flow writes `release-tracking-*` for a commit nobody reviewed. Gated downstream (the release PR, Clearance), not at the push.
- *B(c)/(d)*: the same push starts a `test` run, which applies the commit's `glidepath/<name>.yaml` with the app's Secret copied into the
  Testkube namespace, and a main or tag `build`, which pushes to the main image repository and the build cache.
- A pull request still cannot start any of these (admission), and nothing in A changes.

**The target comes from the run itself.** `env` and `cluster` are read from the run's own parameters, the same ones the release task uses for its real target, so the guard and the target agree; admission pins their pattern, not their value. An omitted target uses the widest range (the strictest reading above).

**Push access is read when the check runs.** `authorCanPushToRepository` is GitHub's answer now, not at review time: a reviewer who later gains push access starts counting, one who loses it stops. **It MUST be verified with the platform's real App installation token before any cluster runs `required`** (it was checked live with a user token only): if the App cannot see the field, every check is `unverifiable`.

**A release pull request must be untouched.** A baseline candidate's timeline (`GET issues/{n}/timeline`, event names recorded in testdata from real pull requests) must show no `head_ref_force_pushed`, `reopened` or `head_ref_restored` and exactly one `committed` event: replacing the pipeline's one commit needs a force push, adding to it a second commit, both show there. A refused pull request still counts as history, so with no other usable baseline the answer is `unverifiable`, never a first release. 1 to 3 more reads per candidate.

**Open follow-up:** the release commits should be created through the git host's API so they are signed as the App; the baseline could then require the App's verified identity instead of "unlinked and unsigned".

**No elevated permission.** The platform holds no Administration permission for this: the broker's `/github-read-token` mints only contents, pull
requests and metadata read, whatever a request or a value says (a test posts `administration: write` in the request body and as a leftover environment
setting, and the token stays read-only). `githubRead.refProtectionPermissions` and `githubRead.refProtectionCallers` are gone from the control plane
chart; a cluster values file that still sets them is ignored. glidepath-api no longer sends a purpose; one that still does (older than this change) is
served the same read-only token.

**Calls per check.** One check reads, with the one token of the request and nothing cached: 1 compare (the commit is on the branch); on the gitops repository 1 to 3 pages of closed pull requests, `pulls/{n}/files` for each of the 30 newest merged ones until the target's releases are found, then 4 reads
(`pulls/{n}`, `pulls/{n}/commits`, `issues/{n}/timeline`, the manifest at the merge commit) per release looked at (up to 8, stopping at 5 corroborated), 1 compare per candidate and the baseline's own evidence (below);
1 compare per 100 commits to list the range (at most 3 pages); then for each commit not yet covered, 1 `commits/{sha}/pulls`, and for each pull request found once:
`pulls/{n}` and `pulls/{n}/commits` (1 page each unless a pull request has more than 100 commits), plus one GraphQL query for its reviews (100 to a page, at most 5 pages), and 1 `commits/{sha}` for a merge commit that is not in a range list (a lone commit). A first release of one squash-merged commit is 7 reads;
a range of 20 commits in 5 pull requests is about 50; each rebase-merged pull request in it adds 3 (see the merge-commit rule). **The evidence stops at 400 reads** (`unverifiable`: "the evidence needs more than 400 GitHub reads"); the response says how many
it made (`evidence.calls`). The informational configuration adds about 4 to 6 more for a branch (rules, rulesets, classic protection), more for a tag; it
has its own bounds (5 pages of rules, 3 pages of rulesets, 25 ruleset reads). A GitHub 429, or a 403 that is a rate limit, is retried up to three times
honouring `Retry-After` and `x-ratelimit-reset` (never waiting more than 10 s at once; a longer ask is not waited for), then the call is a 502 saying
GitHub is rate limiting and when to retry; the task refuses on it. Read-only App permissions only.

**What the Clearance gate can call.** The same endpoint, for a release's source commit: `GET /v1/apps/{app}/ref-protection?repo=<owner>/<source repo>&ref=refs/heads/<default branch>&sha=<released commit>`.
`evidence.commits` (each with its merged `pullRequest`), `evidence.pullRequests` (opener, merger, approvers, commits, authors) and the distinct
`evidence.openers`, `mergers` and `approvers` are what the gate's proposer needs, from authenticated GitHub facts, resolved by one implementation in
glidepath-api that the shell tasks call. See [the API reference](api.md#ref-protection).

**Annotated tags.** Pipelines-as-Code records, for a push, the `head_commit.id` of the event (`parse_payload.go`, `case *github.PushEvent`), which GitHub fills
with the commit a tag points at, not the tag object. If it ever recorded a tag object's sha (or the all-zero sha, its fallback when the event has no head
commit), the endpoint refuses: the all-zero sha is rejected at once, and a sha the host does not know as a commit is reported "not a commit (a tag
object)" and never reaches `protected`. A ref with an empty, `.` or `..` segment, or with an encoded character, is a 400.

**Not built:** the owner-declared baseline (the `accepted` list stays empty and there is no override), per-app settings, and a `verify-ref-protection` record on the
`open-release-pin-pr` (cloud) path, which does not write the release record. Release records are still writable by `pipeline-runner` until slice T4 (see the
baseline rule above for what a forged one can and cannot do).

### Upgrade note: a cluster that deploys Glidepath from `main` (the lab does)

**Do this in order.** Merging to `main` deploys the charts at once, and the policies below start refusing runs from `.tekton` files that have not been regenerated.

1. **Cluster values first** (in the cluster repository's Glidepath values, before the chart that has these policies reaches the cluster): `cicdRunAdmission.shapeActions: [Warn, Audit]`, `cicdRunAdmission.sourceIsolationActions: [Warn, Audit]` (and `noTaskRunsActions` if you want it in the window; it has nothing to resync), and `refProtection.mode: off` while the repositories cannot yet show review evidence (the lab sets it). Only platform values set these; a `.tekton` file cannot.
2. **Then the chart** with these policies (`glidepath-control-plane`, `glidepath-catalog`, `glidepath-app` together). The bindings carry the annotation `glidepath.io/enforcement: NOT ENFORCING` and, with prometheus-operator, the alerts `CicdRunShapeNotEnforcing`, `CicdRunSourceIsolationNotEnforcing`, `CicdRunNoTaskRunsNotEnforcing` fire after 3 days. **They must not be left on.** `cicd-run-shape` has never existed on a cluster that synced a release with the CEL defect (fixed in the PR that adds `hack/ci/vap-compile`), so it enforces for the first time here.
3. **Resync every app's `.tekton/`** (the onboarding-resync PR, merged in each repository).
4. **Run one of each, then read what would have been refused**: one build (a pull request build and a push build), one gate check on a release PR (`policy-validation`, `clearance-gate`, `values`), one release. A `Warn` is returned to the client that made the request: Pipelines-as-Code's controller logs it (`kubectl -n pipelines-as-code logs deploy/pipelines-as-code-controller | grep -i warning`, and the PipelineRun creation event on its repository), and an `Audit` writes the annotation `validation.policy.admission.k8s.io/validation_failure` (the policy name, the failing expression's message) into the API server's audit log. Read both for each of the three policies; anything listed is a run that `[Deny]` would refuse: resync that repository or fix the file first.
5. **Switch both to `[Deny]`** (`shapeActions`, `sourceIsolationActions`; the defaults), and remove `refProtection.mode: off` only once the repositories' releases have review evidence. The annotation and the alerts go away.

What each policy refuses, so you know what to look for: `cicd-run-shape` refuses a `.tekton` file that is not a catalog Pipeline with allowed parameters, whose repository, revision, pull request or app parameters differ from what Pipelines-as-Code recorded, a non-push build without `pr-tag-only=true`, an `image-repo` that is not the app's own, an account other than `source-runner`, `image-pusher` or `glidepath-cloud-deployer` on its listed tasks, a pod template beyond `hostAliases`, a workspace claim outside the allowed shape, and a deploy, test or release started from a pull request or an unprotected branch. `cicd-run-source-isolation` refuses a build, test, policy-validation, clearance-gate or values-check run that does not give its source tasks (`build-source`, `unit-test`, `build-image`, `render`, ...) to `source-runner`, or `push-image` to `image-pusher`.

### Upgrade note (the sequence by change)

**Ref protection (T3, T3b): `required` is the default and means review evidence.** Order: (1) glidepath-api (the endpoint, with review evidence, and the control
plane chart, whose broker no longer has an elevation setting); (2) glidepath-app (the `refprotection` grant for `pipeline-runner`); (3) the catalog. A mis-ordered
upgrade explains itself: the task's refusal says "no ref-protection endpoint (404)" or "may not read ref protection (403)" and names this order. **Before step 3
every cluster decides** whether it can pass: with `required`, **a release whose commits were not each approved by someone other than their author (on the final
head, through a merged pull request) is refused.** That includes a commit pushed directly, a force push, a squash of a pull request nobody else approved, and
**a repository with a single maintainer: nobody else can approve, so it cannot pass.** Such a repository sets `refProtection.mode: off` (recorded as "Off here" in
every release) or adds a second reviewer. There is no override yet. If the cluster has a ruleset that requires reviews, nothing more is needed: no
GitHub App permission is requested, `githubRead.refProtectionPermissions` and `githubRead.refProtectionCallers` are removed (a values file that still sets
them is ignored), and the first check after the change starts from the newest release in the gitops history (the commits since it must have review evidence; releases made before are the adoption boundary in "The range"), and `api.pipelineLogins` must name the account that opens release pull requests (an empty list makes every check `unverifiable` where the gitops history shows releases).
Then drain in-flight chains (a chained run with no origin is refused).

**The lab (`refProtection.mode: off`, the three admission bindings in `[Warn, Audit]`) sees no change in behaviour.** The guard asks nothing and records the same
`ref protection: Off here (...)` line (the reason wording now says the commits' review was not verified). The release record, the Pipelines and the admission
policies are untouched.

Order matters because a build run created from a `.tekton` file written before this release has no `taskRunSpecs`.

1. Release `glidepath-app` and `glidepath-catalog` together. The app chart adds `source-runner` and `pipeline-trigger`, moves the
   Triggers and the bootstrap Job to `pipeline-trigger` and removes run creation from `pipeline-runner`; the catalog's onboarding
   task then writes the new `.tekton/` build file. Triggers fire as `pipeline-trigger` from the moment the chart syncs.
2. Release `glidepath-control-plane` with `cicdRunAdmission.sourceIsolationActions: [Warn, Audit]`. `cicd-run-shape` is Deny from
   the start (every file the current onboarding delivers satisfies it, which the policy test checks). A not-yet-resynced build file is let through and warned.
3. Merge the onboarding-resync PR in every app repository (the one that rewrites `.tekton/`). Until it merges that app's build
   still runs its repository code as `pipeline-runner`; it has lost run creation, but not the ConfigMap grant. While any app is in
   this state `sourceIsolationActions` is not Deny, the binding carries the annotation `glidepath.io/enforcement: NOT ENFORCING` and
   (with prometheus-operator) the alert `CicdRunSourceIsolationNotEnforcing` fires after 3 days. **It must not be left on.** `shapeActions` and `noTaskRunsActions` do the same for the other two bindings.
   Also, before the first `[Deny]` of `cicd-run-shape`, check one real file: on a branch, add a bogus `git-revision` to a copy of
   a check's `.tekton` file and confirm the run is refused (the policy compares it with what Pipelines-as-Code records).
   Apps whose flows trigger on a branch other than `main` need `cicdRunAdmission.protectedBranches` set to include it, or their
   deploy, test and release flows are refused.
4. When no build run carries a source-isolation warning, set `sourceIsolationActions: [Deny]` (the default for a new install).
5. Ask the owner of the cluster repository to let glidepath-api reruns carry the `source-runner` entries (item 6 above).

**Token-less render (the release gates).** Adds to the steps above.

- Release `glidepath-catalog` (adds `render-manifests`; `policy-validation-check` and `clearance-gate-check` become three tasks on one
  volume; the gate Tasks gain a required `phase` parameter and workspaces), `glidepath-control-plane` (`cicdRunAdmission.sourceTasks` names
  `render` for both Pipelines) and `glidepath-app` (the onboarding templates) **together, before any gitops repository's resync PR
  merges**: an older control plane refuses a `.tekton` file that names a `taskRunSpecs` entry for `render`.
- **Resync every gitops repository's `.tekton/`** (the onboarding-resync PR). The two check files (`pull-request-policy-validation.yaml`,
  `pull-request-clearance-gate.yaml`) gain a `workspaces` entry (a 1Gi `volumeClaimTemplate` per run) and `taskRunSpecs` for `render` as
  `source-runner`. Until a repository resyncs, its policy-validation and clearance-gate checks **fail** (the Pipeline needs a workspace
  the old file does not give it: Tekton refuses the run, or the gate refuses a volume without its marker). They do not pass.
  While `sourceIsolationActions` is `[Warn, Audit]` a resynced file is admitted either way; in `[Deny]` a file without the entry is refused.
  **What you see before a repository resyncs.** Tekton checks a PipelineRun's workspace bindings before it starts any task, so the check
  cannot say anything itself: the pull request's `policy-validation-` / `clearance-gate-` / `values-` check fails with Tekton's own
  validation error (the PipelineRun's condition names the missing workspace `gate`; the Pipeline needs it, the repository's `.tekton` file
  does not give it), and no task of ours ran to word it better. Read it as: **this repository's `.tekton` files are older than the platform;
  resync it.** Making the workspace optional so a task could explain it would change the Pipelines on every cluster, which this note
  deliberately does not do.
  **How to resync one app.** A resync fires only on a push to the app repository's `main` that changes `cicd.yaml` (a comment-only edit
  does it), and nothing fires one automatically. Push that, wait for the `onboarding-resync` PipelineRun in the app's `-cicd` namespace, and
  merge the resync pull request in the app repository and in its gitops repository (docs/admin/onboarding-mechanics.md, "Triggering a
  resync"). The gitops step logs `N of M template files differ ... opening the resync PR` or `nothing to deliver - M template files
  compared, 0 differ`; since 2026-11-15 that line is read from the tree, not from a pipe's status (an earlier version logged "already up to
  date" for a repository that was behind).
- **Behaviour changes.** A chart with *remote* `Chart.yaml` dependencies renders as before (it needs its `charts/` committed; `helm template`
  never fetched them); the render no longer has the cluster's network to try. A render that takes more than 120 CPU-seconds or writes more
  than 32 MiB a file fails that environment (raise `renderManifests.cpuSeconds` / `maxFileMiB`); more than 50 environments in one run
  fails the run. The gates refuse a render with more than 1,000,000 nodes after alias expansion, 5000 documents or nesting over 64.
  Helm's error text now appears in the **render task's** log (it holds nothing), not the gate's.
- **`values-check` is the third use of `render-manifests`** (profile `values`; `cicdRunAdmission.sourceTasks` lists `render` for it too): the
  `pull-request-values.yaml` template gains the same workspace and `taskRunSpecs`, and an advisory `values-check` goes on after a failed fetch or render (green) as before.
- **Real-file check for the Warn period (workspace claims).** A stored PipelineRun that Pipelines-as-Code created carries the claim as a typed client writes it
  (`volumeClaimTemplate: {metadata: {creationTimestamp: null}, spec: {...}, status: {}}`; Tekton's binding is a `corev1.PersistentVolumeClaim` and Go writes its empty struct
  fields). The rule accepts that shape (the test has it) and refuses a name, generateName, labels, annotations, owner references or a real creationTimestamp in `metadata`, and
  any content in `status`. I could not find a PaC run with a workspace on hangar-dev to confirm the serialised form live: before `[Deny]`, run one check on a branch
  (`kubectl get pipelinerun -o yaml` of a gate run) and confirm the claim has no other key; in `[Warn, Audit]` the policy log shows a refusal that would have happened.
- **Workspace volume claims are now checked by admission.** A `.tekton` file's `volumeClaimTemplate` may carry only `accessModes: [ReadWriteOnce]`, a
  storage request of 1, 2, 5, 10 or 20Gi and, if set, a `storageClassName` from `cicdRunAdmission.workspaceStorageClassNames` (empty by default: unset only). No
  `dataSource`, `dataSourceRef`, `volumeName`, `selector`, `volumeMode` or metadata. The onboarding templates and the flow generator already fit; a cluster whose
  generated build volumes name a storage class must list it. The render refuses inputs without this run's marker in `inputs/`, so a failed `fetch` in an advisory Pipeline renders nothing.
- **Chart sources.** A chart named in `cicd.yaml` (`deploy.chart.repoURL`) must be on an allowed host (`chartSources.gitHosts`, `ociHosts`, plus the default
  chart's); an environment or cluster directory in a gitops repository whose name is not lower case letters, digits and hyphens refuses the check.
- **Volume.** Each run of the two checks now creates a small PVC (inputs, the rendered manifests, the gate's facts); it goes with the run.
  The `source-runner` pod must be able to write the volume (same check as the build volume's first live run: the PVC's `fsGroup` /
  permissions), and the volume's storage class must allow the same PVC to be mounted by two workspaces of one pod by subPath.
- **First live run: verify on one gitops repository before the rest** (nothing here has run on a cluster). In order: the three tasks
  start in order and the PVC is mounted by the gate and the render by subPath; the render writes `rendered/` and the gate's `ingest` reads it
  read-only; the tree digest of an environment equals the one the previous release registered for the same commit (Clearance's record);
  a `.tekton` copy that gives `render` to `pipeline-runner` is refused; `kubectl auth can-i --list --as system:serviceaccount:<ns>:source-runner`
  is empty.

**Image build and push (the build/push split).** This adds to the steps above; the same order applies.

- Release `glidepath-app` (adds the `image-pusher` ServiceAccount), `glidepath-catalog` (splits `build-image`, adds `push-image`, and
  the onboarding task writes `push-image` -> `image-pusher` into each `.tekton/` build file) and `glidepath-control-plane` (the policy
  names `image-pusher` and `cicdRunAdmission.pusherTasks`) **together, before any app's resync PR merges**: an older control plane refuses a
  `.tekton` file that names `image-pusher`.
- Resync every app's `.tekton/` (the onboarding-resync PR, step 3). Until it merges, that app's `push-image` runs as `pipeline-runner`
  (it works, and the build pod still holds no push credential, but the push pod has a token) and, with `sourceIsolationActions` on `[Warn,
  Audit]`, the run is warned; in `[Deny]` it is refused.
- **Pull credential (decide before the first build on the new catalog).** An app whose `Containerfile` `FROM`s a private image, or whose
  cache package is private, needs `buildPullCredential.remoteKey` set in `glidepath-control-plane` (a read-only registry token in the platform
  secret store; see "Image build and push"). Without it a private base image fails the build with a pull error and a private cache reads as
  cold. Public bases are unaffected.
- **Cache behaviour changes.** A pull request's build no longer exports a cache: it reads the main cache only, so its layers that differ
  from main are rebuilt on each push to the pull request. A main build exports its cache to the workspace and `push-image` publishes it
  to the same `<repo>-buildcache:latest` as before (the first build after the release imports the existing one). The old `<app>-pr-buildcache`
  packages are no longer written or read and can be deleted.
- **The workspace volume now holds the image and the cache layout** as well as the source. The default `build.sourceVolume.size` is
  small (2Gi); an application whose image is large (or whose `mode=max` cache is) must raise it before this release reaches it, or the
  build fails with no space left on the volume. `imagePush.maxImageBytes` and `maxCacheBytes` (8 GiB) are the limits `push-image` accepts.
- **First live run: verify on one application before the rest** (nothing here has run on a cluster; the layout formats were written from
  documentation). In order:
  1. The workspace volume is writable by the build step's user (UID 1000): `build-image` now **writes** `.glidepath-out/` (the PVC's
     `fsGroup`/permissions; the source checkout was only read by that step before).
  2. A real `type=oci,tar=false` layout against the validator: the entry names (including `ingest/`), whether BuildKit adds
     attestation manifests (the validator refuses an `unknown/unknown` entry; a single-platform build must be a plain manifest), the
     image config's keys against the allowlist (a refused config names the rule; read the layout before loosening it).
  3. The real `type=local` cache layout: the manifest and layer media types it uses, the layer count of a real `mode=max` cache, and
     `tag=cache` in `index.json`.
  4. `oras cp --from-oci-layout <dir>@sha256:...` on the **read-only** workspace mount. From oras-go's source (v2.5.0, used by oras 1.2.0)
     an OCI layout opened as a copy *source* writes nothing when `oci-layout`, `index.json` and `blobs/` already exist (it creates the
     marker or index only if missing, and `ingest/` only when pushing into a layout), so the mount stays read-only. If a real run
     proves otherwise, copy the validated layout into an `emptyDir` in the push pod, re-verify every hash after the copy, and push from
     there; do not make the workspace writable.
  5. The registry credential reaches `oras`: `DOCKER_CONFIG=/var/run/registry-credentials` in the toolbox image (and `HOME`/working
     directory of the step), for both the push and `oras resolve`.
  6. A multi-arch build pushes an index with **all** child manifests (pull each platform by digest from the registry).
  7. The toolbox image's `oras` supports `cp --from-oci-layout path@digest` and `resolve` (bump `toolboxImage` if it does not).
  8. A pull request's build creates no `-buildcache` tag, and writes nothing to the main repository.
  9. The cache package's visibility: if the cache package is private, the build's import needs the pull credential.
  10. The workspace volume is big enough for the source, the image layout and the cache layout (`build.sourceVolume.size`).
  11. Tekton Chains signs the PipelineRun's results from `push-image` (`IMAGE_URL`, `IMAGE_DIGEST`) and `verify-image-provenance` still
      finds the build provenance.
  12. A rerun of a main build for the same commit logs `reused`, pushes nothing, and its scan, SBOM and attestations use the published `repo@sha256:`; Chains attests no subject for it.
  13. Tekton Chains signs nothing, and attests no build, when `IMAGE_URL`/`IMAGE_DIGEST` are empty (the reused-image case), and does not fail the run.
  14. The tag-existence check: `oras resolve` of a missing tag prints a "not found" the script recognises (any other error fails the task before a push), against the real registry.
  15. The second main build imports the published cache, and `image-scan`, `generate-sbom` and `attest-api-spec` ran against the
      `repo@sha256:` reference.
- Rollback: re-release the previous catalog and app charts; `build-image` then builds and pushes in one pod as before (with the push
  credential in it again).

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
