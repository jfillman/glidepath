# ADR-0010: Kaniko for rootless image builds under PSS `restricted`

## Context

The platform's whole premise is vanilla Kubernetes with no distribution-specific
lock-in (see ADR-0001), which means no assuming a privileged build daemon, host
namespace access, or a cluster admin willing to grant the shared build identity
elevated privilege. Building container images normally needs exactly that
(Docker-in-Docker, or a privileged `buildah`/`buildctl` daemon) - a real conflict with
running under Pod Security Standards `restricted` (no privileged containers, no host
namespaces/mounts, non-root by default).

## Decision

`build-image` (the shared catalog Task every Application's `build` stage uses) uses
[kaniko](https://github.com/GoogleContainerTools/kaniko) exclusively, not
Docker-in-Docker and not a privileged `buildah`/`buildctl` daemon - a deliberate
"vanilla Kubernetes, portable" choice: no distribution-specific privilege escalation for
the shared build identity, on any cluster this platform runs on.

This was flagged as a Phase 0 validation item rather than assumed to just work: kaniko
under `restricted` is well-trodden, but confirming it against the actual target
cluster/CNI/storage class was treated as a prerequisite before onboarding real
application repos, not something to discover after a dozen apps already depend on it.
Rootless `buildah` is the documented fallback if kaniko doesn't fit a given cluster
(certain multi-stage Dockerfile features, storage-backend cache behavior) - evaluated
as less battle-tested for the "no shell privileges at all" constraint kaniko was built
around, but a real fallback, not a dead end.

## Consequences

- `build-image` is the one Task in the whole catalog with a deliberate, documented
  exception to "step code is bash" - kaniko's own binary drives the build, not a shell
  script wrapping a daemon call.
- Portable by construction: no cluster this platform runs on needs a privileged
  container runtime class, a host-mounted Docker socket, or a cluster-admin-granted
  exception just to let application teams build images.
- Kaniko's build cache and multi-stage Dockerfile support are less mature than a real
  Docker daemon's - accepted for the privilege trade-off, revisit via the `buildah`
  fallback if a real application's build genuinely can't work within kaniko's model.
