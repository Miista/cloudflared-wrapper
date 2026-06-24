# Testing the auto-tunnel feature

These tests exercise the three startup paths of the `TUNNEL_NAME` ensure logic:
**adopt**, **restart-reuse**, and **create**. Run them in this order — each one
sets up the state the next one needs.

## Prerequisites

- Cloudflare account ID
- Zone ID for the test hostname's zone
- API token with **both** scopes:
  - Zone → DNS → Edit
  - Account → Cloudflare Tunnel → Edit
- A throwaway tunnel name (e.g. `cfw-test-1`) that does **not** already exist
- A throwaway hostname under your zone (e.g. `cfw-test.example.com`) whose DNS
  record does **not** already exist

## Setup

```bash
mkdir -p /tmp/cfw-test/cloudflared
cd /tmp/cfw-test
```

Create `cloudflared/config.yml`:

```yaml
ingress:
  - hostname: cfw-test.example.com
    service: http_status:200
  - service: http_status:404
```

No `tunnel:` or `credentials-file:` — the wrapper supplies both.

Build the image from the repo root:

```bash
docker build -t cfw-test /path/to/cloudflared-wrapped
```

Common run command (reused below):

```bash
docker run --rm \
  -e TUNNEL_NAME=cfw-test-1 \
  -e CF_ACCOUNT_ID=<account-id> \
  -e CF_ZONE_ID=<zone-id> \
  -e CF_API_TOKEN=<token> \
  -v "$PWD/cloudflared:/etc/cloudflared" \
  cfw-test
```

## Test A — adopt

Verifies that an already-existing tunnel (with no local `credentials.json`) is
discovered by name and its secret reconstructed via the `/token` endpoint.

**Preconditions**
- Tunnel `cfw-test-1` already exists in Cloudflare (create it manually in the
  Zero Trust dashboard, or run Test C first and then `rm credentials.json`)
- `cloudflared/credentials.json` does **not** exist locally

**Run** the common command above.

**Expected log**
```
[tunnel] adopting existing tunnel name=cfw-test-1 id=<uuid>
[sync] tunnel=<uuid> ...
[entrypoint] launching cloudflared tunnel
... Registered tunnel connection connIndex=0..3 ...
```

**Verify**
- `cloudflared/credentials.json` now exists and contains the adopted UUID
- cloudflared registers all 4 connections without auth errors

Stop with `Ctrl+C`.

## Test B — restart-reuse

Verifies the steady-state path: an existing `credentials.json` is read directly
and no API calls are made to ensure the tunnel.

**Preconditions**
- `cloudflared/credentials.json` exists on disk (left over from Test A)
- Tunnel still exists in Cloudflare

**Run** the common command above, unchanged.

**Expected log** (first line)
```
[tunnel] using existing credentials.json tunnel=<uuid>
```

No `adopting` or `creating` line. Flow goes straight to DNS sync and
cloudflared launch.

Stop with `Ctrl+C`.

## Test C — create

Verifies that when neither local creds nor a remote tunnel exist, a fresh
tunnel is created with a generated secret.

**Preconditions**
- Delete the tunnel in the Cloudflare Zero Trust dashboard (Networks →
  Tunnels → Delete)
- Remove the local creds:
  ```bash
  rm cloudflared/credentials.json
  ```

**Run** the common command above.

**Expected log**
```
[tunnel] creating new tunnel name=cfw-test-1
[tunnel] created tunnel id=<new-uuid>
[sync] tunnel=<new-uuid> ...
[entrypoint] launching cloudflared tunnel
... Registered tunnel connection connIndex=0..3 ...
```

**Verify**
- New tunnel appears in the Cloudflare dashboard
- `cloudflared/credentials.json` was written with the new UUID + a freshly
  generated `TunnelSecret`
- cloudflared connects without errors

Stop with `Ctrl+C`.

## Cleanup

```bash
rm -rf /tmp/cfw-test
docker image rm cfw-test
```

Delete `cfw-test-1` from the Cloudflare Zero Trust dashboard and remove the
leftover `cfw-test.<zone>` CNAME record from DNS.

## Known unrelated noise

DNS sync may log a parse error or "record already exists" error during these
tests — that's a pre-existing bug in `createRecord` response handling and a
leftover CNAME from prior runs. It does not affect the tunnel ensure logic
under test.
