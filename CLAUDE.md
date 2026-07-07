# CLAUDE.md

## What this is

A drop-in replacement for Cloudflare's official `cloudflared` Docker image. It wraps the
upstream `cloudflared` binary with a small Go entrypoint (`cloudflared-wrapped`) that, before
exec'ing cloudflared, can: auto-create/adopt a named tunnel via the Cloudflare API, discover
ingress from Docker container labels, and reconcile DNS CNAMEs. Every feature is opt-in and
gated by its own input — with none set, the image behaves identically to the official one
(same nonroot uid `65532`, distroless base, no shell).

## Build / run / test

```bash
# Build the multi-arch-capable image (single arch locally)
docker build -t cfw-test .                        # uses CLOUDFLARED_VERSION build-arg (default in Dockerfile)

# Build the Go binary directly
go build -o /tmp/cloudflared-wrapped ./cmd/cloudflared-wrapped/

# Unit tests
go test ./...                                      # cmd/cloudflared-wrapped/docker_test.go
```

There is no `make`/task runner. Manual end-to-end testing of the tunnel-ensure paths (adopt /
restart-reuse / create) is documented in `TESTING.md` and requires real Cloudflare credentials
— it hits the live API and creates real tunnels/DNS records, so use throwaway names. Do not run
these against production tunnels or zones.

## Architecture

- **`Dockerfile`** — 3 stages: pulls `cloudflared` binary from the official image, builds the Go
  wrapper static binary, copies both into `gcr.io/distroless/static:nonroot`. ENTRYPOINT is
  `cloudflared-wrapped`.
- **`cmd/cloudflared-wrapped/main.go`** — entrypoint + Cloudflare API client (tunnel ensure, DNS
  sync). Flow:
  1. **Feature 0 — passthrough.** If args were passed, or `TUNNEL_TOKEN` is set, exec cloudflared
     untouched (`cloudflared --no-autoupdate …`) and skip all wrapper logic. This is what makes it
     a true drop-in; an explicit command/token always wins.
  2. **Tunnel identity.** If `TUNNEL_NAME`+`CF_API_TOKEN`+`CF_ACCOUNT_ID` set, `ensureTunnel`
     reuses on-disk `credentials.json`, else adopts an existing tunnel by name (secret rebuilt from
     the `/token` endpoint) or creates a new one with a generated secret. Otherwise the tunnel id
     comes from `config.yml`'s `tunnel:` field.
  3. **Feature 1 — label discovery** (`docker.go`). Gated only by the Docker socket being mounted.
  4. **Feature 2 — DNS sync.** Gated by `CF_API_TOKEN`+`CF_ZONE_ID`. `CF_ZONE_ID` accepts a
     single zone ID or a comma-separated list. Each hostname's apex is matched to a zone via
     `GET /zones/<id>` (cached per run in `zoneNameCache`); unmatched hostnames are logged as
     skipped. Reconciles CNAMEs to `<uuid>.cfargotunnel.com`.
  5. **Exec cloudflared** via `syscall.Exec` so cloudflared becomes PID 1 and gets signals
     directly. When the wrapper resolved the id, it passes `run <uuid>` on the CLI, so `config.yml`
     needs no `tunnel:` field.
- **`cmd/cloudflared-wrapped/docker.go`** — talks to the Docker Engine API over the Unix socket
  with a hand-rolled `http.Client` (no Docker SDK, keeps the binary tiny). Reads
  `cloudflare.io/hostname` labels, infers `http://<container-name>:<port>` (port from the single
  exposed TCP port unless `:port` is given in the label), and writes a merged config to
  `/tmp/config.yml` — the read-only mounted `config.yml` is never modified. A
  `cloudflare.io/reverseproxy` label routes a hostname through a reverse proxy instead of the
  container (skipping port inference; only applied when set); an `https://` target also emits a
  per-rule `originRequest` setting `originServerName` + `httpHostHeader` to the public hostname, so
  routing through a name-addressed HTTPS reverse proxy (e.g. `caddy:443`) matches the right site and
  cert. This is how public tunnel traffic is funneled through the same proxy as LAN traffic (shared
  `forward_auth`/TLS). Note: it does not fall back — only set it for a hostname the proxy serves.

Tunnel-ensure and DNS-sync failures are warnings, not fatal, once a valid `credentials.json`
exists (don't block the tunnel from starting). The only hard exit is a failed `ensureTunnel`.

## Conventions

- **Sync mode** env var is `MODE` (`incremental` default, or `complete`). The README's
  "Removing a service" example writes `CF_SYNC_MODE` — that is a doc error; the code reads `MODE`.
- `incremental` only creates/repoints CNAMEs; `complete` also deletes CNAMEs pointing at this
  tunnel that aren't in the desired set. Guardrail: if the socket is mounted but unreadable,
  `complete` is downgraded to `incremental` for that run to avoid deleting against an incomplete
  desired set.
- DNS records are always created as proxied CNAMEs, TTL auto (`1`).
- `go.mod` module path is `github.com/Miista/cloudflared-wrapper`; published image is
  `ghcr.io/miista/cloudflared-wrapper`.

## Release / tagging scheme

`.github/workflows/build.yml` runs daily (and on push to `main`/`dev`). On a schedule/dispatch it
rebuilds only when upstream cloudflared has a new release (compared against `.version`), then
commits the bumped `.version`. Every build on `main` pushes three tags:

| Tag | Example | Mutable? | Use |
|-----|---------|----------|-----|
| `<version>-g<sha>` | `2026.6.1-ga1b2c3d` | **No** — never reused | Pinning / reproducible deploys |
| `<version>` | `2026.6.1` | Yes — moves to newest build of that cloudflared version | Drop-in version tracking |
| `latest` | `latest` | Yes | Always-newest |

`dev` branch pushes only a `dev` tag.

**For reproducible deployments, pin to the immutable `<version>-g<sha>` tag plus its digest**
(the `-g<sha>` also identifies the exact wrapper commit):

```yaml
image: ghcr.io/miista/cloudflared-wrapper:2026.6.1-ga1b2c3d@sha256:...
```

The mutable `<version>` tag stays drop-in compatible but is rewritten if the wrapper is rebuilt
while cloudflared stays on the same version, so don't rely on it for reproducibility.
