# Build context is platform/dora-exporter/:
#   docker build -f cmd/dora-exporter/Dockerfile -t ghcr.io/jfillman/glidepath-dora-exporter:latest .
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/dora-exporter ./cmd/dora-exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dora-exporter /dora-exporter
USER nonroot:nonroot
ENTRYPOINT ["/dora-exporter"]
