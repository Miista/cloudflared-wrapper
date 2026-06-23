#!/bin/sh
# Sync Cloudflare DNS CNAME records to match ingress hostnames in config.yml.
#
# Env:
#   CF_API_TOKEN  Cloudflare API token (Zone:DNS:Edit on the target zone)
#   CF_ZONE_ID    Cloudflare zone ID
#   MODE          incremental (default) or complete (prunes records not in config)
set -eu

: "${CF_API_TOKEN:?missing}"
: "${CF_ZONE_ID:?missing}"
: "${MODE:=incremental}"

CONFIG=/etc/cloudflared/config.yml
TUNNEL_ID=$(yq '.tunnel' "$CONFIG")
TARGET="${TUNNEL_ID}.cfargotunnel.com"
API="https://api.cloudflare.com/client/v4/zones/${CF_ZONE_ID}/dns_records"
AUTH="Authorization: Bearer ${CF_API_TOKEN}"

desired=$(yq '.ingress[].hostname | select(. != null)' "$CONFIG" | sort -u)
existing=$(curl -fsS -H "$AUTH" "${API}?type=CNAME&content=${TARGET}&per_page=500" \
  | jq -r '.result[] | "\(.name)\t\(.id)"')

echo "$desired" | while IFS= read -r host; do
  [ -z "$host" ] && continue
  id=$(echo "$existing" | awk -F'\t' -v h="$host" '$1==h{print $2}')
  if [ -n "$id" ]; then
    echo "  ok      $host"
  else
    echo "  create  $host"
    curl -fsS -X POST -H "$AUTH" -H "Content-Type: application/json" "$API" \
      -d "$(jq -n --arg n "$host" --arg c "$TARGET" \
            '{type:"CNAME",name:$n,content:$c,proxied:true,ttl:1}')" >/dev/null
  fi
done

if [ "$MODE" = "complete" ]; then
  echo "$existing" | while IFS=$'\t' read -r host id; do
    [ -z "$host" ] && continue
    if ! echo "$desired" | grep -qx "$host"; then
      echo "  delete  $host"
      curl -fsS -X DELETE -H "$AUTH" "${API}/${id}" >/dev/null
    fi
  done
fi

echo "dns sync done (mode=$MODE)"
