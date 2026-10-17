# Rootless image builds

`charts/glidepath-catalog/templates/tasks/build-image.yaml` builds images with rootless
[BuildKit](https://github.com/moby/buildkit) (`moby/buildkit:<version>-rootless`, run with
`buildctl-daemonless.sh`), one daemon per TaskRun. See
[ADR-0026](adr/0026-buildkit-image-builds.md) for why it replaced kaniko on 2026-10-17.

## What the build step needs

| Requirement | Why |
|---|---|
| Pod Security `baseline` or `privileged` on the app's `-cicd` namespace (Glidepath sets and enforces `baseline`) | rootlesskit creates a user namespace; `restricted` (`allowPrivilegeEscalation: false`, RuntimeDefault seccomp) blocks that |
| kubelet `seccompDefault: false`, or a Localhost seccomp profile on the step | RuntimeDefault seccomp blocks the user-namespace setup |
| binfmt_misc handlers on the build node for every foreign platform | RUN steps in a target-platform stage run emulated; kiac-dev registers them with `00-bootstrap/binfmt/` (gitops-cluster-dev) |
| A `registry-credentials` dockerconfigjson Secret in the namespace | Push credentials, mounted at `/home/user/.docker` |

The step runs as UID 1000, adds no capabilities, mounts nothing from the host and sets no
seccomp or AppArmor profile. That is the shape that passes `baseline`: setting either profile
to `Unconfined`, as BuildKit's own Kubernetes examples do, is rejected by `baseline`.

Every `-cicd` namespace carries `pod-security.kubernetes.io/enforce: baseline` (with `warn`
and `audit` at the same level), applied by the tenant-onboarding ApplicationSet's
`managedNamespaceMetadata`. A Task change that adds a privileged field fails at pod
creation, not silently. See [security.md](security.md#build-isolation).

## Cross-architecture builds

The build node is arm64. For an amd64 image, a stage written `FROM --platform=$BUILDPLATFORM`
runs natively and cross-compiles (`GOARCH=$TARGETARCH`, a Java jar, a pure-JS bundle); every
other stage's RUN steps go through QEMU. Emulation is slow and crashes the Go compiler
([known-gaps.md](known-gaps.md) #37), so Containerfiles that compile should compile in a
`$BUILDPLATFORM` stage. See
[cicd-yaml-reference.md](../user/cicd-yaml-reference.md#multi-architecture-images-buildplatform).

## Cache

Each app gets a second GHCR package, `<image-repo>-buildcache`, holding one OCI cache
manifest (`latest`, `mode=max`). The first build logs `failed to configure registry cache
importer ... not found` and carries on. Deleting the package only costs one cold build.

## Checking a cluster

To see whether a build pod would be admitted at a given level, label a scratch namespace and
dry-run the step's pod spec against it:

```bash
kubectl create ns pss-check
kubectl label ns pss-check pod-security.kubernetes.io/enforce=baseline
kubectl create --dry-run=server -n pss-check -f build-pod.yaml
```

Admission is not enough: a `restricted`-shaped pod is admitted and then fails inside
rootlesskit. Run a real build once on a new cluster before onboarding apps.
