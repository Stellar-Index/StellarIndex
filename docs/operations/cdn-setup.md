---
title: CDN setup for `api.stellarindex.io` (L3.14)
last_verified: 2026-10-21
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
| `/v1/assets`, `/v1/assets/*`, `/v1/rwa/assets`, `/v1/stablecoins`, `/v1/pools/reserves`, `/v1/liquidity-pools`, `/v1/sdex/orderbook`, `/v1/protocols/{name}/tvl`, `/v1/lending/pools/{pool}/reserves`, `/v1/external/assets`, `/v1/external/assets/*`, `/v1/aggregators` | `public, max-age=30, s-maxage=60` | Current price + lake state; turns over inside one bucket / a few ledgers |
| `/v1/price`, `/v1/price/batch*`, `/v1/price/changes`, `/v1/price/at`, `/v1/vwap`, `/v1/twap`, `/v1/oracle/latest`, `/v1/oracle/lastprice`, `/v1/oracle/prices`, `/v1/oracle/x_last_price` | `public, max-age=30, s-maxage=5` | Closed-bucket price. The 5 s shared TTL is bounded by the SLA probe's 150 s freshness target (60 s bucket + 30 s CAGG `end_offset` + schedule + runtime); longer puts a compliant origin outside its SLA at the edge (#344) |
| `/v1/history*`, `/v1/ohlc`, `/v1/chart`, `/v1/markets`, `/v1/markets/sources`, `/v1/mev`, `/v1/search`, `/v1/pairs`, `/v1/sources`, the rest of `/v1/oracle/*`, `/v1/pools`, `/v1/lending/pools`, `/v1/incidents`, `/v1/sac-wrappers`, `/v1/issuers`, `/v1/issuers/{g}`, `/v1/accounts/stats`, `/v1/accounts/creators`, `/v1/accounts/sponsors`, `/v1/directory`, `/v1/changes/*`, `/v1/rwa/history`, `/v1/rwa/premium` | `public, max-age=60, s-maxage=300` | Closed-bucket + catalogue. Immutable per ADR-0015 but the trailing edge advances; `s-maxage` caps CDN lag |
| `/v1/ledgers/{n}`, `/v1/ledgers/{n}/transactions`, `/v1/ledgers/{n}/operations`, `/v1/tx/{hash}` | `public, max-age=60, s-maxage=300` | Immutable once closed; 5 min not a year because a seconds-old ledger can be served before every projection lands |
| `/v1/ledgers`, `/v1/ledgers/at`, `/v1/network/throughput`, `/v1/network/stats`, `/v1/status`, `/v1/status/notices`, `/v1/operations`, `/v1/contracts`, `/v1/contracts/stats`, `/v1/contracts/{id}`, `/v1/contracts/{id}/interactions`, `/v1/contracts/{id}/code-history`, `/v1/contracts/{id}/transfers` | `public, max-age=10, s-maxage=15` | Polled surfaces that move every few seconds. Deliberately NOT the 300 s band: the server-side cache serves stale on expiry and refreshes only on a request, so at low arrival rates entry age is bounded by the inter-arrival gap (93 s measured on r1) and a long shared TTL would compound it. `/transfers` first page moves with every transfer |
| `/v1/methodology` | `public, max-age=300, s-maxage=600` | Mostly-static prose |
| `/v1/incidents.atom` | `public, max-age=60, s-maxage=120` | Poll-cadence feed |
| `/v1/price/tip`, `/v1/price/tip/*`, `/v1/observations*`, `/v1/sources/{name}/*` | `private, no-cache, must-revalidate` | Tip has no cross-region consistency contract (ADR-0018); caching would change it |
| `/v1/account/*`, `/v1/accounts`, `/v1/accounts/*`, `/v1/auth/*`, `/v1/dashboard/*`, `/v1/signup`, `/v1/anomalies`, `/v1/divergence`, `/v1/divergence/series`, and any unmatched path | `private, no-store` | Per-caller or unknown; fail closed. Anomalies/divergence carry live freeze state an edge copy would misreport |
| `/v1/price/stream` (SSE), `/v1/healthz`, `/v1/readyz`, `/v1/version`, `/metrics` | `no-store` | Long-lived stream; a cached probe is a lie |

Handlers that set the header themselves **override** the middleware; do not
infer these from the table above:

| Path | Cache policy | Set by |
| --- | --- | --- |
| `/v1/diagnostics/ingestion` | `public, max-age=15, s-maxage=15` | `diagnostics_ingestion.go` (middleware's private band would defeat the edge) |
| `/v1/diagnostics/backups` | `public, max-age=60, s-maxage=60` | `diagnostics_backups.go` |
| `/v1/ledger/tip` | `public, max-age=2` | `ledger_tip.go` |
| `/v1/coverage/verdicts`, `/v1/protocols`, `/v1/protocols/{name}` | `public, max-age=60` | `coverage_verdicts.go`, `protocols.go` |
| `/v1/contracts/{id}/wasm` | `public, max-age=300` with a TTL, else `public, max-age=60, s-maxage=300` | `explorer/wasm_view.go` |
| `/v1/incidents/{slug}` | `public, max-age=300` | `incidents.go` |
| `/errors/`, `/errors/{slug}` | `public, max-age=3600` | `server.go` |
| `/.well-known/security.txt`, `/robots.txt` | `public, max-age=86400` | `server.go` |
| `/v1/livez/lake` | `no-store` | `server.go` |
| Any error response, and any envelope flagged `degraded` | `no-store` | `envelope.go`, so a CDN never replays a transient failure |
| CSV exports | `private, no-store` | `explorer/csv.go` |
| SSE streams (`/stream` suffix) | `no-store` | CDN must passthrough |

If a CDN deployment differs, override per path at the CDN layer rather than
patching `policyForPath`; origin behaviour stays universal.

## Provider choice

Pick by ops familiarity. **v1: Cloudflare** (free tier covers launch traffic,
one panel for DNS + TLS; rules port to Bunny/CloudFront later).

| Provider | Pricing (rough) | Notes |
| --- | --- | --- |
| CloudFront (AWS) | ~$0.085/GB | No AWS footprint since r2/r3 moved to Vultr (ADR-0050) |
| Bunny CDN | ~$0.005–0.020/GB | Best price/performance; manual web-UI config |
| Cloudflare | Free–$20/mo | Zero TLS work; deepest DDoS posture |

## Step-by-step (Cloudflare)

```
0. Pre-reqs
   - DNS for stellarindex.io is already in Cloudflare.
   - Origin reachable at each region's API origin
     (per ha-plan.md §2.3).

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
   - Cache Rules: Cache Eligibility = Eligible for cache, Edge TTL =
     "Use cache-control header if present". Never set a fixed Edge TTL:
     the origin's `s-maxage` bands are the contract (the price band's 5 s
     is bounded by the SLA probe), and Cloudflare does not cache JSON
     without a rule.
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
- Multi-region origin layout: [ha-plan.md §2.3](../architecture/ha-plan.md#23-planned-regions)
