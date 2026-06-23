# cloudflared-wrapped

Drop-in cloudflared image that converges DNS records from `config.yml` on every start.

On container start, the wrapper reads every `hostname:` from the tunnel's ingress
config, ensures a matching Cloudflare CNAME exists via the API, then execs
`cloudflared tunnel run`. DNS sync failures log a warning but do not block the
tunnel from starting.

## Image

- Based on `gcr.io/distroless/static` — no shell, same security posture as the official image
- Two static binaries: `cloudflared` (copied from official) + `cloudflared-wrapped` (Go sync + entrypoint)
- ~70 MB vs ~103 MB official

## Modes

- `MODE=incremental` (default): create missing CNAMEs, leave others alone
- `MODE=complete`: also delete CNAMEs that point at this tunnel but are not in config

## Required env

- `CF_API_TOKEN` — Cloudflare API token with `Zone:DNS:Edit` on the target zone
- `CF_ZONE_ID` — Cloudflare zone ID

## Optional env

- `MODE` — `incremental` (default) or `complete`
- `CONFIG_PATH` — path to config.yml (default: `/etc/cloudflared/config.yml`)

## Required mounts

- `/etc/cloudflared/config.yml` — tunnel config with `tunnel:` UUID and `ingress:` list
- `/etc/cloudflared/credentials.json` — tunnel credentials

## Compose snippet

```yaml
  cloudflared:
    image: ghcr.io/sorenguldmund/cloudflared-wrapped:latest
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
2. `docker compose restart cloudflared`
3. For a pruning deploy: `CF_SYNC_MODE=complete docker compose up -d --force-recreate cloudflared`
