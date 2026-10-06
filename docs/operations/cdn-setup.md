---
title: CDN setup for `api.stellarindex.io` (L3.14)
last_verified: 2026-05-03
status: operator runbook
---

# CDN setup for `api.stellarindex.io`

Operator runbook for L3.14: **infra-side** CDN provisioning in front of the
API. The origin-side `Cache-Control` middleware ships in code.

## What the API origin sends today

Authority: `internal/api/v1/middleware/cachecontrol.go` (`policyForPath` +
`ledgerPolicy`), pinned by `cachecontrol_test.go` (ADR-0018). Values in use: 2,
10, 15, 30, 60, 300, 3600, 86400 and `no-store`; there is no `max-age=1`.
`/v1/price/tip` does NOT share `/v1/price`'s policy: it is private.

`s-maxage` is emitted only when the origin is configured with a CDN in front
(`cdnEnabled`); otherwise the same routes emit `max-age` alone.

| Path | Cache policy (CDN enabled) | Why |
| --- | --- | --- |
| `/v1/assets`, `/v1/assets/{id}`, `/v1/pools/reserves`, `/v1/liquidity-pools`, `/v1/sdex/orderbook`, `/v1/lending/pools/{pool}/reserves`, `/v1/external/assets`, `/v1/external/assets/{slug}` | `public, max-age=30, s-maxage=60` | Current price + lake state; turns over inside one bucket / a few ledgers |
| `/v1/price`, `/v1/price/batch`, `/v1/price/changes`, `/v1/oracle/latest` | `public, max-age=30, s-maxage=5` | Closed-bucket price. The 5 s shared TTL is bounded by the SLA probe's 150 s freshness target (60 s bucket + 30 s CAGG `end_offset` + schedule + runtime); longer puts a compliant origin outside its SLA at the edge (#344) |
| `/v1/history*`, `/v1/price/at`, `/v1/ohlc`, `/v1/vwap`, `/v1/twap`, `/v1/chart`, `/v1/markets`, `/v1/markets/sources`, `/v1/mev`, `/v1/search`, `/v1/pairs`, `/v1/sources`, the rest of `/v1/oracle/*` (NOT `latest`), `/v1/pools`, `/v1/lending/pools`, `/v1/aggregators`, `/v1/network/stats`, `/v1/incidents`, `/v1/sac-wrappers`, `/v1/issuers`, `/v1/issuers/{g}`, `/v1/accounts/stats`, `/v1/accounts/creators`, `/v1/accounts/sponsors`, `/v1/directory`, `/v1/changes/*` | `public, max-age=60, s-maxage=300` | Closed-bucket + catalogue. Immutable per ADR-0015 but the trailing edge advances; `s-maxage` caps CDN lag |
| `/v1/ledgers/{n}`, `/v1/ledgers/{n}/transactions`, `/v1/ledgers/{n}/operations`, `/v1/tx/{hash}` | `public, max-age=60, s-maxage=300` | Immutable once closed; 5 min not a year because a seconds-old ledger can be served before every projection lands |
| `/v1/ledgers`, `/v1/network/throughput`, `/v1/status`, `/v1/status/notices`, `/v1/operations`, `/v1/contracts`, `/v1/contracts/{id}`, `/v1/contracts/{id}/interactions`, `/v1/contracts/{id}/code-history`, `/v1/contracts/{id}/transfers` | `public, max-age=10, s-maxage=15` | Polled surfaces that move every few seconds. Deliberately NOT the 300 s band: the server-side cache serves stale on expiry and refreshes only on a request, so at low arrival rates entry age is bounded by the inter-arrival gap (93 s measured on r1) and a long shared TTL would compound it. `/transfers` first page moves with every transfer |
| `/v1/methodology` | `public, max-age=300, s-maxage=600` | Mostly-static prose |
| `/v1/incidents.atom` | `public, max-age=60, s-maxage=120` | Poll-cadence feed |
| `/v1/price/tip`, `/v1/price/tip/*`, `/v1/observations*`, `/v1/sources/{name}/*` | `private, no-cache, must-revalidate` | Tip has no cross-region consistency contract (ADR-0018); caching would change it |
| `/v1/account/*`, `/v1/accounts`, `/v1/accounts/*`, `/v1/auth/*`, `/v1/dashboard/*`, `/v1/signup`, `/v1/anomalies`, `/v1/divergence`, `/v1/divergence/series`, and any unmatched path | `private, no-store` | Per-caller or unknown; fail closed. Anomalies/divergence carry live freeze state an edge copy would misreport |
| `/v1/price/stream` (SSE), `/v1/healthz`, `/v1/readyz`, `/v1/version`, `/metrics` | `no-store` | Long-lived stream; a cached probe is a lie |

Handlers that set the header themselves **override** the middleware; do not
infer these from the table above:

| Path | Cache policy | Set by |
| --- | --- | --- |
| `/v1/history/since-inception` | `public, max-age=300, s-maxage=86400` | Closed buckets are immutable: long edge, short browser |
| `/v1/assets/{id}` | `public, max-age=60` | F2 fields refresh on supply-snapshot cadence |
| `/v1/diagnostics/*` | `private, no-cache, must-revalidate` | Operator live data, showcase polls every 15 s |
| `/v1/diagnostics/ingestion` | `public, max-age=15, s-maxage=15` | `diagnostics_ingestion.go:508,524` (middleware's private band would defeat the edge) |
| `/v1/diagnostics/backups` | `public, max-age=60, s-maxage=60` | `diagnostics_backups.go:645` |
| `/v1/ledger/tip` | `public, max-age=2` | `ledger_tip.go:72` |
| `/v1/coverage/verdicts`, `/v1/protocols*` | `public, max-age=60` | `coverage_verdicts.go:216`, `protocols.go:660,755` |
| `/v1/incidents/{slug}` | `public, max-age=300` | `incidents.go:157` |
| `/errors/`, `/errors/{slug}` | `public, max-age=3600` | `server.go:2428,2434,2447` |
| `/.well-known/security.txt`, `/robots.txt` | `public, max-age=86400` | `server.go:2391,2507` |
| `/v1/livez/lake` | `no-store` | `server.go:2313` |
| SSE streams (`/stream` suffix) | `no-store` | CDN must passthrough |

If a CDN deployment differs, override per path at the CDN layer rather than
patching `policyForPath`; origin behaviour stays universal.

## Provider choice

Pick by ops familiarity. **v1: Cloudflare** (free tier covers launch traffic,
one panel for DNS + TLS; rules port to Bunny/CloudFront later).

| Provider | Pricing (rough) | Notes |
| --- | --- | --- |
| CloudFront (AWS) | ~$0.085/GB | r2 runs on AWS; natural if multi-region shares AWS infra |
| Bunny CDN | ~$0.005–0.020/GB | Best price/performance; manual web-UI config |
| Cloudflare | Free–$20/mo | Zero TLS work; deepest DDoS posture |

## Step-by-step (Cloudflare)

```
0. Pre-reqs
   - DNS for stellarindex.io is already in Cloudflare.
   - Origin reachable at the per-region HAProxy frontends
     (api-r1.stellarindex.io etc., per multi-region-topology.md).

1. Create the proxied DNS record
   - Type: CNAME (or A if pointing at a single region pre-multi-region)
   - Name: api
   - Target: <origin host>
   - Proxy status: Proxied (orange cloud)

2. Set up an SSL/TLS mode
   - SSL/TLS → Overview → Mode: Full (strict)
   - Edge cert is Cloudflare's; origin uses the existing HAProxy TLS chain.

3. Configure caching
   - Caching → Configuration → Browser Cache TTL: Respect Existing Headers.
   - Page Rules / Cache Rules:
     - URL pattern: api.stellarindex.io/v1/history/*
       Cache Level: Cache Everything
       Edge Cache TTL: 1 day
     - URL pattern: api.stellarindex.io/v1/sources
       Cache Level: Cache Everything
       Edge Cache TTL: 5 minutes
     - URL pattern: api.stellarindex.io/v1/markets
       Cache Level: Cache Everything
       Edge Cache TTL: 5 minutes
     - URL pattern: api.stellarindex.io/v1/auth/*
       Cache Level: Bypass
     - URL pattern: api.stellarindex.io/v1/account/*
       Cache Level: Bypass
     - URL pattern: api.stellarindex.io/*/stream
       Cache Level: Bypass
       (also: WebSockets/SSE → set to no-buffer at proxy layer)

4. Lock SSE passthrough
   - Network → WebSockets: ON
   - Cloudflare's auto-buffer of long-lived responses breaks SSE: keep the
     per-route `cache-control` bypass + a Page Rule with `Disable Performance`
     for `/*/stream`.
```

## Verification

After config takes effect (DNS propagation + first proxy):

```sh
# 1. Cache headers survive the edge
curl -sI https://api.stellarindex.io/v1/history/since-inception?asset=native | grep -iE "cache-control|cf-cache-status|age"
# Expect:
#   cache-control: public, max-age=60, s-maxage=300
#   cf-cache-status: HIT (or MISS on first request, then HIT)

# 2. Auth endpoints bypass
curl -sI https://api.stellarindex.io/v1/account/me -H "Authorization: Bearer <demo-key>" | grep -iE "cache-control|cf-cache-status"
# Expect:
#   cache-control: private, no-store   (bare `no-store` on the 401 path)
#   cf-cache-status: BYPASS

# 3. SSE passes through
curl -sN https://api.stellarindex.io/v1/price/tip/stream?base=native&quote=fiat:USD &
# Should emit `data:` lines within 5s; Ctrl-C to close.

# 4. Origin http_requests_total for historical endpoints should drop vs unproxied.
```

## Rollback

If the CDN caches too aggressively, breaks SSE or masks 5xx as 200, one DNS change
rolls back (record unchanged, origin behaviour unchanged):

```
DNS → api → Proxy status: DNS only (grey cloud)
```

## Cross-references

- Origin middleware: `internal/api/v1/middleware/cachecontrol.go`
- Per-surface policy decisions: [ADR-0018](../adr/0018-api-consistency-surfaces.md)
- Multi-region origin layout: [multi-region-topology.md](../architecture/infrastructure/multi-region-topology.md)
