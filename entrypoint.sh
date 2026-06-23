#!/bin/sh
set -eu

echo "[entrypoint] starting dns sync (mode=${MODE:-incremental})"
if /sync.sh; then
  echo "[entrypoint] dns sync ok"
else
  echo "[entrypoint] WARN: dns sync failed, continuing to start tunnel"
fi

echo "[entrypoint] launching cloudflared tunnel"
exec cloudflared tunnel --config /etc/cloudflared/config.yml run
