# Build context is platform/broker/:
#   docker build -f cmd/token-review-interceptor/Dockerfile -t ghcr.io/jfillman/glidepath-token-review-interceptor:latest .
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/token-review-interceptor ./cmd/token-review-interceptor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/token-review-interceptor /token-review-interceptor
USER nonroot:nonroot
ENTRYPOINT ["/token-review-interceptor"]
