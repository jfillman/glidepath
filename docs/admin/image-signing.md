# Image + provenance signing (Phase 3 item 2, sub-item 2 of SLSA/Sigstore/Tekton Chains)

Tekton Chains signs every `build` PipelineRun's image and SLSA provenance with a
short-lived, identity-bound cert - the same keyless model as `gitsign` (sub-item 1,
`docs/commit-signing.md`), but for a **cluster workload identity** instead of a human.
No SPIFFE/SPIRE involved (see "Why not SPIFFE/SPIRE" below) - this platform's own
Kubernetes ServiceAccount tokens are the identity source. Policy validation of the
resulting provenance (Conforma/`ec`, sub-item 3) is documented separately in
`docs/provenance-policy.md`.

## Why self-hosted Fulcio, not the public instance

The public Fulcio (`fulcio.sigstore.dev`) only trusts a fixed set of known CI issuers
(`google`/`spiffe`/`github`/`filesystem` as token *providers*, and a curated issuer
allowlist on Fulcio's own side) - it has no way to trust an arbitrary Kubernetes
cluster's own, private `kind` issuer. This platform runs its own Fulcio in-cluster
(`fulcio-system` namespace) instead.

## Why not SPIFFE/SPIRE

Fulcio has a dedicated, built-in `type: kubernetes` OIDC issuer mode - a separate
mechanism from SPIFFE entirely, not a prerequisite for it. Confirmed against Fulcio's own
CI test (`sigstore/fulcio/.github/workflows/verify-k8s.yml`), not guessed:

```yaml
oidc-issuers:
  https://kubernetes.default.svc.cluster.local:
    issuer-url: "https://kubernetes.default.svc.cluster.local"
    client-id: "sigstore"
    type: "kubernetes"
    ca-cert: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
```

Fulcio validates a plain Kubernetes-projected ServiceAccount token directly against the
cluster's own API server and mints a cert with SAN
`https://kubernetes.io/namespaces/{ns}/serviceaccounts/{sa}`. On the Chains side this
pairs with the `filesystem` provider (`signers.x509.fulcio.provider: filesystem`), which
just reads an identity token from a file. No SPIRE agent, no separate trust domain - the
cluster's own existing ServiceAccount token mechanism (which every pod already gets for
free) *is* the identity source. Red Hat's `spire-controller-manager` (or similar) solves
a related but different problem and isn't needed here.

## What's deployed, and what's deliberately still deferred

**Deployed**: Fulcio (`fulcio-system` namespace) - a plain `Deployment`/`Service`,
not Knative (`sigstore-scaffolding`'s own "getting started" install path deploys Fulcio
behind Kourier ingress for *external* test clients authenticating like a human/CI job -
not this platform's case, since every caller here is in-cluster and reaches Fulcio via
plain Service DNS). **2026-09-05: Fulcio's secret bootstrap is now an ArgoCD pre-install
hook Job** (`hooks/fulcio-bootstrap-job.yaml`), not a script an operator runs by hand -
see `docs/admin/adr/0006-cluster-agnostic-bootstrap.md`'s "Update".

**Also deployed as of 2026-09-05**: Rekor + Trillian + MySQL (`rekor-system`/
`trillian-system`), after four earlier attempts destabilized the cluster under the old
podman/kind stack - see `docs/provenance-policy.md`'s full incident writeup for what
those root causes turned out to actually be (largely podman-emulation artifacts, not
real capacity limits) and the real bugs found bringing it up for real on kiac's
arm64-native runtime. Tekton Chains now uploads to it
(`transparency.enabled: "true"`), and verification uses real tlog checking
(`--insecure-ignore-tlog=false`) instead of the flags described below.

**Still deferred**: CTLog, TSA, TUF. None are required for signing to work (confirmed
even Fulcio's own CI test signs with `cosign sign ... --upload=false`), and none of the
gaps they'd close are currently blocking anything real on this platform - the cert-expiry
problem CTLog/TSA would also have addressed is now solved by Rekor's own tlog timestamp
instead (see `docs/provenance-policy.md`'s "Fulcio certs are short-lived" section).

**Practical consequence of no CT log**: certs have no embedded SCT.
`--insecure-ignore-sct` is still needed on `cosign` calls (see Verification below) - a
genuinely separate mechanism from Rekor's artifact tlog, and standing up a CT log isn't
currently justified by any real gap it would close.

## Fulcio configuration - three things that had to be discovered live, not guessed

1. **The issuer URL is cluster-specific.** Fulcio's own CI example uses
   `https://kubernetes.default.svc` (no suffix). On every real cluster this platform
   has run on so far, Fulcio's own OIDC discovery against the API server returns
   `https://kubernetes.default.svc.cluster.local` - using the shorter form fails
   Fulcio's own startup validation (`issuer URL provided to client ... did not match the
   issuer URL returned by provider`). Re-verify this if ever repointing at a different
   cluster.

2. **`ca-cert` must be set explicitly, inline.** Fulcio has a special-case shortcut that
   auto-trusts the cluster's own CA and auto-attaches a bearer token for its outbound
   discovery call - but only when the issuer key is the *exact* hardcoded string
   `https://kubernetes.default.svc`. Since our issuer key is the longer
   `.cluster.local` form (required per #1), that shortcut doesn't fire, and Fulcio's
   discovery request fails TLS trust (`x509: certificate signed by unknown authority`)
   without `ca-cert` (this cluster's own `kube-root-ca.crt`) set explicitly.

3. **The cluster doesn't allow anonymous reads of its own OIDC discovery/JWKS
   endpoints by default.** Even with `ca-cert` fixing TLS trust, Fulcio's request failed
   with a real `403: system:anonymous cannot get path "/.well-known/openid-configuration"`
   - the auto-bearer-token shortcut from #2 doesn't fire either, for the same reason.
   Fixed with a new, narrow `ClusterRoleBinding`
   (`charts/platform-cicd-control-plane/templates/sigstore/issuer-discovery-rbac.yaml`) granting `system:anonymous`
   specifically (not the broader `system:unauthenticated` group) the built-in
   `system:service-account-issuer-discovery` ClusterRole - the same mechanism AWS
   EKS/GKE use to make their own clusters' issuer URLs externally verifiable, not a novel
   pattern. A new binding, not a widened existing one - this platform's consistent
   "additive, don't touch what you don't own" RBAC discipline.

Root CA: a one-time, manually-bootstrapped `fileca` (self-signed ed25519 root, matching
Fulcio's own CI test recipe), stored as the `fulcio-secret` Secret in `fulcio-system`,
never automated via a Job - same precedent as this platform's other one-time bootstrap
secrets (e.g. the GitHub App credential copy in `docs/release.md`):

```bash
openssl req -x509 -newkey ed25519 -sha256 \
  -keyout key.pem -out cert.pem \
  -subj "/CN=platform-cicd-fulcio-root" -days 36500 -passout pass:"<random>"
kubectl create secret generic fulcio-secret -n fulcio-system \
  --from-file=cert.pem --from-file=key.pem
```

CT log is disabled (`ct-log-url: ""` in `server.yaml`) - Fulcio runs without it, "not
recommended for production" per its own docs, an accepted tradeoff (see above).

## Tekton Chains configuration

Not previously installed on this cluster at all - installed as part of cluster bootstrap
(`storage.googleapis.com/tekton-releases/chains/latest/release.yaml`, same convention as
Pipelines/Triggers). Ships one Deployment (`tekton-chains-controller`), no separate
webhook Deployment like Pipelines/Triggers/PaC have.

`chains-config` ConfigMap overlay (`charts/platform-cicd-control-plane/templates/hooks/chains-config-patch-job.yaml`, applied
via `kubectl patch --type merge`, not a full replace - Chains owns this ConfigMap):

```yaml
signers.x509.fulcio.enabled: "true"
signers.x509.fulcio.address: "http://fulcio-server.fulcio-system.svc"
signers.x509.fulcio.issuer: "https://kubernetes.default.svc.cluster.local"
signers.x509.fulcio.provider: "filesystem"
signers.x509.identity.token.file: "/var/run/sigstore/cosign/oidc-token"
artifacts.taskrun.storage: ""
artifacts.pipelinerun.storage: "oci"
artifacts.oci.storage: "oci"
transparency.enabled: "false"
```

`artifacts.taskrun.storage` is deliberately empty (disabled), not `"oci"` as originally
configured here - see "A real bug found in sub-item 3" below for why.

Three more things confirmed live, not assumed from docs:

- **The identity token volume already exists by default.** Chains' controller
  Deployment ships a projected ServiceAccount token (`audience: sigstore`, matching
  Fulcio's `client-id`) mounted at `/var/run/sigstore/cosign/oidc-token` out of the box -
  no patch needed to add it, unlike this sub-item's original plan.
- **`artifacts.taskrun.storage`/`artifacts.pipelinerun.storage` are separate keys, and
  the "tekton" default (annotations on the object) genuinely breaks.** A real
  PipelineRun's full in-toto/SLSA payload exceeded Kubernetes' hard 256KiB
  total-annotations cap (`metadata.annotations: Too long: may not be more than 262144
  bytes`) - Fulcio signing itself succeeded every time; only the write-back storage step
  failed. `"oci"` (provenance attached to the image in the registry, the standard
  cosign/SLSA convention, verifiable via `cosign verify-attestation`) has no such
  ceiling.
- **`transparency.enabled: "false"` is load-bearing, not a placeholder.** Chains' own
  default `transparency.url` is the *public* `rekor.sigstore.dev` - leaving this unset
  would mean this Fulcio-only setup silently attempts to upload every signature there.

**2026-09-05: `transparency.enabled` is now `"true"`, `transparency.url` points at this
cluster's own self-hosted `rekor-server` (not the public default)**, now that Rekor is
deployed - see `docs/provenance-policy.md`. The config block above is left as written
for historical accuracy; the live `chains-config` ConfigMap now differs only in that one
value pair.

**Registry credentials**: Chains signs asynchronously from its own central controller
(watching completed TaskRuns cluster-wide), not from within an Application's pipeline
pod - it resolves registry push credentials via the `secrets`/`imagePullSecrets`
attached to **the ServiceAccount that ran the TaskRun** (`pipeline-runner`, per
Application), not via any credential in Chains' own namespace:

```bash
kubectl patch serviceaccount pipeline-runner -n <app-namespace> --type merge \
  -p '{"secrets":[{"name":"registry-credentials"}]}'
```

**Pipeline-level `IMAGE_URL`/`IMAGE_DIGEST` results - required for Chains to know which
image a PipelineRun's provenance belongs to.** Without these, Chains signs successfully
but logs `No image subject to attest ... Skipping upload to registry` and silently
produces nothing in the registry, despite `chains.tekton.dev/signed: "true"` on the
object - a real, non-obvious gap this surfaced live, not a hypothetical. `IMAGE_URL` is
the bare repo name (no tag); `IMAGE_DIGEST` is `sha256:...`. `charts/platform-cicd-catalog/templates/pipelines/build.yaml`
declares these as Pipeline-level results. One added wrinkle: Tekton's own admission
webhook rejects a Pipeline result whose value is a plain `$(params.*)` reference - it
must be a task-result expression (`$(tasks.*.results.*)`) - so
`charts/platform-cicd-catalog/templates/tasks/build-image.yaml` grew a trivial new `image-repo` result (the bare repo
name, echoing back its own `image-repo` param) purely to give the Pipeline-level result
something to point at.

## Known, accepted RBAC breadth (not narrowed by this platform)

Tekton Chains ships its own `tekton-chains-controller-tenant-access` ClusterRole, bound
cluster-wide, granting `get`/`list`/`watch` on `secrets`/`configmaps`/`serviceaccounts`
across **every namespace** - not something `platform-cicd`'s own manifests introduce.
This is a real, meaningful deviation from this platform's otherwise-consistent
least-privilege posture (TokenReview-scoped broker, per-Application impersonation, resourceName
-scoped ConfigMap reads elsewhere) - flagged here honestly rather than silently accepted,
per this platform's "make tradeoffs structurally loud" precedent. It appears to exist so
Chains can resolve registry credentials via any Application's own ServiceAccount, cluster-wide,
matching the credential-resolution mechanism described above. Narrowing this would need a
careful audit of what Chains' own reconciliation loop actually depends on across
`pods`/`pods/log`/`events`/`pvc`/`statefulsets`/`configmaps`/`secrets`/`serviceaccounts` -
not done here, since breaking Chains' own core signing loop is a worse outcome than the
current breadth. Worth revisiting if this platform ever has an Application whose threat
model doesn't tolerate it.

## Verification

Real end-to-end test performed live (not synthetic): a genuine `build` PipelineRun for
`nodejs-demo-app`, confirmed via:

```bash
kubectl get pipelinerun <name> -o jsonpath='{.metadata.annotations.chains\.tekton\.dev/signed}'
# -> "true"
```

Then the real signature/attestation, verified against this platform's own root CA (not
the public Sigstore trust root - we're not on it):

```bash
kubectl get secret fulcio-secret -n fulcio-system -o jsonpath='{.data.cert\.pem}' | base64 -d > root.pem

cosign verify-attestation \
  --insecure-ignore-tlog=true \
  --insecure-ignore-sct=true \
  --type slsaprovenance \
  --ca-roots=root.pem \
  --certificate-identity-regexp "https://kubernetes.io/namespaces/tekton-chains/serviceaccounts/.*" \
  --certificate-oidc-issuer "https://kubernetes.default.svc.cluster.local" \
  ghcr.io/<org>/<app>@<digest>
```

`--insecure-ignore-tlog`/`--insecure-ignore-sct` are both required and both expected -
direct consequences of deferring Rekor/CTLog (see above), not verification bugs. Real
output confirmed: certificate subject
`https://kubernetes.io/namespaces/tekton-chains/serviceaccounts/tekton-chains-controller`,
issuer `https://kubernetes.default.svc.cluster.local`, `predicateType:
https://slsa.dev/provenance/v0.2`, `builder.id: https://tekton.dev/chains/v2`, real task
list matching the actual PipelineRun.

- Confirmed no Rekor upload attempt in Chains controller logs during a real signing run
  (`transparency.enabled: "false"` genuinely takes effect).
- Security check: `fulcio-secret` unreadable from other namespaces' ServiceAccounts
  (confirmed `pipeline-runner` in an Application's namespace gets `no`); the projected identity
  token's audience (`sigstore`) is narrowly scoped, not a general credential.

**2026-09-05 update: the `--insecure-ignore-tlog=true` call above is now historical -
verification uses `--insecure-ignore-tlog=false` in production** (`--insecure-ignore-sct`
stays, see above). Re-verified live with the same shape of test, standalone rather than
through a real PipelineRun (the webhook trigger path was down for an unrelated reason
this session): a real `cosign sign-blob`/`cosign verify-blob` round trip against this
cluster's real Fulcio + Rekor produced a genuine Rekor entry (`logIndex: 1`, this
cluster's first), fetched back independently from `rekor-server`'s own
`/api/v1/log/entries` endpoint, and verified with `--insecure-ignore-tlog=false`
(`Verified OK`) - confirming the exact mechanism `verify-image-provenance.yaml`/
`verify-sast-attestation.yaml` now use.

## A real bug found in sub-item 3: `artifacts.taskrun.storage: "oci"` broke every signing run

Originally this config also set `artifacts.taskrun.storage: "oci"` (confirmed present and
working at the time this section was first verified). Sub-item 3's real end-to-end testing
found it had silently started breaking signing entirely - every `build` PipelineRun
reported `chains.tekton.dev/signed: "true"` but pushed no `.att` tag to the registry at
all. Forcing a fresh sign attempt (`kubectl annotate ... chains.tekton.dev/signed-
chains.tekton.dev/retries-`) while live-tailing the controller surfaced the real error,
reproduced identically across two independent real builds:

```
expected imageID sha256:f531d4eda8c7... to be separable by @
```

That digest belonged to this platform's own shared toolbox image
(`ghcr.io/jfillman/platform-cicd-toolbox`), not the app image - and the reason became
clear once every step's `containerStatus.imageID` was inspected directly
(`kubectl get taskrun ... -o json | jq '.status.steps[].imageID'`): steps running a
registry-pulled image (kaniko, `node`, `git-clone`) report a properly-qualified
`repo@sha256:digest` imageID, but steps running the toolbox image - at the time
`kind load docker-image`d (or, working around that command's known podman-provider bug,
`ctr images import`ed) rather than pulled from a real registry - reported a **bare**
`sha256:digest` with no registry name to combine with an `@`. Tekton Chains'
PipelineRun-level SLSA materials-gathering walks every constituent TaskRun's step
imageIDs, and chokes hard on the unqualified one, failing the *entire* PipelineRun's
signing - not just skipping that one bad material. Since the toolbox image is used by
nearly every step in every pipeline, this broke signing for every real build.

**Fix, applied in two parts:**

1. **Root cause**: the toolbox image is now actually `docker push`ed to
   `ghcr.io/jfillman/platform-cicd-toolbox` (the toolbox publish step) instead of
   `kind load`ed - so its imageID is always a real, registry-qualified reference. This
   also simplifies onboarding a new toolbox version going forward: a normal push, no
   `docker save`/`ctr images import` dance. (A freshly-pushed GHCR package defaults to
   *private*, unlike `nodejs-demo-app`'s image, which was made public separately at some
   point - rather than also flipping toolbox to public, `pipeline-runner` now carries
   `imagePullSecrets: [{name: registry-credentials}]`, reusing the same Secret kaniko
   already pushes with. See `charts/platform-cicd-app/templates/identity/pipeline-runner.yaml`.)
2. **Defense in depth**: `artifacts.taskrun.storage` is now `""` (disabled) rather than
   `"oci"`. This platform's own provenance consumer
   (`charts/platform-cicd-catalog/templates/tasks/verify-image-provenance.yaml`) only ever checks PipelineRun-level
   attestations, never TaskRun-level ones, so TaskRun-level OCI storage was unused
   surface area, not a feature being traded away.

Re-verified live after both fixes: a real `build` PipelineRun signs successfully and a
real `.att` tag appears in the registry, `cosign verify-attestation` against this
platform's own Fulcio root succeeds with the correct certificate subject/issuer and a
real SLSA provenance payload for the correct image digest.
