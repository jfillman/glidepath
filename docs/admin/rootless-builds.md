# Rootless image builds under Pod Security Standards "restricted"

`charts/glidepath-catalog/templates/tasks/build-image.yaml` uses [kaniko](https://github.com/GoogleContainerTools/kaniko)
rather than Docker-in-Docker or a privileged `buildah`/`buildctl` daemon, specifically so
image builds work under PSS `restricted` (no privileged containers, no host
namespaces/mounts, non-root by default) - a deliberate "vanilla Kubernetes, portable"
decision: no distribution-specific privilege escalation for the shared build identity,
on any cluster this platform runs on.

This is flagged in the plan as a Phase 0 validation item, not assumed to just work:
kaniko under `restricted` is a well-trodden combination, but confirm it on your actual
target cluster/CNI/storage class **before** onboarding real application repos - a
build-tooling problem discovered after a dozen apps are already relying on the shared
`build-image` Task is a much more expensive fix than catching it in Phase 0.

If kaniko turns out not to fit (e.g. certain multi-stage Dockerfile features, build
cache behavior on your storage backend), `buildah` in rootless mode is the fallback
worth evaluating next - rootless mode support has matured, but it's less
battle-tested specifically for the "no shell privileges at all" constraint kaniko was
built around. Either way, this is the one Task in the whole
catalog with a deliberate, documented exception to "step code is bash" - see the note at
the top of `build-image.yaml`.
