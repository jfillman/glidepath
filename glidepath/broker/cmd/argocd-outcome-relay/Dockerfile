# Build context is platform/broker/:
#   docker build -f cmd/argocd-outcome-relay/Dockerfile -t ghcr.io/jfillman/glidepath-argocd-outcome-relay:latest .
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/argocd-outcome-relay ./cmd/argocd-outcome-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/argocd-outcome-relay /argocd-outcome-relay
USER nonroot:nonroot
ENTRYPOINT ["/argocd-outcome-relay"]
