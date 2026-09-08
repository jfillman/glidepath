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
#
# Build context MUST be the repo root (paths below are repo-root-relative):
#   docker build -f catalog/toolbox/Dockerfile -t ghcr.io/jfillman/platform-cicd-toolbox:latest .
#
# schemas/cicd.schema.json is baked in rather than mounted from a ConfigMap, so a
# schema change requires an image rebuild + version bump, not a silent live update -
# deliberate, see docs/user/cicd-yaml-reference.md.

FROM golang:1.22-alpine AS yajsv-build
ARG YAJSV_VERSION=1.4.1
RUN CGO_ENABLED=0 go install "github.com/neilpa/yajsv@v${YAJSV_VERSION}"

FROM alpine:3.20

ARG OTEL_CLI_VERSION=0.4.5
ARG YQ_VERSION=4.44.3
ARG KUBECTL_VERSION=1.30.4
ARG GITSIGN_VERSION=0.17.0
ARG EC_VERSION=0.9.88
ARG COSIGN_VERSION=3.1.2
ARG TRIVY_VERSION=0.73.0
ARG TESTKUBE_VERSION=2.13.1

RUN apk add --no-cache bash curl jq git openssl ca-certificates util-linux coreutils libxml2-utils

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
    && rm /tmp/testkube.tar.gz

COPY --from=yajsv-build /go/bin/yajsv /usr/local/bin/yajsv

COPY catalog/lib/otel.sh /opt/platform/lib/otel.sh
COPY catalog/lib/cdevents.sh /opt/platform/lib/cdevents.sh
COPY catalog/lib/github-app.sh /opt/platform/lib/github-app.sh
COPY catalog/lib/retry.sh /opt/platform/lib/retry.sh
COPY catalog/lib/argocd-outcome-hook.sh /opt/platform/lib/argocd-outcome-hook.sh
COPY catalog/lib/build-agents.env /opt/platform/lib/build-agents.env
# Executed directly as a Job command (ArgoCD PostSync/SyncFail hook), unlike the other
# lib/*.sh files above, which are only ever `source`d - git's own executable bit should
# survive COPY, but set it explicitly rather than depend on that.
RUN chmod +x /opt/platform/lib/argocd-outcome-hook.sh
COPY schemas/cicd.schema.json /opt/platform/schemas/cicd.schema.json

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
