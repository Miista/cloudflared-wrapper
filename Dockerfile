ARG CLOUDFLARED_VERSION=2026.6.1
FROM cloudflare/cloudflared:${CLOUDFLARED_VERSION} AS cloudflared
FROM alpine:latest
RUN apk add --no-cache curl jq yq ca-certificates
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY sync.sh /sync.sh
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /sync.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
