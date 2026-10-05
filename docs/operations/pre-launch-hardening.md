---
title: Pre-launch hardening checklist
last_verified: 2026-05-05
status: operator runbook
---

# Pre-launch hardening checklist

Run BEFORE flipping public DNS at `api.stellarindex.io` / `stellarindex.io`. Each
item is a config edit + restart on R1 (~10 min total). At startup the API logs
`SECURITY:` warnings for dev-friendly defaults still in place:
`journalctl -u stellarindex-api -b -p warning | grep SECURITY`.

## 1. Bind the API to loopback

**Why:** the API listens on `0.0.0.0:3000` by default, so raw
`http://<R1-IP>:3000` bypasses Caddy's TLS. Loopback makes Caddy mandatory.

```toml
# /etc/stellarindex.toml
[api]
listen_addr = "127.0.0.1:3000"
```

```sh
systemctl restart stellarindex-api
ss -tlnp | grep stellarindex-api    # should show 127.0.0.1:3000, not *:3000
```

Verify:

```sh
# From R1 (loopback) — works:
curl -fsS http://localhost:3000/v1/healthz

# From the public internet (replace with your IP) — should now refuse:
curl --connect-timeout 3 http://136.243.90.96:3000/v1/healthz   # connection refused

# Through Caddy (TLS) — works post-DNS:
curl -fsS https://api.stellarindex.io/v1/healthz
```

Caddy is on the same host, so only external direct-port access dies.

## 2. Narrow CORS

**Why:** `[api].allowed_origins = ["*"]` with `auth_mode = apikey_optional` (R1's
mode) lets a third-party site phish a logged-in user's bearer token.

```toml
[api]
allowed_origins = [
  "https://stellarindex.io",
  "https://api.stellarindex.io",
]
```

For Cloudflare Pages previews (`<branch>.<projectslug>.pages.dev`) add specific
hostnames temporarily; the CORS middleware does not honour wildcards. Then
`systemctl restart stellarindex-api`.

## 3. Confirm trusted-proxy CIDRs match the proxy's source

With the loopback bind the only caller is Caddy, so
`trusted_proxy_cidrs = ["127.0.0.1/32"]` is already correct on R1. Expand it only
if Caddy moves to another host, else `X-Forwarded-For` from the wider internet
becomes trusted. No edit needed today.

## 4. Cloudflare proxy in front of api.stellarindex.io (recommended)

**Why:** Caddy gives TLS but no L7 protection; R1 is one box. Cloudflare's free tier
adds WAF, edge rate limiting and bot blocking.

1. Set the `api` A record to `136.243.90.96` with the **orange cloud** ON.
2. **No API-side `trusted_proxy_cidrs` change** (ADR-0025): the trust boundary stays
   at Caddy. The Caddyfile's global `servers { trusted_proxies static <CF CIDRs>;
   client_ip_headers CF-Connecting-IP X-Forwarded-For }` block resolves the real
   client IP and forwards it as `X-Forwarded-For: {client_ip}`; the API trusts only
   `127.0.0.1/32`.
3. Refresh CF's CIDR list in the Caddyfile quarterly or on a CF notice; see
   [`configs/caddy/README.md` §"Real client IP under
   Cloudflare"](../../configs/caddy/README.md).
4. (Optional) Cloudflare Origin Cert instead of Let's Encrypt, so CF edge → origin
   is authenticated.

## 5. Healthchecks.io URLs

Five URLs in `/etc/default/stellarindex-healthchecks`:

```sh
HEALTHCHECKS_URL_INDEXER='https://hc-ping.com/<uuid-indexer>'
HEALTHCHECKS_URL_AGGREGATOR='https://hc-ping.com/<uuid-aggregator>'
HEALTHCHECKS_URL_API='https://hc-ping.com/<uuid-api>'
HEALTHCHECKS_URL_SMOKE='https://hc-ping.com/<uuid-smoke>'
HEALTHCHECKS_URL_SLA_PROBE='https://hc-ping.com/<uuid-sla-probe>'
```

Deadmansswitch and Discord webhooks in `/etc/default/alertmanager-secrets`:

```sh
HEALTHCHECKS_DEADMANSSWITCH_URL='https://hc-ping.com/<uuid-dms>'
# Discord incoming webhooks (Server Settings → Integrations →
# Webhooks). One per channel; point both at the same URL for a
# single channel.
DISCORD_WEBHOOK_URL_PAGES='https://discord.com/api/webhooks/<id>/<token>'
DISCORD_WEBHOOK_URL_ALERTS='https://discord.com/api/webhooks/<id>/<token>'
```

Apply. `stellarindex-sla-probe.timer` must be in the restart set so systemd reloads
the EnvironmentFile; otherwise the SLA-evidence check stays silent (F-1304).

```sh
systemctl restart \
  'stellarindex-heartbeat@*.timer' \
  stellarindex-smoke.timer \
  stellarindex-sla-probe.timer
bash /opt/stellarindex/alertmanager/apply.sh
```

## 6. FX API keys (recommended, not blocking)

The 4 FX sources shown "stopped" in `/v1/sources` lack operator API keys. In
`/etc/stellarindex.toml`:

```toml
[external.fx]
openexchangerates_app_id = "<your-key>"
# … per the source-config docs
```

Restart the indexer; the aggregator picks them up next tick. Without them fiat
divergence has fewer cross-checks (CoinGecko + Reflector still cover most cases).

## 7. Smoke from the open internet

After DNS lands and Caddy has its cert, from your laptop (NOT R1):

```sh
API_BASE_URL=https://api.stellarindex.io make smoke
```

13/13 green confirms TLS, DNS, cert and path end-to-end.

## 8. Backup baseline

Until L4.16 (automated daily Postgres dumps + MinIO replication) lands, take a
manual baseline, copy it off-host and document where:

```sh
# On R1:
pg_dump -h localhost -U stellarindex stellarindex | gzip \
  > /var/backups/stellarindex-baseline-$(date +%F).sql.gz
mc mirror /var/lib/galexie-archive remote-backup/galexie-archive-baseline-$(date +%F)
```

## Verification at the end

`scripts/ops/pre-launch-check.sh` is read-only, prints `pass / warn / fail` per
step, exit code = number of fails.

```sh
# Run end-to-end on R1:
ssh root@r1 'bash -s' < scripts/ops/pre-launch-check.sh

# Or interactively after a one-time copy:
scp scripts/ops/pre-launch-check.sh root@r1:/opt/stellarindex/
ssh root@r1 'bash /opt/stellarindex/pre-launch-check.sh'
```

By hand:

```sh
journalctl -u stellarindex-api -b -p warning | grep SECURITY    # empty == good
ss -tlnp | grep stellarindex-api        # 127.0.0.1:3000
API_BASE_URL=https://api.stellarindex.io make smoke
```
