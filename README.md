# cloudflared-wrapped

Cloudflared tunnel that converges DNS records from `config.yml` on every start.

## How it works

On container start, the entrypoint runs `sync.sh` to ensure a Cloudflare CNAME
exists for every `hostname:` in the tunnel's `config.yml` ingress block, then
execs `cloudflared tunnel ... run`. DNS sync failures log a warning but do not
block the tunnel from starting.

## Modes

- `MODE=incremental` (default): add missing CNAMEs, leave others alone
- `MODE=complete`: also delete CNAMEs that point at this tunnel but are not in `config.yml`

## Required env

- `CF_API_TOKEN` — Cloudflare API token with `Zone:DNS:Edit` on the target zone
- `CF_ZONE_ID` — Cloudflare zone ID

## Required mounts

- `/etc/cloudflared/config.yml` — tunnel config (must contain `tunnel:` UUID and `ingress:` list)
- `/etc/cloudflared/credentials.json` — tunnel credentials

## Compose snippet

```yaml
  cloudflared:
    build: ./cloudflared-wrapped
    container_name: cloudflared
    restart: unless-stopped
    environment:
      - CF_API_TOKEN=${CF_API_TOKEN}
      - CF_ZONE_ID=${CF_ZONE_ID}
      - MODE=${CF_SYNC_MODE:-incremental}
    volumes:
      - ./cloudflared/data:/etc/cloudflared:ro
    extra_hosts:
      - host.docker.internal:host-gateway
```

## Deploy workflow

1. Edit `cloudflared/data/config.yml` — add/remove ingress entries
2. `docker compose up -d --build cloudflared` (or `restart` if no image rebuild needed)
3. For a pruning deploy: `CF_SYNC_MODE=complete docker compose up -d --force-recreate cloudflared`
