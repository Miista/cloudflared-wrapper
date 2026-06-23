# cloudflared-wrapped

A drop-in replacement for the official [cloudflared](https://github.com/cloudflare/cloudflared) Docker image that automatically manages Cloudflare DNS records from your tunnel's `config.yml`.

With the official image, adding a new service means editing `config.yml` **and** manually creating a DNS record in the Cloudflare dashboard (or via CLI). This image removes that second step — on every container start it reads your ingress hostnames and ensures matching CNAME records exist via the Cloudflare API. Then it execs `cloudflared tunnel run` as normal.

Without DNS credentials set, it behaves identically to the official image.

## How it works

1. On start, the wrapper parses `config.yml` and extracts every `hostname:` from the ingress rules
2. It queries the Cloudflare API for existing CNAME records pointing at this tunnel
3. Missing records are created, existing ones are left alone
4. In `complete` mode, records pointing at this tunnel that are **not** in config are deleted
5. The wrapper then replaces itself with `cloudflared` via exec — cloudflared becomes PID 1 and receives signals directly

DNS sync failures are logged as warnings but never prevent the tunnel from starting.

## Image

- Based on `gcr.io/distroless/static` — no shell, no package manager, same security posture as the official cloudflared image
- Contains two static binaries: `cloudflared` (copied from official) and `cloudflared-wrapped` (the Go sync + entrypoint)
- ~70 MB vs ~103 MB for the official image

## Quick start

### 1. Create a tunnel

Create a tunnel in the [Zero Trust dashboard](https://one.dash.cloudflare.com/) or via CLI. You need:
- A `credentials.json` (tunnel ID + secret)
- The tunnel UUID

### 2. Write config.yml

```yaml
tunnel: <your-tunnel-uuid>
credentials-file: /etc/cloudflared/credentials.json

ingress:
  - hostname: app.example.com
    service: http://host.docker.internal:8080
  - hostname: grafana.example.com
    service: http://host.docker.internal:3000
  - service: http_status:404
```

The catch-all `http_status:404` at the end is required by cloudflared.

### 3. Create a Cloudflare API token

Go to [API Tokens](https://dash.cloudflare.com/profile/api-tokens) and create a token with:
- **Permissions**: Zone > DNS > Edit
- **Zone resources**: the zone(s) your hostnames are in
- **Expiry**: optional (can be set to never expire)

### 4. Run it

```yaml
# docker-compose.yml
services:
  cloudflared:
    image: ghcr.io/miista/cloudflared-wrapper:latest
    container_name: cloudflared
    restart: unless-stopped
    environment:
      - CF_API_TOKEN=${CF_API_TOKEN}
      - CF_ZONE_ID=${CF_ZONE_ID}
    volumes:
      - ./cloudflared/data:/etc/cloudflared:ro
    extra_hosts:
      - host.docker.internal:host-gateway
```

```bash
docker compose up -d cloudflared
```

On first start you'll see:
```
[sync] tunnel=abc123 mode=incremental hostnames=2
  create  app.example.com
  create  grafana.example.com
[sync] summary: ok=0 created=2 deleted=0 errors=0
[sync] dns sync ok in 340ms
[entrypoint] launching cloudflared tunnel
```

On subsequent starts, existing records are detected and skipped:
```
[sync] tunnel=abc123 mode=incremental hostnames=2
  ok      app.example.com
  ok      grafana.example.com
[sync] summary: ok=2 created=0 deleted=0 errors=0
[sync] dns sync ok in 120ms
[entrypoint] launching cloudflared tunnel
```

## Adding a new service

1. Add an ingress entry to `config.yml`
2. `docker compose restart cloudflared`

That's it. The DNS record is created automatically.

## Removing a service

In the default `incremental` mode, removing a hostname from `config.yml` does **not** delete the DNS record — it's left in place as a safe default.

To also clean up stale DNS records, use `complete` mode:

```bash
CF_SYNC_MODE=complete docker compose up -d --force-recreate cloudflared
```

This deletes any CNAME pointing at your tunnel that isn't in config. Think of it as the difference between an incremental and a full deployment.

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `CF_API_TOKEN` | No | — | Cloudflare API token with Zone:DNS:Edit. If unset, DNS sync is skipped. |
| `CF_ZONE_ID` | No | — | Cloudflare zone ID. If unset, DNS sync is skipped. |
| `MODE` | No | `incremental` | `incremental` or `complete` |
| `CONFIG_PATH` | No | `/etc/cloudflared/config.yml` | Path to the tunnel config file |

Without `CF_API_TOKEN` and `CF_ZONE_ID`, the image behaves identically to the official cloudflared image.

## Mounts

| Path | Description |
|---|---|
| `/etc/cloudflared/config.yml` | Tunnel config with `tunnel:` UUID and `ingress:` rules |
| `/etc/cloudflared/credentials.json` | Tunnel credentials (generated during tunnel creation) |

Mount as read-only (`:ro`) — the container never writes to these files.

## Automated builds

A GitHub Actions workflow checks for new cloudflared releases daily. When a new version is detected, the image is rebuilt and pushed with both a version tag and `latest`.

## License

Apache 2.0 — same as cloudflared.
