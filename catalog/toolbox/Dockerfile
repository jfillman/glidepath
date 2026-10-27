# catalog/toolbox/Dockerfile
#
# Shared base image for every catalog Task step (all step logic is bash) - common
# tools/libs are versioned and patched in exactly one place instead of duplicated per
# Task. Tool versions are pinned explicit build args, not "latest" - a supply-chain-
# sensitive shared image.
#
# Architecture-aware: built and run on an arm64 node, so every download resolves via
# `uname -m` at build time rather than hardcoding amd64 - a hardcoded amd64 binary
# would fail at runtime with "exec format error", not at build time.
#
# Notable tool choices:
#   - yajsv: no linux/arm64 release binary exists, so built from source in a Go stage.
#   - gitsign: verifies a fetched app-repo commit's signature (never signs from inside
#     a Task - signing is a developer-machine, interactive-OIDC action). See
#     docs/admin/commit-signing.md.
#   - ec (Conforma CLI): the GitHub-release binary, not the quay.io container image -
#     that image is amd64-only and crashes under arm64 emulation. See
#     docs/admin/provenance-policy.md.
#   - cosign: does the actual signature/attestation crypto verification - its
#     --ca-roots flag verifies against a local file directly, no TUF trust
#     distribution needed (which ec's own built-in verification would require).
#   - libxml2-utils (xmllint): pulls a Maven app's <version> from pom.xml for untagged
#     builds (package.json's version needs only jq, already present).
#   - gpgv + /etc/glidepath/github-web-flow.gpg: the provenance gate's GitHub-merge path
#     (docs/admin/commit-signing.md) verifies a commit GitHub's merge button signed
#     against GitHub's web-flow OpenPGP keys only. The key is checked in
#     (catalog/toolbox/github-web-flow.asc, from https://github.com/web-flow.gpg) and its
#     fingerprints are asserted below, so a changed file fails the build; the gate never
#     fetches a key at verify time. GitHub rotating its key means: update the .asc, the
#     assert here and the pinned list in verify-commit-signature.yaml, then rebuild.
#   - oasdiff: the api-compat release check's OpenAPI diff (breaking-change rules, JSON output). Pinned with the
#     release's own sha256 per architecture, checked at build time.
#   - kyverno (CLI): the policy-validation release check (ADR-0028) evaluates rendered manifests offline against a
#     cluster's admission policies. Copied from Kyverno's own multi-arch CLI image, pinned by index digest (the
#     build platform picks the matching static binary), at the version the clusters run (1.19.0).
#   - oras: pushes build.artifacts' non-container outputs (python-wheel/go-binary) as
#     real OCI artifacts to the same registry/credentials the image itself uses - see
#     build-artifact.yaml. Respects DOCKER_CONFIG the same way crane does.
#
# Build context MUST be the repo root (paths below are repo-root-relative):
#   docker build -f catalog/toolbox/Dockerfile -t ghcr.io/<owner>/glidepath-toolbox:latest .
#
# schemas/cicd.schema.json is baked in rather than mounted from a ConfigMap, so a
# schema change requires an image rebuild + version bump, not a silent live update -
# deliberate, see docs/user/cicd-yaml-reference.md.

FROM ghcr.io/kyverno/kyverno-cli:v1.19.0@sha256:f40718123e3eea8dc36960023e2af7a980bcc76dec24842ad8705c82f4c9172e AS kyverno-cli

FROM golang:1.22-alpine AS yajsv-build
ARG YAJSV_VERSION=1.4.1
RUN CGO_ENABLED=0 go install "github.com/neilpa/yajsv@v${YAJSV_VERSION}"

# GitHub's web-flow keys, dearmored into a gpgv keyring in a throwaway stage (full gnupg is
# needed only here; the final image gets just gpgv). The assert lists every primary
# fingerprint the keyring may hold, so an extra or swapped key fails the build too.
FROM alpine:3.20 AS web-flow-keyring
RUN apk add --no-cache gnupg
COPY catalog/toolbox/github-web-flow.asc /tmp/github-web-flow.asc
RUN set -eu; \
    gpg --dearmor < /tmp/github-web-flow.asc > /github-web-flow.gpg; \
    got="$(gpg --show-keys --with-colons /tmp/github-web-flow.asc | awk -F: '/^pub/{p=1;next} p&&/^fpr/{print $10;p=0}' | sort | paste -sd' ' -)"; \
    want="5DE3E0509C47EA3CF04A42D34AEE18F83AFDEB23 968479A1AFF927E37D1A566BB5690EEEBB952194"; \
    if [ "${got}" != "${want}" ]; then echo "web-flow key fingerprints '${got}' != pinned '${want}'" >&2; exit 1; fi

FROM alpine:3.20

ARG OTEL_CLI_VERSION=0.4.5
ARG YQ_VERSION=4.44.3
ARG KUBECTL_VERSION=1.30.4
ARG GITSIGN_VERSION=0.17.0
ARG EC_VERSION=0.9.88
ARG COSIGN_VERSION=3.1.2
ARG TRIVY_VERSION=0.73.0
ARG TESTKUBE_VERSION=2.13.1
ARG ORAS_VERSION=1.2.0
ARG OASDIFF_VERSION=1.33.0
ARG OASDIFF_SHA256_AMD64=43a4e328e2d13ba1552d760aa68d2485c75c5621f309f6ff64ae895188345247
ARG OASDIFF_SHA256_ARM64=4ae3c362d6074d919aada2dea82d0ee84366591600384455d0bdc658ddf8f7ae

RUN apk add --no-cache bash curl jq git openssl ca-certificates util-linux coreutils libxml2-utils gpgv

RUN set -euo pipefail; \
    case "$(uname -m)" in \
      x86_64) ARCH=amd64 ;; \
      aarch64|arm64) ARCH=arm64 ;; \
      *) echo "unsupported build architecture: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/otel-cli.tar.gz \
      "https://github.com/equinix-labs/otel-cli/releases/download/v${OTEL_CLI_VERSION}/otel-cli_${OTEL_CLI_VERSION}_linux_${ARCH}.tar.gz" \
    && tar -xzf /tmp/otel-cli.tar.gz -C /usr/local/bin otel-cli \
    && chmod +x /usr/local/bin/otel-cli \
    && rm /tmp/otel-cli.tar.gz \
    && curl -fsSL -o /usr/local/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${ARCH}" \
    && chmod +x /usr/local/bin/yq \
    && curl -fsSL -o /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && curl -fsSL -o /usr/local/bin/gitsign \
      "https://github.com/sigstore/gitsign/releases/download/v${GITSIGN_VERSION}/gitsign_${GITSIGN_VERSION}_linux_${ARCH}" \
    && chmod +x /usr/local/bin/gitsign \
    && curl -fsSL -o /usr/local/bin/ec \
      "https://github.com/conforma/cli/releases/download/v${EC_VERSION}/ec_linux_${ARCH}" \
    && chmod +x /usr/local/bin/ec \
    && curl -fsSL -o /usr/local/bin/cosign \
      "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-${ARCH}" \
    && chmod +x /usr/local/bin/cosign \
    && case "$(uname -m)" in x86_64) TRIVY_ARCH=64bit ;; aarch64|arm64) TRIVY_ARCH=ARM64 ;; esac \
    && curl -fsSL -o /tmp/trivy.tar.gz \
      "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${TRIVY_ARCH}.tar.gz" \
    && tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy \
    && chmod +x /usr/local/bin/trivy \
    && rm /tmp/trivy.tar.gz \
    && case "$(uname -m)" in x86_64) TK_ARCH=x86_64 ;; aarch64|arm64) TK_ARCH=arm64 ;; esac \
    && curl -fsSL -o /tmp/testkube.tar.gz \
      "https://github.com/kubeshop/testkube/releases/download/${TESTKUBE_VERSION}/testkube_${TESTKUBE_VERSION}_Linux_${TK_ARCH}.tar.gz" \
    && tar -xzf /tmp/testkube.tar.gz -C /tmp kubectl-testkube \
    && mv /tmp/kubectl-testkube /usr/local/bin/testkube \
    && chmod +x /usr/local/bin/testkube \
    && rm /tmp/testkube.tar.gz \
    && curl -fsSL -o /tmp/oras.tar.gz \
      "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_linux_${ARCH}.tar.gz" \
    && tar -xzf /tmp/oras.tar.gz -C /usr/local/bin oras \
    && chmod +x /usr/local/bin/oras \
    && rm /tmp/oras.tar.gz \
    && case "${ARCH}" in amd64) OASDIFF_SHA256="${OASDIFF_SHA256_AMD64}" ;; arm64) OASDIFF_SHA256="${OASDIFF_SHA256_ARM64}" ;; esac \
    && curl -fsSL -o /tmp/oasdiff.tar.gz \
      "https://github.com/oasdiff/oasdiff/releases/download/v${OASDIFF_VERSION}/oasdiff_${OASDIFF_VERSION}_linux_${ARCH}.tar.gz" \
    && echo "${OASDIFF_SHA256}  /tmp/oasdiff.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/oasdiff.tar.gz -C /usr/local/bin oasdiff \
    && chmod +x /usr/local/bin/oasdiff \
    && rm /tmp/oasdiff.tar.gz

COPY --from=yajsv-build /go/bin/yajsv /usr/local/bin/yajsv
COPY --from=kyverno-cli /ko-app/kubectl-kyverno /usr/local/bin/kyverno

COPY catalog/lib/otel.sh /opt/platform/lib/otel.sh
COPY catalog/lib/chain-slug.sh /opt/platform/lib/chain-slug.sh
COPY catalog/lib/cdevents.sh /opt/platform/lib/cdevents.sh
COPY catalog/lib/github-app.sh /opt/platform/lib/github-app.sh
COPY catalog/lib/retry.sh /opt/platform/lib/retry.sh
COPY catalog/lib/build-agents.env /opt/platform/lib/build-agents.env
COPY schemas/cicd.schema.json /opt/platform/schemas/cicd.schema.json
COPY --from=web-flow-keyring /github-web-flow.gpg /etc/glidepath/github-web-flow.gpg

# Onboarding boilerplate (catalog/tasks/deliver-onboarding-files.yaml,
# docs/admin/onboarding-mechanics.md) used to be baked in here too - moved to a
# ConfigMap instead (charts/glidepath-app/templates/configmaps/onboarding-
# templates.yaml, one per Application), the opposite choice from schema.json below.
# Baking it in meant a template edit silently depended on an image rebuild+push AND
# :latest/IfNotPresent re-pulling on every node - a real staleness class this platform
# has hit before. A ConfigMap ties an edit to a real `helm upgrade` of the app chart
# instead. A shared catalog-chart ConfigMap was tried first and dropped - a Pod can
# only volume-mount a ConfigMap from its own namespace, so that shape would have needed
# cross-namespace RBAC for no real benefit.

# Non-root by default - every catalog Task should run under Pod Security Standards
# "restricted" without needing per-Task overrides.
RUN adduser -D -u 65532 platform
USER 65532

ENV PLATFORM_LIB=/opt/platform/lib
