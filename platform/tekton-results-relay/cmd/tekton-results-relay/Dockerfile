# Build context is platform/tekton-results-relay/:
#   docker build -f cmd/tekton-results-relay/Dockerfile -t ghcr.io/jfillman/platform-cicd-tekton-results-relay:latest .
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/tekton-results-relay ./cmd/tekton-results-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tekton-results-relay /tekton-results-relay
USER nonroot:nonroot
ENTRYPOINT ["/tekton-results-relay"]
