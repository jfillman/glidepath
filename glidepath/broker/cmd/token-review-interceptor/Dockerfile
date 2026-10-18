# Build context is glidepath/broker/:
#   docker build -f cmd/token-review-interceptor/Dockerfile -t ghcr.io/<owner>/glidepath-token-review-interceptor:latest .
# Cross-compiles on the build host: the fleet runs arm64 and amd64 clusters, so CI pushes both (broker-image-ci.yaml).
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/token-review-interceptor ./cmd/token-review-interceptor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/token-review-interceptor /token-review-interceptor
USER nonroot:nonroot
ENTRYPOINT ["/token-review-interceptor"]
