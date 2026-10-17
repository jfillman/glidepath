# ADR-0026: Rootless BuildKit for image builds

*Status: Accepted (2026-10-17). Supersedes [ADR-0010](0010-kaniko-rootless-builds.md). Built: `build-image`
runs one rootless BuildKit step for every requested platform; `deploy-lambda` resolves the function's
platform from the image index; the GoApplication scaffold cross-compiles from `$BUILDPLATFORM` (airframe).
Every `-cicd` namespace enforces Pod Security `baseline` (2026-10-17, tenant-onboarding ApplicationSet).*

## Context

ADR-0010 chose kaniko for image builds so they could run under Pod Security `restricted`. By October 2026
three things made that choice wrong:

- **Kaniko is unmaintained.** Google archived GoogleContainerTools/kaniko in June 2025; v1.24.0, the version
  pinned here, was the last release. Chainguard maintains a fork for security fixes only and publishes images
  to its customers, not publicly.
- **Kaniko emulated every cross-architecture build in full.** kiac-dev's build node is arm64 and kind-prod is
  amd64, so most apps build both. Kaniko runs each stage as the target platform and ignores a per-stage
  `FROM --platform=$BUILDPLATFORM`, so every RUN step of the amd64 image ran under QEMU. The Go compiler
  crashes there ([known-gaps.md](../known-gaps.md) #37); a Spring build that compiles inside its Containerfile
  took about 11 minutes for amd64 against 5 for arm64; Backstage spent 11 of its 25 build minutes in
  `build-image`. The two per-arch builds also ran one after the other, then crane assembled the index.
- **The `restricted` claim was never true.** Kaniko needs root inside its container, which `restricted`
  forbids. On kiac-dev the app `-cicd` namespaces carry no Pod Security label and Tekton's
  `set-security-context` is off, so builds ran as root with nothing enforcing any level.

## Decision

`build-image` runs `moby/buildkit:<version>-rootless` (`buildctl-daemonless.sh`), one daemon per TaskRun and
none shared between tenants (a shared buildkitd Deployment would give a warm local cache but would build every
tenant's images in one process, against [ADR-0011](0011-two-namespace-tenancy-model.md)).

- One invocation builds every platform in `build.platforms`, in parallel, and pushes one OCI image index under
  the real tag. A single platform pushes a plain manifest. No per-arch tags, no crane step.
- The layer cache is an OCI cache manifest in a separate package per app, `<image-repo>-buildcache:latest`,
  `mode=max`. A separate package keeps it out of Tower's Images tab and image prune.
- The step runs as UID 1000 under rootlesskit with `--oci-worker-no-process-sandbox`, no privileged container,
  no added capabilities, no host mounts, and **no seccomp or AppArmor override**. That meets Pod Security
  `baseline`. BuildKit's own Kubernetes examples, and the Tekton catalog `buildkit` Task, set both profiles to
  `Unconfined`, which `baseline` rejects.
- No attestation manifests (BuildKit adds none unless asked), so Tekton Chains, Conforma and
  `verify-image-provenance` see the same `IMAGE_URL`/`IMAGE_DIGEST` shape as before.

The switch was made in one step for every app, with no per-app opt-in period.

## Evidence (Phase 0 spike, kiac-dev, 2026-10-17)

| Test | Result |
|---|---|
| Go scaffold-shaped app, `FROM --platform=$BUILDPLATFORM` + `GOARCH=$TARGETARCH`, both platforms, BuildKit | Passed, 37 s including push and cache export; log shows `[linux/arm64->amd64 build]` |
| Same Containerfile, kaniko, amd64 | Failed: `runtime: lfstack.push invalid packing` (#37) - kaniko ignored `$BUILDPLATFORM` |
| Unmodified GoApplication scaffold Containerfile, BuildKit | Failed the same way - the Containerfile has to opt in |
| Kaniko, arm64 only (native) | 27 s |
| Rendered `build-image` Task, both platforms / amd64 only | Passed: OCI index, 42 s cold / plain manifest, 8 s warm; `image-digest` matches the registry |
| TLS with Tekton's injected `SSL_CERT_DIR` | Works without the `SSL_CERT_FILE` override kaniko needed |
| UID 1000, no seccomp/AppArmor fields, namespace enforcing `baseline` | Admitted and built |
| Explicit `Unconfined` seccomp/AppArmor | Rejected by `baseline` |
| `restricted`-shaped (non-root, `allowPrivilegeEscalation: false`, drop ALL, RuntimeDefault), with and without `hostUsers: false` | Admitted by `restricted`, but the build fails: rootlesskit cannot create its user namespace (`newuidmap ... Operation not permitted` / `fork/exec /proc/self/exe: operation not permitted`) |

## Consequences

- Builds meet Pod Security `baseline`, not `restricted`, and only because the kubelet's `seccompDefault` is off.
  A cluster that turns it on needs a Localhost seccomp profile for the build step. Every `-cicd`
  namespace enforces `baseline` since 2026-10-17 (see [security.md](../security.md#build-isolation)).
- A Containerfile that compiles in a `$BUILDPLATFORM` stage builds the other architecture natively. One that
  compiles in a target-platform stage still emulates and is still slow (and still crashes for Go); existing
  apps change their Containerfile to benefit. The `script` + thin Containerfile shape was never affected.
- Lambda needs a single-platform manifest. `deploy-lambda` used to copy the kaniko-era `<tag>-<arch>` tag; it
  now resolves the function's platform inside the index with `crane digest --platform` and checks the built
  architecture.
- The build step is still a non-bash step: the BuildKit image's own `sh` runs one `buildctl` call.
- BuildKit's daemonless state lives in the container filesystem and is discarded with the pod; only the
  registry cache carries over between runs.
