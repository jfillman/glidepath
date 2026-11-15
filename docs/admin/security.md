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

An Application's `-cicd` namespace runs four kinds of code, and they do not share a credential. The trust model, plainly:

| Code | Runs as | Can reach the Kubernetes API? |
|---|---|---|
| The **repository's own code**: `build-source` (`build.sh`), `unit-test` (`test.sh`), `build-image` (the `Containerfile` `RUN` lines), `build-python-wheel-artifact`, `build-go-binary-artifact`, and the dependency installs those run | `source-runner` | **No.** No Role, no RoleBinding, `automountServiceAccountToken: false`, no projected token. `build-image` mounts **no registry push credential** (see [Image build and push](#image-build-and-push)). |
| The **image push**: `push-image`, which holds the registry push credential and runs nothing from the repository | `image-pusher` | **No.** Same as `source-runner`: no Role, no token. It is a separate account so the admission policy can say which task may have it. |
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
  (`deliver-onboarding-files`) carries six entries (`build-source`, `unit-test`, `build-image` and the two artifact builds as `source-runner`, and `push-image` as `image-pusher`), a test run (generated, or chained by the Trigger) carries
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
  so ValidatingAdmissionPolicies in `glidepath-control-plane` (`templates/admission/cicd-run-shape.yaml`) do, for runs created
  by the Pipelines-as-Code controller in a `(app|infra)-*-cicd` namespace only:
  - `cicd-run-shape` (always Deny): `tekton.dev/v1` only, a cluster-resolver `pipelineRef` to one of the Pipelines in
    `cicdRunAdmission.pipelines` (the 18 PR-check and post-merge Pipelines the onboarding templates deliver, and the five flow
    stages), only the parameters each takes, shaped by `paramPatterns` and tied to what Pipelines-as-Code recorded (see below), no
    inline `pipelineSpec`, run-wide account `pipeline-runner`, one `taskRunSpecs` entry per task and only for `source-runner`, `image-pusher`
    (only `push-image`, `cicdRunAdmission.pusherTasks`) or `glidepath-cloud-deployer` (which has its own policy), a `podTemplate` of
    `hostAliases` only, workspaces `emptyDir` or `volumeClaimTemplate` only; a build that a push did not start (a pull request, a
    comment re-run) must pass `pr-tag-only=true`, and `pr-tag-only=true` needs the `-pr` image repository (this is what keeps a pull
    request from being treated as a main build: see Cache below). Also on UPDATE. `cicd-run-no-taskruns` refuses a TaskRun from the PaC identity.
  - `cicd-run-source-isolation`: a run of a Pipeline that runs the repository's code (`build`: `build-source`, `unit-test`, `build-image`
    and the two artifact builds; `test`: `run-integration-script`; the list is `cicdRunAdmission.sourceTasks` and the policy's message is
    rendered from it) must give those tasks to `source-runner`, and `push-image` (`cicdRunAdmission.pusherTasks`) to `image-pusher`. A PR that
    deletes those lines from its `.tekton` file is refused, not obeyed. `cicdRunAdmission.sourceIsolationActions` sets what a refusal does.
  Every expression is `has()`-guarded and the match conditions cannot raise, so a CEL error cannot refuse requests cluster-wide.
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
- shapes are checked before values are read: `layers` and `manifests` are arrays of objects, descriptors have string `mediaType` and `digest` and a non-negative integer **number** `size`, `platform` is an object of strings, annotations are string-to-string and may not name a ref (`org.opencontainers.image.ref.name`, `io.containerd.image.name`: the push is by `layout@digest`, so an alias has no legitimate use); every JSON file is exactly one document with no BOM, no NUL byte and nothing after it; the exit status of every `jq` call is checked, so an element after a bad one cannot go unvalidated;
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

### What remains possible for someone who can open a pull request

Read this before relying on the section above. Two kinds of person, because the platform gives them different things.

**A. Someone who can open a pull request** (a branch in the application repository, or a fork after an owner's `/ok-to-test`). Their code
runs in the build and test stages before anything is merged.

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path.** One unaudited lead. | `build.sh`, `test.sh`, `integration-test.sh`, installs, build backends and `Containerfile` `RUN` lines all run as `source-runner` (no token). The lead: a credentialed task that opens a repository-chosen path (a symlink to its own token) and prints or publishes it. Closed for `attest-api-spec`, `build-artifact`'s publish, `sast-scan`, the TestWorkflow file and the chart renders; `stage-preflight` reading `cicd.yaml` was not audited. |
| **(b) write or delete a release record** | **No known path.** | It needs the token, so it is only as closed as (a). |
| **(c) read a Secret** | **Only the optional read-only pull credential** (`build-pull-credentials`, if the operator supplied one). **Not the registry push credential.** Nothing else known. | A `RUN` line in the `Containerfile` runs in `build-image`, which mounts no push credential and runs as `source-runner`. What it can read is the pull credential, which can only read packages. |
| **(d) push an image to another application's repository, or a base image** | **No known path for repository code.** Not through parameters; not through a cache. | The push credential is only in `push-image`'s pod, which runs none of the repository's code and pushes what the validated layout and the run's parameters say (`image-repo` is bound to the app). A pull request writes no cache, so it cannot poison the cache a main build imports. What stays possible: its own `-pr` image (by design), and a compromise of the trusted push step (see "Image build and push"). |

**B. Someone with push rights to the application repository** (can push `main` where it is not protected, any branch, and `v*` tags). They
get everything in A, and:

| They want | Possible? | Shortest remaining path |
|---|---|---|
| **(a) a Kubernetes token** | **No known path for the platform's service accounts.** Code in the Testkube namespace is a separate account, not audited here. | A `test` flow applies the commit's `glidepath/<name>.yaml` (now checked: one TestWorkflow, no service-account choice, no host/secret/projected/PVC volume, no `envFrom`, secrets only via the placeholder) and runs it as Testkube's executor in the shared `testkube` namespace. What the executor's own account can do there is Testkube's, and is not constrained by this chart. |
| **(b) write a release record** | **Yes, by releasing.** Not arbitrary content. | Push a `v1.2.3` tag, or `main`: the release flow's own task writes `release-tracking-*` for that commit, with that commit's `cicd.yaml` as input. That is a release of a commit nobody reviewed; it is gated downstream (the release PR, Clearance), not here. Forging a record's contents directly needs the token. |
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
  refused: the safe direction. Re-run those from the portal (glidepath-api rerun; see "Runs glidepath-api starts" under Ref protection) or push again.

**Ref protection is two layers.** The admission policy's name check is the first: a push to a configured protected branch name or a
tag of the shape `v<major>.<minor>.<patch>`. It cannot ask the git host whether that branch needs review or who may push the tag, so on
its own anyone who can push `main` (where it is not protected) or a `v1.2.3` tag can start the deploy, test, release and gitops-image-bump
flows, and a main or tag build, for that commit as `pipeline-runner`. The second is [Ref protection](#ref-protection), below: a trusted
first task that asks the host. It is on by default and is "Off here" only where a cluster sets it off.

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
   `cicdRunAdmission.sourceTasks`. A new push or `/retest` is not affected.

### Ref protection

*Pipeline trust slice T3.* Every privileged Pipeline (`release`, `deploy`, `test`, `gitops-image-bump`) and the `build` starts with the catalog
task `verify-ref-protection`, before anything else, and refuses the run unless the git host protects the ref that started it. The
cluster-resolved Pipelines cannot be changed by a `.tekton` file (the admission policy requires the catalog's cluster resolver and refuses an inline
`pipelineSpec`), so the file cannot drop the task or reorder it; every other task of those Pipelines runs after it (a chart test
checks the order for each Pipeline), so nothing pushes an image, deploys or writes a release record first. It runs no repository code, has no
workspace and takes no parameter a file can set: it reads the run's own Pipelines-as-Code annotations (`event-type`, `source-branch`, `sha`,
`repo-url`) and the run's namespace, and calls [glidepath-api](api.md#ref-protection) with its own ServiceAccount token (the app's
`pipeline-runner` may `get` the `refprotection` resource, nothing else). A task that fails the check leaves its reasons in the task log and in
its results, and the run ends red.

| Ref | What the host must show |
|---|---|
| a protected branch (push) | an active ruleset or branch protection: pull requests with at least one approving review **and** approvals that do not survive a later push (`dismiss_stale_reviews_on_push` or `require_last_push_approval`; without one, an approval on a harmless commit still counts after a malicious one is pushed and merged), force pushes blocked, deletion blocked; the commit on the branch; no write/maintain/custom role, team, deploy key or named user able to bypass it (administrators, organization administrators and GitHub Apps are reported, not counted). `require_code_owner_review` and `required_review_thread_resolution` are reported in the verdict's `notes`, not required. The requirements add up across every ruleset and classic protection that apply |
| a release tag (push) | active tag rulesets restricting creation, update and deletion, with the same bypass rule, and the tagged commit on a protected branch that is itself protected as above |

A host that answers "not on this plan" is the verdict `unavailable`, not protected. A host that fails to answer, a ruleset that cannot be read or a
bypass list the token is not shown refuse too (fail closed); nothing is cached, and every list read is paged and bounded.
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
  the guard checks it against the host: the branch must be protected and the commit on it (`sha` an ancestor of the branch head, or its head). A rollback
  to an older release therefore passes **by design**: it is an older, already reviewed commit of the protected branch. A commit that never reached the
  protected branch (a tag-only commit, say) is refused. A first deploy from an image tag is stamped the same way.
- **Tower's own promote path** (the Backstage backend creating the run from the `promote-release` template itself) carries no stamp and is refused
  under `required`; promote through glidepath-api.
- **Owed outside this repository:** the cluster policy that holds glidepath-api's run creations to a shape (`glidepath-api-pipelinerun-shape`, in the cluster
  repositories) must allow the `glidepath.io/origin-ref`, `origin-sha` and `origin-event` annotations; until it does, a rerun, promote or rollback is refused
  at creation. Not verified here.

**A forged chain event replays, at most, an older reviewed commit.** A chained stage verifies the origin the event named. Someone who could forge a
chain event (the broker authenticates the sending namespace, so that is a compromised task of the same app) cannot name an unreviewed commit, but can name
an older commit of the protected branch together with an `image-ref`: the image reference travels in the event and is not bound to the origin sha. What
binds the image to the commit downstream is the release PR's gates (provenance: signature and SLSA provenance of the image by the platform's identity) and
Clearance's gate; this change does not add a check that the image's source commit equals the origin sha. That is open.

**The setting.** `refProtection.mode` in the catalog values: `required` (the default) or `off`. Nothing else is accepted, and no
hostname or plan is detected in code: only this setting decides. A cluster on a host plan without rulesets (this lab: GitHub Free) sets
`off` explicitly in its own values; the task then asks nothing and records `ref protection: Off here (...)` in its result
`ref-protection-summary`, and the release Pipeline writes that line into the release record as `refProtection`, so the gap is in the evidence.
A verified run records the proof the same way (`verified`, or `origin-verified` for a run verified from its stamped origin).

**What `off` leaves open.** It is the name check only, so the two tables above go back to what they said before this slice:

- *B(b), write a release record*: **Yes, by releasing.** Someone with push rights pushes `main` (when unprotected) or a `v1.2.3` tag and the release
  flow writes `release-tracking-*` for a commit nobody reviewed. Gated downstream (the release PR, Clearance), not at the push.
- *B(c)/(d)*: the same push starts a `test` run, which applies the commit's `glidepath/<name>.yaml` with the app's Secret copied into the
  Testkube namespace, and a main or tag `build`, which pushes to the main image repository and the build cache.
- A pull request still cannot start any of these (admission), and nothing in A changes.

**Reading the bypass list needs more than read access: say so before choosing `required`.** GitHub's REST documentation for "Get a repository
ruleset" (and the organization one) states: *the `bypass_actors` property is only returned if the user making the API request has write access to
the ruleset*. The endpoint itself needs only Metadata: read, which is why the ruleset is returned without the field (checked live against public
repositories as an anonymous reader). For a GitHub App installation token the permission that gives write access to a repository ruleset is
**repository Administration: write**; for a ruleset defined at the organization the organization permission **Administration: write**, which a
repository-scoped token cannot carry (so a branch protected only by an organization ruleset stays `unverifiable`). With today's token (contents,
pull requests, metadata read) every ruleset-protected ref is therefore `unverifiable`, and `required` refuses everything on every plan, until the
owner of the platform's GitHub App grants it Administration: write on the installation. The platform does not change the App. Then set the control
plane value `githubRead.refProtectionPermissions: {administration: write}`: the broker adds that permission to the token it mints for glidepath-api's
ref-protection call (purpose `ref-protection`) and to no other token; the default is `{}`, today's read-only set. **There is no mode that treats an
unreadable bypass list as "no bypass"**: the verdict stays `unverifiable`, and its reason names the permission and these steps. Without the
App permission, set `refProtection.mode: off` and read the section above for what that leaves open. Classic branch protection needs Administration: read
and is read with the same token, so it is readable once the above is in place. Rulesets on a private repository need GitHub Team or Enterprise.

**Blast radius of `githubRead.refProtectionPermissions: {administration: write}`: read this, and prefer not to set it.** With it set, the broker mints,
for the ref-protection purpose, a token with repository Administration: write on a source repository of any app that glidepath-api names, valid
for about an hour. Anything that can act as glidepath-api (a compromised glidepath-api, or code running as its ServiceAccount) can therefore edit or disable rulesets
and branch protection, add deploy keys and change settings on every app's source repository, which defeats this guard and more. What the broker
does to limit it: the elevation is bound to the identities in `githubRead.refProtectionCallers` (default glidepath-api only; being allowlisted to read is
not enough, and an empty list elevates nobody), only for `purpose: ref-protection`, one repository per token, never an app's gitops repository, each mint
logged with caller, repository and permissions. It is not separated further: glidepath-api itself holds the credential to ask, so a dedicated
identity would only help if the ref-protection call ran in its own pod, which is not built. **Mark it "only if you accept this risk".** The
alternative being proposed to the owner (verifying each released commit's review evidence with read-only access, so no bypass list is needed) would remove the need; until
then the choices are `refProtection.mode: off` or this setting.

**What a bypass list is allowed to contain.** By design the verdict accepts, and shows in its record, repository administrators, organization
administrators and GitHub Apps on a ruleset's bypass list: they are the platform's and the owners' own door, not "anyone with push rights". Classic
branch protection with `enforce_admins: false` is recorded as an administrator bypass and does **not** fail the check. Write and maintain roles, custom
roles, teams, deploy keys and named users do fail it.

**Calls per check.** A branch: `rules/branches/{b}` (1 page of 100 rules), one `rulesets/{id}` per ruleset that supplies a requirement (typically 1 to 3),
`branches/{b}/protection` (1), `compare` (1); one more `rulesets?per_page=1` only when the host says classic protection is unavailable. About 4 to 6 in
all. A tag: `rulesets` (1 page of 100), one `rulesets/{id}` per active tag ruleset (typically 2), `compare` (1), then the branch calls above for the
protected branch: about 8 to 10. Bounds: 5 pages of rules, 3 pages of rulesets, 25 ruleset reads; beyond them the verdict is `unverifiable`. A GitHub
429, or a 403 that is a rate limit, is retried up to three times honouring `Retry-After` and `x-ratelimit-reset` (never waiting more than 10 s at once; a
longer ask is not waited for), then the call is a 502 saying GitHub is rate limiting and when to retry; the task refuses on it.

**Annotated tags.** Pipelines-as-Code records, for a push, the `head_commit.id` of the event (`parse_payload.go`, `case *github.PushEvent`), which GitHub fills
with the commit a tag points at, not the tag object. If it ever recorded a tag object's sha (or the all-zero sha, its fallback when the event has no head
commit), the endpoint refuses: the all-zero sha is rejected at once, and a sha the host does not know as a commit is reported "not a commit (a tag
object)" and never reaches `protected`. A ref with an empty, `.` or `..` segment, or with an encoded character, is a 400.

**Not built:** an allowlist for a named bypass team or App, per-app settings, organization-level bypass lists, and a `verify-ref-protection` record on the
`open-release-pin-pr` (cloud) path, which does not write the release record.

### Upgrade note

**Ref protection (T3): `required` is the default and needs more than a release.** Order: (1) glidepath-api (the endpoint; the control plane chart also
carries the broker setting); (2) glidepath-app (the `refprotection` grant for `pipeline-runner`); (3) the catalog. A mis-ordered upgrade explains itself: the
task's refusal says "no ref-protection endpoint (404)" or "may not read ref protection (403)" and names this order. **Before step 3 every cluster decides:**
set `refProtection.mode: off` in the catalog values (the lab, GitHub Free: no rulesets), or have the owner of the platform's GitHub App grant it
Administration: write and set `githubRead.refProtectionPermissions: {administration: write}` (see "Reading the bypass list" above). `required` without
that App permission refuses every run. Then drain in-flight chains (a chained run with no origin is refused).

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
