# Build context is the REPO ROOT (the image carries contract/glidepath-contract.json):
#   docker build -f glidepath/api/cmd/glidepath-api/Dockerfile -t ghcr.io/<owner>/glidepath-api:dev .
# Cross-compiles on the build host: the fleet runs arm64 and amd64 clusters, so CI pushes both (api-ci.yaml).
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY glidepath/api/go.mod glidepath/api/go.sum ./
RUN go mod download
COPY glidepath/api/ .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/glidepath-api ./cmd/glidepath-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/glidepath-api /glidepath-api
COPY contract/glidepath-contract.json /contract/glidepath-contract.json
USER nonroot:nonroot
ENTRYPOINT ["/glidepath-api"]
