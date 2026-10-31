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
namespaced `Role`/`RoleBinding` scoped to exactly that Application's `pipeline-runner`
ServiceAccount - never a cluster-wide impersonation grant. See
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
