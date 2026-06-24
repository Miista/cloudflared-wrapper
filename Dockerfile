ARG CLOUDFLARED_VERSION=2026.6.1

FROM cloudflare/cloudflared:${CLOUDFLARED_VERSION} AS cloudflared

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /cloudflared-wrapped ./cmd/cloudflared-wrapped/
RUN mkdir -p /creds

FROM gcr.io/distroless/static:nonroot
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY --from=builder /cloudflared-wrapped /usr/local/bin/cloudflared-wrapped
COPY --from=builder --chown=65532:65532 /creds /var/lib/cloudflared
ENTRYPOINT ["/usr/local/bin/cloudflared-wrapped"]
