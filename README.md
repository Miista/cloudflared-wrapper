# cloudflared-wrapped

A drop-in replacement for the official [cloudflared](https://github.com/cloudflare/cloudflared) Docker image that:

- Auto-creates (or adopts) the tunnel itself from a name — no manual `cloudflared tunnel create`, no `cert.pem` to mount.
- Auto-manages Cloudflare DNS records from your tunnel's `config.yml`. CNAMEs are created if missing and repointed if they exist but target the wrong tunnel.

With the official image, standing up a new tunnel means running `cloudflared tunnel login`, `cloudflared tunnel create`, copying out a `credentials.json`, and then maintaining DNS records by hand. This image removes all of that — set an API token and a tunnel name and the rest is done on container start.

Without any Cloudflare credentials set, it behaves identically to the official image.

## How it works

On start, with credentials configured, the wrapper:

1. **Ensures the tunnel exists.** If `credentials.json` is on disk it's reused. Otherwise the wrapper looks the tunnel up by name via the Cloudflare API; an existing tunnel is adopted (its secret reconstructed from the `/token` endpoint), a missing one is created with a freshly generated secret. The resulting `credentials.json` is written to the data dir so subsequent restarts skip the API entirely.
2. **Syncs DNS.** Parses `config.yml`, extracts every `hostname:` from ingress, and reconciles the corresponding CNAMEs in your zone:
   - Missing → created.
   - Already pointing at this tunnel → left alone.
   - Pointing at something else → repointed (logged as `Update  host from X to Y`).
   - In `complete` mode, CNAMEs pointing at this tunnel that are **not** in config are also deleted.
3. **Execs cloudflared.** Replaces itself with `cloudflared tunnel run <uuid>` — cloudflared becomes PID 1 and receives signals directly. The tunnel UUID is passed on the CLI, so `config.yml` does not need a `tunnel:` field.

Tunnel-ensure and DNS-sync failures are logged as warnings but never prevent the tunnel from starting (when the data dir already holds a valid `credentials.json`).

## Image

- Based on `gcr.io/distroless/static` — no shell, no package manager, same security posture as the official cloudflared image
- Contains two static binaries: `cloudflared` (copied from official) and `cloudflared-wrapped` (the Go sync + entrypoint)
- ~70 MB vs ~103 MB for the official image

## Quick start

### 1. Gather your IDs

From the Cloudflare dashboard you need:

- **Account ID** — right sidebar of any zone, or Account Home.
- **Zone ID** — right sidebar of the zone containing your ingress hostnames.

### 2. Create an API token

Go to [API Tokens](https://dash.cloudflare.com/profile/api-tokens) and create a token with:

- **Zone → DNS → Edit** on the zone(s) your hostnames live in
- **Account → Cloudflare Tunnel → Edit** on your account

The Tunnel scope is only needed for the auto-create/adopt path. If you'd rather manage the tunnel yourself, omit it (see [Manual tunnel](#manual-tunnel) below).

### 3. Write config.yml

```yaml
ingress:
  - hostname: app.example.com
    service: http://host.docker.internal:8080
  - hostname: grafana.example.com
    service: http://host.docker.internal:3000
  - service: http_status:404
```

No `tunnel:` or `credentials-file:` — the wrapper supplies both at runtime. The catch-all `http_status:404` at the end is required by cloudflared.

### 4. Run it

```yaml
# docker-compose.yml
services:
  cloudflared:
    image: ghcr.io/miista/cloudflared-wrapper:latest
    container_name: cloudflared
    restart: unless-stopped
    environment:
      - TUNNEL_NAME=my-tunnel
      - CF_ACCOUNT_ID=${CF_ACCOUNT_ID}
      - CF_ZONE_ID=${CF_ZONE_ID}
      - CF_API_TOKEN=${CF_API_TOKEN}
    volumes:
      - ./cloudflared:/etc/cloudflared
    extra_hosts:
      - host.docker.internal:host-gateway
```

Note the volume is **read-write** — the wrapper writes `credentials.json` here on the first start. The container runs as uid `65532` (the distroless `nonroot` user), so make sure that uid can write to your host directory:

```bash
sudo chown -R 65532:65532 ./cloudflared
```

Then start it:

```bash
docker compose up -d cloudflared
```

On first start (tunnel doesn't exist yet):
```
[tunnel] Creating new tunnel name=my-tunnel
[tunnel] Created tunnel id=abc123...
[sync] tunnel=abc123... mode=incremental hostnames=2
  Create  app.example.com
  Create  grafana.example.com
[sync] Summary: ok=0 created=2 updated=0 deleted=0 errors=0
[sync] DNS sync OK in 340ms
[entrypoint] Launching cloudflared tunnel
```

On subsequent starts, `credentials.json` is reused and existing records are left alone:
```
[tunnel] Using existing credentials.json tunnel=abc123...
[sync] tunnel=abc123... mode=incremental hostnames=2
  OK      app.example.com
  OK      grafana.example.com
[sync] Summary: ok=2 created=0 updated=0 deleted=0 errors=0
[sync] DNS sync OK in 120ms
[entrypoint] Launching cloudflared tunnel
```

If `credentials.json` is missing but a tunnel with that name already exists in Cloudflare (e.g. fresh volume, same account), the wrapper adopts it:
```
[tunnel] Adopting existing tunnel name=my-tunnel id=abc123...
```

If a CNAME for one of your hostnames exists but points at a different tunnel, it's repointed:
```
  Update  app.example.com from old-uuid.cfargotunnel.com to abc123.cfargotunnel.com
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

## Manual tunnel

If you'd rather create and own the tunnel yourself (e.g. you don't want to grant the `Cloudflare Tunnel:Edit` scope, or you manage tunnels via Terraform), omit `TUNNEL_NAME` and place a pre-existing `credentials.json` in the data dir. Add `tunnel:` and `credentials-file:` to your `config.yml`:

```yaml
tunnel: <your-tunnel-uuid>
credentials-file: /etc/cloudflared/credentials.json
ingress:
  - hostname: app.example.com
    service: http://host.docker.internal:8080
  - service: http_status:404
```

DNS sync still works as long as `CF_API_TOKEN` and `CF_ZONE_ID` are set. The volume can be read-only in this mode.

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `TUNNEL_NAME` | No | — | If set (with `CF_API_TOKEN` and `CF_ACCOUNT_ID`), the wrapper ensures a tunnel with this name exists and writes `credentials.json`. |
| `CF_API_TOKEN` | No | — | Cloudflare API token. Needs Zone:DNS:Edit for DNS sync; add Account:Cloudflare Tunnel:Edit for auto tunnel ensure. |
| `CF_ACCOUNT_ID` | No | — | Cloudflare account ID. Required when `TUNNEL_NAME` is set. |
| `CF_ZONE_ID` | No | — | Cloudflare zone ID. If unset, DNS sync is skipped. |
| `MODE` | No | `incremental` | `incremental` or `complete` |
| `CONFIG_PATH` | No | `/etc/cloudflared/config.yml` | Path to the tunnel config file |
| `CREDENTIALS_DIR` | No | `/etc/cloudflared` | Directory where `credentials.json` is read/written |

Without any Cloudflare credentials, the image behaves identically to the official cloudflared image.

## Mounts

| Path | Description |
|---|---|
| `/etc/cloudflared/config.yml` | Tunnel config — `ingress:` rules. With manual tunnel, also `tunnel:` + `credentials-file:`. |
| `/etc/cloudflared/credentials.json` | Tunnel credentials. Written by the wrapper in auto mode; supplied by you in manual mode. |

In auto mode, mount the directory **read-write** so the wrapper can persist `credentials.json`, and ensure it's writable by uid `65532` (`sudo chown -R 65532:65532 <dir>`). In manual mode you can mount it read-only.

If a bind-mount chown is awkward (e.g. the host dir is shared with other tooling), use a Docker named volume instead — Docker creates it with the container's uid by default, so no host-side chown is needed.

## Automated builds

A GitHub Actions workflow checks for new cloudflared releases daily. When a new version is detected, the image is rebuilt and pushed with both a version tag and `latest`.

## License

Apache 2.0 — same as cloudflared.
