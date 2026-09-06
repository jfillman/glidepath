# Provenance policy validation (Phase 3 item 2, sub-item 3 of SLSA/Sigstore/Tekton Chains)

The last sub-item of SLSA/Sigstore/Tekton Chains: the release-guardrail gate covering
this doc's territory - named `policy-check` when the events below happened, renamed
`provenance` on 2026-08-23 (see docs/admin/release-guardrails.md) - gets a second,
genuinely independent real gate alongside `verify` (gitsign commit signatures,
`docs/commit-signing.md`). `verify-provenance` checks the *promoted image's* own
signature and SLSA provenance attestation (both produced by Tekton Chains,
`docs/image-signing.md`) against Conforma's policy engine. See
`charts/platform-cicd-catalog/templates/tasks/verify-image-provenance.yaml` and
`charts/platform-cicd-catalog/templates/pipelines/provenance-check.yaml`. The rest of
this doc's "confirmed live" sections were written before the rename and quote the
GitHub Check/PipelineRun names as they were observed at the time (`policy-check`) -
left as accurate historical record rather than rewritten.

## Scope decision: additive, not a replacement for gitsign

`ec validate image` is fundamentally an OCI-image-and-attestation validator - it has no
mechanism to check a *git commit's* signature, that's gitsign's job and already works
(sub-item 1). Rather than building plumbing to bridge gitsign's result into something
Conforma could re-check, the Application's existing `allowed-commit-signers` ConfigMap
continues to drive gitsign's check unchanged, and this sub-item adds a genuinely new gate:
does the promoted **image** have a valid signature and SLSA provenance attestation,
conforming to policy. `provenance` ends up with two independent, real gates, neither
`runAfter`s the other, both required. The same ConfigMap is exposed to Conforma as
`ruleData` too - structurally "concatenated" per the original request - even though no
current base-policy rule reads it, ready for a future rule to consume.

## Rekor: deployed 2026-09-05, after four earlier failed attempts - a real, honest incident writeup

The original design for this sub-item was to finish the deferred Rekor+Trillian+MySQL
stack (`sigstore/helm-charts`' `rekor` chart, which pulls in Trillian as a dependency,
bundled MySQL, Redis, an auto-`createtree` Job) so `ec` could check real transparency-log
inclusion, not just signature validity.

Four separate attempts at deploying it destabilized the cluster, across two distinct root
causes:

1. **A genuine, two-level nested cgroup PID limit.** kind's node container has *two*
   separate `pids.max` cgroup levels - an outer `.../libpod-<container-id>.scope/pids.max`
   (kind's own documented 2048 default, but **not** the actual binding constraint) and a
   deeper `.../libpod-<container-id>.scope/container/pids.max`, found via `find $CG -iname
   'kubepods*' -o -iname 'pids.max'`, which was the real constraint - genuinely maxed out
   (2048/2048), causing `fork/exec ... resource temporarily unavailable` errors across
   every component on the node, not just Rekor's own pods. Found and fixed once, held
   clean across every later attempt in this session.
2. **MySQL + Trillian's simultaneous first-boot initialization causing severe, repeated
   memory/CPU/I/O pressure**, even after the PID fix. `docker stats --no-stream` during one
   storm showed genuine memory exhaustion (41.04GB/41.96GB, 97.81%) and heavy block I/O
   (337.8GB) - pointing at I/O contention on this Mac's virtualized virtio-fs disk path as
   the likely root cause. This recurred even after removing MySQL's own memory limit
   entirely (trusting 30GB+ of real spare VM memory), which rules out a simple
   memory-sizing fix. Not conclusively root-caused - this was worked around, not solved.

Recovery each time required forceful intervention: `podman machine stop`/`start`, and when
the podman API itself became unresponsive, `kill -TERM` on the underlying `krunkit`
hypervisor process directly (orphaned `gvproxy` companion processes don't die
automatically and had to be killed manually - they hold stale port bindings). The kind
node container does **not** restart automatically when the podman VM restarts - it needs a
manual `docker start` every time, and the cgroup `pids.max` fix is a live edit on the
running container's cgroup, not persisted, so it had to be reapplied after every restart
too. `kubectl delete pods -A --field-selector=status.phase=Succeeded/Failed` cleanup of
289+ pods accumulated over the session produced a real, measured improvement (35Gi→7.1Gi
memory used) - not just cosmetic.

**Decision, made with the user after the fourth incident: defer Rekor, finish Conforma
without it.** `ec` (like `cosign`) has a real, first-class `--ignore-rekor`-equivalent mode
- signature/cert-chain trust verification is completely unaffected by skipping
transparency-log inclusion checking. The public Rekor is still real and in use elsewhere
on this platform (gitsign's own commit signatures, sub-item 1, verify against
`rekor.sigstore.dev`) - only this platform's own self-hosted image/provenance signing
chain has no transparency log to check against. Rekor is not abandoned, just out of scope
for this pass; `platform/sigstore/rekor-helm-values.yaml` is left in the repo as a
ready-to-retry reference rather than deleted.

**2026-09-05: deployed for real, fifth attempt, on kiac-dev (Apple `container` runtime,
not podman/kind).** Both of the above root causes turned out to be podman-specific: the
cgroup PID limit doesn't exist on kiac's per-VM node isolation, and the "I/O storm" is
now understood to almost certainly have been qemu-user binfmt emulation overhead, not a
real capacity limit - the chart's pinned MySQL image and its netcat init-container image
are genuinely single-arch (amd64-only, confirmed live via `docker manifest inspect`
returning a single manifest, not a multi-arch index), and MySQL's disk-heavy first-boot
schema init is exactly the workload emulation overhead turns into an apparent storm.
Fixed by swapping both for real multi-arch equivalents (`docker.io/library/mysql:8.0`,
`docker.io/library/busybox` for the netcat wait-loop) - `createdb` (a separate,
already-multi-arch sigstore job) applies Trillian's schema itself over a plain
`mysql_uri`, so the old image was never anything more than a stock MySQL server with the
right env vars, a drop-in replacement rather than a re-architecture. Two more real bugs
found in the process: the stock `mysql:8.0` image's probe command
(`/etc/init.d/mysql status`, a Debian/sysvinit path baked into the old amd64-only image)
doesn't exist on it - RPM-based, no `/etc/init.d` at all - swapped for a plain TCP check
on 3306; and the chart's own random-password helper (`trillian.randomSecret`) has a
namespace-lookup bug (checks `.Release.Namespace` but the Secret actually lives in
`forceNamespace`'s `trillian-system`) that silently regenerates MySQL's password on
every single `helm upgrade`, fixed by pinning explicit credentials instead of relying on
the chart's own generator. CPU/memory stayed flat (6-10%/37%) through the entire
install, confirming the historical "storm" really was these bugs, not real resource
contention. Deployed declaratively (`gitops-cluster-dev/50-platform-cicd/rekor/`, the
real upstream chart via ArgoCD) - see docs/admin/adr/0006-cluster-agnostic-bootstrap.md's
"Update" for why no manual `helm install` was needed even for the very first bring-up on
a fresh cluster shape.

## Design: cosign + `ec validate input`, not `ec validate image`

`ec validate image`'s own built-in keyless signature verification needs a TUF-distributed
trust root (`ec sigstore initialize`, defaulting to the public Sigstore TUF CDN). There's
no `--ca-roots`/`--trusted-root`-equivalent flag under `validate image` the way `cosign
verify` has. `ec sigstore initialize --root <path> --mirror file:///...` supports an
air-gapped local static TUF root instead of the live public CDN, but hand-generating a
valid minimal TUF repo (root.json + Fulcio/Rekor target metadata) for a self-hosted-only
Fulcio turned out to be genuinely heavy infrastructure for what should be a simple local
trust anchor - `ec sigstore initialize --root <pem>` rejects a raw PEM outright ("invalid
character '-' in numeric literal", it expects a real TUF `root.json`).

**Resolved with a split-tool design instead**, using each tool for what it's actually good
at:

1. `cosign verify-attestation --ca-roots <fulcio-root.pem> --insecure-ignore-tlog
   --insecure-ignore-sct` - the same invocation already proven working in sub-item 2's own
   verification, needing no TUF at all, just a local PEM. Confirms the attestation's
   signature is valid and was issued by our own Fulcio to the expected cluster identity
   (`tekton-chains-controller`).
2. `ec validate input <decoded-provenance.json> --policy <policy.yaml>` - Conforma's real
   Rego policy engine, run against the already-verified, decoded provenance JSON. This
   variant is pure policy evaluation against arbitrary JSON with no signature/TUF
   involvement at all - a genuinely different code path from `ec validate image`.

Same three logical stages `ec validate image` itself documents (signature, attestation
signature, policy) - stages 1-2 via `cosign`, stage 3 via `ec`.

No standalone bare-image `cosign verify` stage - deliberately. Confirmed live this
platform's Chains config produces attestations (`.att`) but not a standalone image
signature (`.sig`, a distinct Chains feature keyed off a *TaskRun-level* `IMAGE_URL`/
`IMAGE_DIGEST` type hint this platform never added, only a Pipeline-level one). Not a gap
worth closing: the attestation's own signed statement already records the exact subject
digest inside the cryptographically-signed payload, so `verify-attestation`'s signature
check alone already proves "this identity vouches for exactly this image digest."

## How `provenance` finds the app-repo commit and the promoted image

Same trailer mechanism sub-item 1 already built for `verify` -
`open-release-pr.yaml`'s commit carries `X-App-Repo`/`X-App-Commit` trailers - reused
as-is for `verify`. `verify-provenance` doesn't need that pointer at all: the promoted
**image** reference is read straight from the single `*/values.yaml` file the gitops PR
changed (found via `git diff`, not a hardcoded `<app-name>/staging/deployment.yaml`
path - see `verify-image-provenance.yaml`'s own header, 2026-08-16), reconstructed from
its `rollout.image.repository`/`rollout.image.tag` fields - the same manifest
`open-release-pr.yaml` itself already `yq`-patched, so it's already the authoritative
record of what's being promoted, no new plumbing needed.

The tag is resolved to a digest via a direct GHCR registry API call before verification -
`cosign`'s signature/attestation lookup is digest-keyed (the `.sig`/`.att` OCI tags are
derived from the digest), so a plain tag reference genuinely returns "no signatures found"
even when real signatures exist for the resolved digest (confirmed live, not assumed - it
does not resolve tag→digest internally the way a normal image pull does). The registry
call requests **both** Docker v2 and OCI manifest media types in its `Accept` header -
requesting only the older Docker v2 type gets a real, confirmed-live 404 ("OCI manifest
found, but Accept header does not support OCI manifests") for kaniko's OCI-format
manifests, even though the manifest genuinely exists.

## Policy construction

`verify-image-provenance.yaml` builds an `EnterpriseContractPolicy`-shaped `policy.yaml`
at runtime via `jq`, concatenating:

- **Conforma's pre-configured base policy**: `github.com/conforma/policy//policy/lib` and
  `github.com/conforma/policy//policy/release` as **two separate** required
  `sources[].policy` entries (confirmed live - `release`'s rules import from `lib`;
  omitting it fails with "undefined function data.lib.rule_data.get"). `config.include:
  ["@slsa3"]` selects the `slsa3` rule collection - not the safer `minimal` default,
  matching this item's own original framing ("try and achieve a high SLSA level"). A
  deliberate, not-the-safest choice - see "What a real run against this platform's actual
  provenance shows" below for what that surfaces.
- **This Application's own data**: the same `<app-name>-policy-config` ConfigMap
  (`charts/platform-cicd-app/templates/governance/policy-config.yaml`) `verify-commit-signature.yaml`
  already reads, turned into a JSON array and passed as `ruleData.allowed_commit_signers`
  - present and "concatenated" per the original request, unused by any current base-policy
  rule (see "Scope decision" above for why that's fine).
- **`allowed_builder_ids: ["https://tekton.dev/chains/v2"]`** - pins provenance to exactly
  this platform's own Tekton Chains builder identity. Confirmed live this rule's own
  built-in default already matches this exact value, so this doesn't close a live gap -
  it makes an already-correct implicit default explicit and structurally visible, matching
  this platform's "loud, not silent" precedent for trust decisions.
- **A platform-wide `required-tasks` data file** (`sources[].data`, a directory containing
  one `data.json`) - see "Required tasks: verifying the pipeline actually ran" below for
  the mechanism, the file-naming gotcha, and why it's platform-wide rather than per-app
  (confirmed with the user: this describes an invariant of `charts/platform-cicd-catalog/templates/pipelines/build.yaml`
  itself, not an Application preference).

`config.include` also explicitly adds `tasks.required_tasks_found`,
`tasks.required_tasks_list_provided`, and `tasks.data_provided` - confirmed live these
three rule codes are tagged `redhat`/`redhat_security`, not `slsa3`, so `@slsa3` alone
doesn't include them. Added individually by rule code rather than pulling in the whole
`@redhat` collection, which would also enable `trusted_task`'s rules - see "What's
deliberately excluded" below for why that's a real architectural mismatch for this
platform, not an oversight.

No CRD/controller install needed - `ec validate input --policy <file>` works standalone
via the CLI. This platform doesn't run the `appstudio.redhat.com` CRDs or an
"enterprise-contract-service" controller at all, keeping the install surface to a single
pinned CLI binary (`ec_linux_arm64` from `github.com/conforma/cli`'s GitHub releases,
v0.9.88 - the amd64-only `quay.io/enterprise-contract/ec-cli` container image crashes
under this node's arm64 emulation with a real Go-runtime panic, confirmed live), same
footprint as `gitsign` in sub-item 1.

## A real bug found while adding required-tasks: `ec validate input` needs its own attestation wrapping

Extending the policy from "just `@slsa3`" to also check that the real build pipeline
actually ran (below) surfaced a much bigger, previously-invisible bug. While debugging why
a deliberately-bogus required-tasks list produced no violation, `count(data.lib.pipelinerun_attestations)` was checked directly via `ec opa eval` against the exact
same input this Task feeds `ec validate input` - it evaluated to **zero**, even though the
input file plainly contained a real, valid provenance statement.

Root cause, confirmed by reading Conforma's own source
(`policy/release/lib/attestations.rego`): `lib.pipelinerun_attestations` - the function
nearly every `slsa3`-collection rule depends on, directly or via `tasks_from_pipelinerun` -
reads `input.attestations[].statement`, not a bare in-toto Statement as `input` itself.
That wrapping is `ec validate image`'s own internal job (it assembles this shape from a
real image's attestations before invoking policy) - `ec validate input` is a raw JSON/YAML
file validator and does no such wrapping. This Task was handing `ec validate input` the
bare decoded payload directly, so `input.attestations` never existed, and every rule built
on `lib.pipelinerun_attestations` was **silently, vacuously passing** - not because the
check ran and found no problem, but because it had nothing to iterate over.

This means the "two genuine gaps" this doc originally reported (below) were an
undercount: several other `@slsa3` rules that appeared to "Pass" during that testing were
also vacuous, not confirmed-passing. Once fixed, real evaluation surfaced two more genuine,
previously-hidden findings - see "What a real run shows" below for the corrected, complete
picture.

**Fix**: `verify-image-provenance.yaml` now wraps each verified, decoded payload as
`{"attestations": [{"statement": <payload>}, ...]}` via `jq -s` before invoking
`ec validate input`, and passes that one wrapped file instead of the bare payload file(s)
directly.

## Required tasks: verifying the pipeline actually ran

Direct answer to "ensure our build pipeline was actually executed": Conforma's `tasks`
package (`policy/release/tasks/tasks.rego`) has a purpose-built rule for exactly this,
`required_tasks_found` - it does **not** need this platform's full Tekton `Pipeline` spec
as input (an assumption worth correcting explicitly: it was the natural first guess, and
wrong). It wants a plain list of expected task **names**, supplied as
`data["required-tasks"]`: `[{"effective_on": "<date>", "tasks": [...]}]`.

Two things had to be confirmed live, not assumed, to get this working correctly:

1. **Which name it matches against.** Confirmed via `policy/lib/tekton/refs.rego`'s
   `_ref_name()`: it reads the `tekton.dev/task` label Tekton itself automatically
   attaches to every step (`invocation.environment.labels["tekton.dev/task"]` in SLSA
   v0.2) - the underlying **Task CRD name** (`git-clone`, `validate-cicd-config`,
   `send-cdevent`, ...), not the per-step `pipelineTask` name (`clone-repo`,
   `validate-config`, `pipelinerun-started`, ...) `build.yaml` itself uses. Confirmed
   directly against a real provenance document
   (`jq -r '.predicate.buildConfig.tasks[].invocation.environment.labels["tekton.dev/task"]'`)
   before writing the list.
2. **How to actually supply the data file so `ec validate input` sees it.** `sources[].data`
   entries are go-getter references, and a **single-file** local reference
   (`/path/to/required-tasks.yaml`) gets downloaded through `ec`'s own caching layer, which
   renames it to a content hash **without preserving the extension** - OPA's data loader
   then silently ignores it (unrecognized file type), no error, no data loaded. Confirmed
   live via `ec opa eval -d <dir>` and directly inspecting `ec`'s own download workdir.
   **Fix**: reference a **directory** instead of a single file
   (`sources[].data: ["/path/to/data-dir/"]`) - directory downloads preserve filenames, so
   the file just needs a `.json`/`.yaml`/`.yml` extension (any name works; it does **not**
   need to be literally `data.json`, despite that being the convention in Conforma's own
   example files - confirmed live by testing both).

The required list itself (`git-clone`, `validate-cicd-config`, `start-flow-root-span`,
`start-stage-span`, `send-cdevent`, `run-tests`, `build-image`, `end-stage-span`,
`notify-slack`) is every Task that **unconditionally** runs in `build.yaml`
(`resolve-build-agent-image`/`extract-governance-flags` used to be two more entries here
- both folded into `validate-cicd-config` as a performance pass, see docs/chaining.md's
Task-count note, so the live-derived list below is simply shorter now, no policy change
needed) - the three `when:`-gated
governance-stub tasks (`sast-scan`/`image-scan`/`generate-sbom`, all resolving to the same
`governance-gate-stub` Task CRD) are deliberately excluded, since they're legitimately
skipped whenever an Application's `cicd.yaml` disables that gate - including them would make
`provenance` fail for any Application that's turned SAST/image-scan/SBOM off, which isn't what
"the pipeline actually ran" should mean.

### Derived live from the real `build` Pipeline object, not hardcoded

The list above isn't typed into `verify-image-provenance.yaml` as a static array.
`verify-image-provenance.yaml`'s own script runs
`kubectl get pipeline build -n platform-catalog -o json` and derives it with `jq`:

```
[(.spec.tasks // [])[], (.spec.finally // [])[] | select(.when == null) |
 (.taskRef.params[] | select(.name=="name") | .value)] | unique
```

This matches this platform's existing "read fresh from the source of truth" precedent
(`cicd.yaml` is read fresh from the triggering commit on every run rather than synced to a
side copy - the architecture doc explicitly rejected a sync-to-ConfigMap mechanism for the
same reason). A hardcoded list was tried first, and the risk it invites showed up
immediately: a hand-written first pass over `build.yaml`'s source silently missed the two
tasks that live in `spec.finally` (a separate array from `spec.tasks`, easy to overlook
reading the YAML by eye) - exactly the class of drift a static copy invites every time
`build.yaml` changes, and the check would have kept "passing" without ever noticing the
gap. Deriving live makes this structurally impossible to drift: whatever `build.yaml`
currently requires is what gets checked, always. `pipeline-runner` already has cluster-wide
`get`/`list`/`watch` on `pipelines.tekton.dev` in `platform-catalog`
(`charts/platform-cicd-catalog/templates/rbac/catalog-read-only.yaml`, needed for the cluster resolver anyway) - no new
RBAC required.

**Known limitation, not currently hit**: the `jq` above only recognizes resolver types
that put the target name in a `name` param - `cluster` and `hub`, everything `build.yaml`
currently uses. A task added via the `git` resolver (whose params are
`url`/`revision`/`pathInRepo`, no `name`) would be silently dropped from the derived list
rather than erroring loudly. Revisit if `build.yaml` ever adopts the `git` resolver.

### Task parameters can be required too, not just task presence

`tasks.rego`'s own header documents a `name[PARAM=val]` syntax: a required-tasks entry can
mandate a specific invocation parameter, not just that the task ran at all (only a single
parameter per entry; repeat the entry to require more than one). Confirmed live with both
a positive and a negative case against real provenance before relying on it - a required
`git-clone[sslVerify=true]` entry passes when the real value is `"true"`, and a deliberately
wrong `git-clone[sslVerify=false]` entry correctly fails closed with `Required task
"git-clone[sslVerify=false]" is missing`.

Used for exactly one thing today: the derived list rewrites its own `git-clone` entry to
`git-clone[sslVerify=true]`, pinning the clone to have run with TLS verification enabled -
not just that a task named `git-clone` appeared somewhere in the pipeline, but that it ran
with a specific, security-relevant configuration. A small, concrete demonstration of the
mechanism rather than an exhaustive audit of every task's parameters - worth extending if a
specific parameter on another required task becomes worth pinning.

## What's deliberately excluded: `trusted_task` and pinned-ref checks

Conforma's `trusted_task` package (and `tasks.pinned_task_refs`/
`tasks.required_untrusted_task_found`/`tasks.unsupported`) verify that every Task resolved
in the build came from a **pinned OCI-bundle digest**, checked against a trusted-bundle
allowlist - Red Hat AppStudio/Konflux's own Task-distribution model. This platform
deliberately doesn't use that model: catalog Tasks resolve live via Tekton's `cluster`
resolver (from this platform's own `platform-catalog` namespace) and `hub` resolver
(`git-clone`), neither of which is a bundle reference.

Confirmed live via Conforma's own ref-parsing (`policy/lib/tekton/refs.rego`'s
`task_ref()`): it has explicit branches for bundle references and `git`-resolver
references, and falls through to a generic `"pinned": false, "key": "<UNKNOWN>"` case for
everything else - including `cluster` and `hub`. Enabling `trusted_task` here wouldn't
surface a real trust problem; it would fail every single task in every single build,
always, because Conforma has no way to recognize this platform's own resolver types as
trustworthy - a structural blind spot in the rule, not a finding about this platform's
catalog. Deliberately excluded from `config.include` rather than force-included and
explained away; revisit if this platform ever adopts OCI-bundle-resolved Tasks.

## Fulcio root CA, exposed safely as a separate object

`verify-image-provenance.yaml` needs the same public Fulcio root CA sub-item 2 uses for
its own verification, but the private signing key lives in the same `fulcio-secret`
Secret and Kubernetes RBAC can't scope access to one key within a Secret - only to the
whole object. `charts/platform-cicd-control-plane/templates/sigstore/fulcio-root-configmap.yaml` splits the public cert.pem
into its own `fulcio-root-ca` ConfigMap in `platform-catalog`, readable by
`system:serviceaccounts` broadly (genuinely public data, same trust level as Fulcio's own
`/api/v2/configuration` endpoint) via a `Role`/`RoleBinding` scoped by `resourceNames` to
exactly this one object. Verified live: `pipeline-runner` can `get` `fulcio-root-ca` but
cannot `get` `fulcio-secret`.

## A real bug found during this sub-item's own end-to-end testing: Chains couldn't sign at all

Real end-to-end testing (a genuine push through build→test→deploy→release→gitops PR)
surfaced that Tekton Chains had silently stopped producing attestations for *any* real
build - `chains.tekton.dev/signed: "true"` was set, but no `.att` tag ever appeared in the
registry. Forcing a fresh sign attempt (`kubectl annotate ... chains.tekton.dev/signed-
chains.tekton.dev/retries-` while live-tailing the controller) surfaced the real error,
reproduced identically across two independent real builds:

```
expected imageID sha256:f531d4eda8c7... to be separable by @
```

That digest belonged to this platform's own shared toolbox image, not the app image.
Root cause: `kubectl get taskrun ... -o json | jq '.status.steps[].imageID'` showed every
step running a registry-pulled image (kaniko, `node`, `git-clone`) reports a
properly-qualified `repo@sha256:digest` imageID, but every step running the toolbox image
- at the time only `kind load docker-image`d (or, working around that command's known
podman-provider bug on this machine, `ctr images import`ed) rather than pulled from a real
registry - reported a **bare** `sha256:digest` with no registry name to combine with an
`@`. Tekton Chains' PipelineRun-level SLSA materials-gathering walks every constituent
TaskRun's step imageIDs, and a single unqualified one fails the *entire* PipelineRun's
signing, not just that one material. Since the toolbox image is used by nearly every step
in every pipeline, this broke signing for every real build.

Fixed in two parts (see `docs/image-signing.md` for the full writeup and the updated
`chains-config-patch.yaml`):

1. The toolbox image is now actually `docker push`ed to
   `ghcr.io/jfillman/platform-cicd-toolbox`  instead of
   `kind load`ed, so its imageID is always registry-qualified. A freshly-pushed GHCR
   package defaults to private, so `pipeline-runner` now carries `imagePullSecrets:
   [{name: registry-credentials}]` (reusing the Secret kaniko already pushes with) rather
   than also flipping the package to public.
2. `artifacts.taskrun.storage` is now `""` (disabled) rather than `"oci"` - this
   platform's own provenance consumer only ever checks PipelineRun-level attestations, so
   TaskRun-level OCI storage was unused surface area, not a feature traded away.

## What a real run against this platform's actual provenance shows

After the attestation-wrapping fix above, `verify-provenance` was re-run for real (a fresh
signed commit, fresh Chains signature, real gitops PR). `verify` (gitsign) passed - the
test commit was genuinely gitsign-signed. `verify-provenance` ran real cryptographic
verification (passed - correct certificate subject `tekton-chains-controller`, correct
issuer, real SLSA provenance payload for the correct digest) followed by `ec validate
input` policy evaluation that now genuinely exercises every included rule (not vacuously,
per the fix above). With the correct input shape, real evaluation reports **four** genuine
findings - two more than this doc originally reported, both previously hidden by the
vacuous-pass bug:

```json
{"msg":"Build task not found","metadata":{"code":"slsa_build_scripted_build.build_task_image_results_found"}}
{"msg":"The attested material contains no source code reference","metadata":{"code":"slsa_source_correlated.attested_source_code_reference"}}
{"msg":"Expected source code reference was not provided for verification","metadata":{"code":"slsa_source_correlated.source_code_reference_provided"}}
{"msg":"No materials match expected format","metadata":{"code":"slsa_source_version_controlled.materials_format_okay"}}
```

All four are honest, structural gaps in this platform's current provenance shape, not
policy misconfiguration or a regression from the fix:

- **`build_task_image_results_found`**: this rule looks for a Task whose own results are
  literally named `IMAGE_URL`/`IMAGE_DIGEST` (Tekton Chains' type-hinting convention at the
  **TaskRun** level). This platform only ever added that type hint at the **Pipeline**
  level (`build.yaml`'s own `results:` block, see `docs/image-signing.md`) - deliberately,
  since Chains only needed it there to sign at all. This rule wants the narrower,
  task-level convention too; not currently provided.
- **The three source-correlation/materials findings**: Tekton Chains' own materials only
  ever include catalog-task/tool-image references, never the git repo/commit actually
  being built (`git-clone`'s own task results don't capture author/committer info, and
  Chains isn't configured for the kind of deep-inspection that would promote arbitrary
  task results into materials) - the same underlying gap, now correctly flagged by three
  separate real rules instead of appearing to be covered by rules that were actually
  vacuous.

Meanwhile `tasks.required_tasks_found`, `tasks.pipeline_has_tasks`,
`tasks.successful_pipeline_tasks`, and `slsa_build_build_service.slsa_builder_id_accepted`
all now report genuine, confirmed passes - verified by first proving they correctly
**fail** against a deliberately-bogus required-tasks list (below), then re-running against
the platform's real, correct list.

The real GitHub Check on the actual PR reflects the overall result honestly:
`Pipelines as Code CI / policy-check: completed/failure` - genuinely blocking merge per
the branch protection configured in `docs/release.md`, not just failing in isolation.
Choosing `@slsa3` (not `minimal`) deliberately surfaces this rather than hiding it; the
documented, supported escape hatch if this proves too strict for real day-to-day use is
`sources[].config.exclude` on specific rule codes, not a wrong architecture.

**Required-tasks negative test**: a policy built with a deliberately nonexistent required
task name (`totally-bogus-task-one`) against this platform's real provenance was confirmed
to fail closed with `"Required task \"totally-bogus-task-one\" is missing"` -
`tasks.required_tasks_found` genuinely evaluates the real task list, not a vacuous pass.
**Task-parameter negative test**: same real provenance, `git-clone[sslVerify=false]`
(the real value is `"true"`) also correctly fails closed with the same message shape,
confirming the parameter-matching syntax genuinely checks the parameter value, not just
task presence.

**Negative test, confirming the other failure mode**: `cosign verify-attestation` against
a pre-Chains-setup image tag (built before Tekton Chains existed on this cluster) fails
cleanly with `no matching attestations` - the "no attestation exists at all" case, distinct
from "attestation exists but fails policy" proven above.

## Closing the two remaining gaps

All four findings above are now fixed - `verify-provenance` reports a genuine, zero-
violation pass (`"success":true, "violations":[]`, `success-count: 22`) against a real
build, confirmed on the real GitHub Check (`policy-check: completed/success`).

**`build_task_image_results_found` / `subject_build_task_matches`**: `build-image.yaml`
now emits two new **Task-level** results, `IMAGE_URL`/`IMAGE_DIGEST` (exact case), duplicating
the same values already computed for the existing `image-repo`/`image-digest` results.
Confirmed live before assuming it was safe: this reads
`predicate.buildConfig.tasks[].results[]`, which Chains populates for every task
regardless of `artifacts.taskrun.storage` - completely unrelated to the signing-storage
bug fixed earlier in this doc, not a risk of reintroducing it. Tested in isolation first
(a real build, confirmed `chains.tekton.dev/signed: "true"` still worked) before moving on,
given the stakes of getting this wrong.

Two real bugs surfaced while wiring this up:

1. Kaniko's `image-digest` is a **Task**-level result (`--digest-file=$(results.image-digest.path)`),
   not a step-level one - a cross-step step-result reference to it silently resolves to the
   literal, unresolved placeholder string rather than erroring. Task-level result files
   live at the stable, documented `/tekton/results/<name>` path regardless of which step
   wrote them - read directly from there instead.
2. Tekton's admission webhook rejects a result-path reference used inline in `script:`
   once the result belongs to a different, earlier step ("stepResult substitutions are
   only allowed in env, command and args") - and, non-obviously, this validation is a
   **plain string match against the whole script text**, with no awareness of bash
   comments. Writing the offending syntax inside a `#`-prefixed explanatory comment
   trips the same rejection as using it for real. Worded the comment around the
   pattern instead of quoting it literally.

**`attested_source_code_reference` / `source_code_reference_provided` /
`materials_format_okay`**: two separate, real fixes were needed, since these two rules
read from two genuinely different places:

1. `build.yaml` gained two new Pipeline-level results, `CHAINS-GIT_URL`/`CHAINS-GIT_COMMIT`
   (Tekton Chains' own real, documented type-hint convention for promoting a task's
   source-checkout results into `predicate.materials` - confirmed via
   `tekton.dev/docs/chains/slsa-provenance`, not guessed), sourced from `clone-repo`'s
   own `url`/`commit` results. Same Pipeline-level-result pattern already proven for
   `IMAGE_URL`/`IMAGE_DIGEST` - not the separate `artifacts.pipelinerun.enable-deep-
   inspection` auto-discovery mechanism, which isn't needed for an explicit Pipeline-level
   result. This closes `attested_source_code_reference` and `materials_format_okay`,
   which read `predicate.materials` itself.
2. `verify-image-provenance.yaml` now also supplies `input.image.source.git.url`/
   `.revision` in its wrapped document - a genuinely separate field
   `slsa_source_correlated.rego`'s `_expected_sources` reads directly off plain `input`,
   not off the attestation at all. Extracted independently from `git-clone`'s own task
   results (the one true source), not round-tripped back out of the newly-populated
   materials entry. This closes `source_code_reference_provided`.

## A real operational constraint: Fulcio certs are short-lived, and there's no TSA

Hit live while re-testing against an already-open PR after a long gap: `cosign
verify-attestation` failed with `no matching attestations: expected a signed timestamp to
verify an expired certificate`. This isn't a bug - Fulcio-issued certs are deliberately
short-lived (minutes), and normally Rekor's transparency-log inclusion proof (or a
Timestamp Authority) is what lets a verifier confirm a signature was made *while* the cert
was still valid, long after the cert itself has expired. Since this platform deliberately
deferred both Rekor and TSA (see above), a signature genuinely becomes unverifiable once
its cert's short validity window passes - an honest, accepted consequence of that
deferral, not a gap in `verify-image-provenance.yaml` itself. In practice this isn't an
issue for the real automated flow (`provenance` runs within seconds of the image being
signed, well inside the validity window) - it only surfaces when manually re-testing
against a stale signature, as happened here.

**2026-08-23 update: this assumption no longer holds, confirmed live - see
docs/admin/release-guardrails.md's "Known open issue" section.** The release-PR check
now genuinely runs minutes after signing (test+deploy+release all run first), not
seconds, and Fulcio's leaf certs expire in exactly 10 minutes - `sast` (a different
Task, same underlying problem) was caught failing closed on a real release PR for
precisely this reason. Revisit this paragraph once that's resolved.

**2026-09-05 update: resolved - Rekor is deployed, closing this gap for real.** See
the "Rekor" section above for the full history; short version, this platform now has a
self-hosted Rekor+Trillian+MySQL instance, Tekton Chains uploads to it
(`transparency.enabled: "true"`), and `verify-image-provenance.yaml`/
`verify-sast-attestation.yaml` do real tlog verification (`--insecure-ignore-tlog=false`)
instead of the flag that made this whole section necessary. cosign now uses the Rekor
entry's own `integratedTime` to prove the cert was valid at signing time, independent of
how long release-time re-verification takes. Live-verified end to end via a standalone
sign/verify round trip against the real cluster (not just unit-level): a real Fulcio
cert, a real Rekor entry (`logIndex: 1`, this cluster's first), fetched back
independently from `rekor-server`'s own API, and `cosign verify-blob
--insecure-ignore-tlog=false` returning `Verified OK` against it.

## A real, unrelated incident hit mid-testing: the cluster's disk filled to 100%

While re-verifying the two gap-closure fixes, a `deploy` PipelineRun died mid-run with
`no space left on device` writing a Tekton init binary - nothing to do with this platform's
own code. `df -h` on the kind node showed the root filesystem at 100% (453MB free of 93GB).

Root cause, found by checking what was actually consuming space: **MinIO's PVC in the
pre-existing `observability` namespace was using 27GB** (`du -sh
/var/local-path-provisioner/pvc-*-observability_minio`) - unrelated shared infrastructure
on this cluster, not anything from platform-cicd's own churn. A safe, reversible
`crictl rmi --prune` (removing unused container image layers) reclaimed a little space but
wasn't the real fix.

The actual root cause, one layer deeper: the podman VM's own virtual disk was already
379GB (`podman machine inspect`), but the VM's root **partition** had only ever been sized
to 93GB (`lsblk` inside the VM showed `vda` at 379G with `vda4` - the root partition - at
only 92.5G) - roughly 286GB of already-available disk was simply never allocated to the
partition. Fixed live, no VM restart needed: `growpart /dev/vda 4` to extend the partition
into the unallocated space, then `xfs_growfs /` to grow the filesystem to match (XFS
supports online/live growth of a mounted root filesystem). Went from 93GB/94% full to
379GB/25% full in under a minute.

Separately, at the user's explicit direction, MinIO's old 27GB of accumulated data was
also cleared for a genuinely fresh start: scale the `minio` Deployment to 0 (release the
volume), clear the local-path-provisioner backing directory's contents **including the
hidden `.minio.sys` metadata directory** (a plain shell glob `*` doesn't match dotfiles -
confirmed live it left `.minio.sys` behind on the first pass), then scale back to 1. MinIO
came back up healthy against empty storage.

Net result: 379GB/18% used, 314GB free - real, lasting headroom, not just a one-time
reclaim. Worth remembering for future sessions: if a pod fails with `no space left on
device` on this cluster again, check `lsblk`/lsblk-equivalent partition sizing before
assuming the VM itself needs a bigger disk allocation - the disk may already be big enough.

## Verification performed (live, not synthetic)

- Real end-to-end signing test: a genuine `build` PipelineRun, confirmed
  `chains.tekton.dev/signed: "true"` and a real `.att` tag landed in the registry.
- `cosign verify-attestation` against the real image, using this platform's own Fulcio
  root: succeeded, correct certificate subject/issuer, real SLSA provenance payload for
  the correct digest.
- Real end-to-end policy-check test: a real gitops-repo PR from a real release chain,
  `verify-provenance` genuinely ran `cosign` + `ec validate input` against real data and
  reported real, expected findings (not silently passing, not erroring out).
- Real GitHub Check Runs API confirms `policy-check: completed/failure` on the actual PR.
- Negative test: `cosign verify-attestation` against an image with no attestation at all
  fails closed with `no matching attestations`.
- RBAC check: `pipeline-runner` can `get` `fulcio-root-ca` (public) but not `fulcio-secret`
  (private key) - confirmed via `kubectl auth can-i`.

## Phase 3 item 8.7 fallout: cosign's `--ca-roots` deprecation, and an unresolved SBOM/provenance conflict

Building real SBOM generation (`charts/platform-cicd-catalog/templates/tasks/generate-sbom.yaml`, Trivy + a real
keyless cosign attestation) surfaced two real issues in this Task's own
`verify-attestation` call, one fixed, one genuinely still open.

**Fixed: `--ca-roots` is deprecated and stopped working outright.** The moment
`generate-sbom.yaml`'s attestation (signed via cosign 3.x's newer `--signing-config`/
`--trusted-root`-based flow, since `attest` has no `--fulcio-url`/`--rekor-url`/
`--oidc-issuer` flags at all anymore - confirmed live via `--help`, not assumed from
older cosign docs) lands on an image, this file's own `cosign verify-attestation --ca-
roots <pem>` call - checking the *unrelated* SLSA provenance attestation - started
failing outright: `unsupported: CA roots/intermediates must be provided using
--trusted-root when using --new-bundle-format`. Fixed by switching this call to the same
`cosign trusted-root create` + `--trusted-root <json>` mechanism `generate-sbom.yaml`
already uses for its own signing side, built from the same `fulcio-root-ca` ConfigMap.
Re-verified end to end afterward with a genuinely clean, real chain (`policy-check4x5ch`):
`success-count: 22`, zero violations, same as before this fix - this migration is a real
improvement independent of the SBOM conflict below, worth keeping regardless of how that
gets resolved.

**Still open: a real, disclosed conflict between the two attestation types.** Even with
`--trusted-root` in place, once `generate-sbom.yaml`'s attestation actually coexists on
an image alongside Chains' SLSA provenance attestation, this file's
`--type slsaprovenance --certificate-identity-regexp <chains-controller-regex>` call
started failing with `no matching attestations: failed to verify certificate identity: no
matching CertificateIdentity found, last error: expected SAN value to match regex ...,
got ".../platform-cicd-demo/serviceaccounts/pipeline-runner"` - i.e. cosign appears to be
checking the SBOM attestation's identity against the regex meant for the *provenance*
attestation, despite `--type` supposedly scoping the lookup to `slsaprovenance` only.
Confirmed this is not caused by `verify-image-provenance.yaml`'s own `config.include`
list (the failure happens inside cosign's own attestation discovery/identity matching,
before `ec`/Conforma ever runs - reproduced identically with and without the SBOM rule
packages included) and not caused by `--type` shorthand ambiguity (an explicit full
predicate-type URI, `https://slsa.dev/provenance/v0.2`, produced the identical error).
Root cause narrowed further (2026-08-10 session), still not a cosign fix, but the actual
trigger condition is now understood precisely rather than "a genuine cosign 3.x
behavior." Confirmed live via `cosign tree` against a real, repeatedly-checked image: a
single digest carried **nine separate CycloneDX SBOM attestations**, each attached via
cosign's newer OCI-referrers mechanism, alongside the one real SLSA provenance
attestation attached via the older tag-based (`.att`) mechanism. The immediate cause was
`generate-sbom.yaml` running unconditionally on every release-PR check (sbom-check.yaml's
own design) with no check for an existing attestation - any image digest checked by more
than one PR (duplicate/retried release PRs against the same promoted commit, e.g. from a
webhook redelivery storm) accumulated a new SBOM attestation on every single check, never
deduplicated. Confirmed `--type`/`--predicate-type` filtering works correctly in
isolation - `cosign download attestation --predicate-type slsaprovenance` against the
same contaminated digest cleanly returns exactly the one real attestation, no
cross-contamination. The bug is specifically in `cosign verify-attestation`'s own
attestation-*discovery* step (which apparently walks OCI-referrer-attached artifacts
alongside tag-based ones regardless of `--type`) failing closed on every non-matching
referrer it finds, before ever reaching the one real match - not a `--type` filtering
bug, confirming the earlier finding, just now with a concrete mechanism instead of "a
genuine cosign 3.x behavior."
Fixed: `generate-sbom.yaml` now checks `cosign download attestation --predicate-type
cyclonedx` first and skips generation if one already exists for that exact digest, so a
digest can never accumulate more than one SBOM attestation going forward. This does NOT
fully resolve the conflict - the doc's own prior testing already established that even a
*single* coexisting SBOM attestation was sufficient to trigger it - but it stops the
active compounding and removes the specific, controllable trigger this session traced it
to. Fully resolving `verify-attestation`'s own discovery bug would mean either patching/
downgrading cosign, or bypassing `verify-attestation` entirely in favor of manually
verifying a `cosign download attestation`-fetched bundle's certificate against Fulcio
directly (a real rewrite of security-sensitive verification logic, deliberately not
attempted this session without more consideration - see this platform's own "a little
duplication is safer than re-touching an already-verified, security-sensitive Task"
precedent elsewhere).

**Current state, until this is resolved**: `sbom.found`/`sbom_cyclonedx.cdx_supported_
version` are deliberately left out of `verify-image-provenance.yaml`'s `config.include`
(see that file's own comment), and `nodejs-demo-app/cicd.yaml` has `governance.sbom` back
to `false` - do not enable both `governance.sbom` and `governance.policyCheck` for the
same app until this conflict is actually fixed; note the idempotency fix above reduces
how often the conflict triggers but does not eliminate it. SBOM generation itself works
correctly and independently (verified live: a real, discoverable CycloneDX attestation
via `cosign tree`/`cosign download attestation` against a real freshly-built image, in
both the `build.yaml` and gitops-repo PR-check call sites) - it's specifically the
*coexistence* with provenance verification that's broken, not the SBOM feature on its
own.
