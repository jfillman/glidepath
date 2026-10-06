# Build context is platform/broker/:
#   docker build -f cmd/glidepath-relay/Dockerfile -t ghcr.io/jfillman/glidepath-relay:latest .
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/glidepath-relay ./cmd/glidepath-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/glidepath-relay /glidepath-relay
USER nonroot:nonroot
ENTRYPOINT ["/glidepath-relay"]
