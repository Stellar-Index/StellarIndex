---
title: CDN setup for `api.stellarindex.io` (L3.14)
last_verified: 2026-05-03
status: operator runbook
---

# CDN setup for `api.stellarindex.io`

Operator runbook for closing **L3.14** in the launch-readiness
backlog. The origin-side `Cache-Control` middleware ships in code
(`internal/api/v1/middleware/cachecontrol.go` + tests); this doc
covers the **infra-side** provisioning of a CDN in front of the
API.

## What the API origin sends today

Per ADR-0018 each surface emits a different `Cache-Control` header.
The authority is `internal/api/v1/middleware/cachecontrol.go`
(`policyForPath` + `ledgerPolicy`), pinned by
`cachecontrol_test.go`; the bands below are transcribed from it.

**Two things the earlier version of this table got wrong, so check
them before you build a CDN rule on it:** `/v1/price/tip` does NOT
share `/v1/price`'s policy — it is private — and the API emits no
`max-age=1` anywhere. The values actually in use are 2, 10, 15, 30,
60, 300, 3600, 86400 and `no-store`.

`s-maxage` (the CDN tier) is only emitted when the origin is
configured with a CDN in front of it (`cdnEnabled`); with it off,
the same routes emit the `max-age` half alone so a CDN the operator
does not have cannot cache anything.

| Path | Cache policy (CDN enabled) | Why |
| --- | --- | --- |
| `/v1/assets`, `/v1/assets/{id}`, `/v1/pools/reserves`, `/v1/liquidity-pools`, `/v1/sdex/orderbook` | `public, max-age=30, s-maxage=60` | Current price + current lake state — turns over inside one bucket / a few ledgers |
| `/v1/price`, `/v1/price/batch`, `/v1/price/changes`, `/v1/oracle/latest` | `public, max-age=30, s-maxage=5` | Closed-bucket price surfaces. The 5 s shared TTL is bounded by the SLA probe's 150 s freshness target (60 s bucket + 30 s CAGG `end_offset` + schedule + runtime); a longer shared TTL puts a compliant origin outside its own SLA at the edge (#344). |
| `/v1/history*`, `/v1/price/at`, `/v1/ohlc`, `/v1/vwap`, `/v1/twap`, `/v1/chart`, `/v1/markets`, `/v1/pairs`, `/v1/sources`, the rest of `/v1/oracle/*` (NOT `latest` — see the row above), `/v1/pools`, `/v1/lending/pools`, `/v1/aggregators`, `/v1/network/stats`, `/v1/incidents`, `/v1/sac-wrappers`, `/v1/issuers`, `/v1/issuers/{g}`, `/v1/accounts/stats`, `/v1/directory`, `/v1/changes/*` | `public, max-age=60, s-maxage=300` | Closed-bucket + catalogue. Immutable per ADR-0015, but the trailing edge advances — `s-maxage` caps how far a CDN entry may lag it |
| `/v1/ledgers/{n}`, `/v1/ledgers/{n}/transactions`, `/v1/ledgers/{n}/operations`, `/v1/tx/{hash}` | `public, max-age=60, s-maxage=300` | Immutable once the ledger closes. Deliberately 5 min, not a year: a seconds-old ledger can be served before every downstream projection lands |
| `/v1/ledgers`, `/v1/network/throughput`, `/v1/status` | `public, max-age=10, s-maxage=15` | Polled surfaces that move every few seconds |
| `/v1/methodology` | `public, max-age=300, s-maxage=600` | Mostly-static prose |
| `/v1/incidents.atom` | `public, max-age=60, s-maxage=120` | Poll-cadence feed |
| `/v1/price/tip`, `/v1/price/tip/*`, `/v1/observations*`, `/v1/sources/{name}/*` | `private, no-cache, must-revalidate` | **Not** the same class as `/v1/price`. Tip has no cross-region consistency contract (ADR-0018); caching it would change the contract |
| `/v1/account/*`, `/v1/accounts`, `/v1/accounts/*`, `/v1/auth/*`, `/v1/dashboard/*`, `/v1/signup`, and the default for any unmatched path | `private, no-store` | Per-caller or unknown; fail closed |
| `/v1/price/stream` (SSE), `/v1/healthz`, `/v1/readyz`, `/v1/version`, `/metrics` | `no-store` | Long-lived stream; and a cached probe is a lie |

A handful of handlers set the header themselves, which **overrides**
the middleware — do not infer these from the table above:

| Path | Cache policy | Set by |
| --- | --- | --- |
| `/v1/price`, `/v1/price/batch`, `/v1/price/changes`, `/v1/oracle/latest` | `Cache-Control: public, max-age=30, s-maxage=5` | Closed-bucket price surfaces. The 5 s shared TTL is bounded by the SLA probe's 150 s closed-bucket freshness target, which is itself 60 s bucket + 30 s CAGG `end_offset` + schedule + runtime; a longer shared TTL puts a compliant origin outside its own SLA at the edge (#344) |
| `/v1/price/tip` | `Cache-Control: private, no-cache, must-revalidate` | Tip — cache for ~tick-cadence; CDN absorbs hot-asset request floods |
| `/v1/history/since-inception` | `Cache-Control: public, max-age=300, s-maxage=86400` | Historical data is immutable on closed buckets — long edge-cache, short browser-cache |
| `/v1/assets/{id}` | `Cache-Control: public, max-age=60` | Asset-detail blocks; F2 fields refresh on supply-snapshot cadence |
| `/v1/sources`, `/v1/markets`, `/v1/assets`, `/v1/issuers`, `/v1/issuers/{g}` | `Cache-Control: public, max-age=60, s-maxage=300` | Catalogue surfaces — change rarely |
| `/v1/changes/{entity_type}/{id}` | `Cache-Control: public, max-age=60, s-maxage=300` | Multi-window delta strip — refreshed every 5 min by the change-summary worker |
| `/v1/diagnostics/*` | `Cache-Control: private, no-cache, must-revalidate` | Operator-facing live data — showcase polls every 15 s |
| `/v1/account/*`, `/v1/auth/*` | `Cache-Control: no-store` | Per-caller; never cacheable |
| SSE streams (`/stream` suffix) | `Cache-Control: no-store` | Long-lived; CDN must passthrough |
| `/v1/diagnostics/ingestion` | `public, max-age=15, s-maxage=15` | `diagnostics_ingestion.go:508,524` (the explorer polls it every 15 s; the middleware's `private, no-cache` band would defeat the edge) |
| `/v1/diagnostics/backups` | `public, max-age=60, s-maxage=60` | `diagnostics_backups.go:645` |
| `/v1/ledger/tip` | `public, max-age=2` | `ledger_tip.go:72` |
| `/v1/coverage/verdicts`, `/v1/protocols*` | `public, max-age=60` | `coverage_verdicts.go:216`, `protocols.go:660,755` |
| `/v1/incidents/{slug}` | `public, max-age=300` | `incidents.go:157` |
| `/errors/`, `/errors/{slug}` | `public, max-age=3600` | `server.go:2428,2434,2447` |
| `/.well-known/security.txt`, `/robots.txt` | `public, max-age=86400` | `server.go:2391,2507` |
| `/v1/livez/lake` | `no-store` | `server.go:2313` |

The middleware classifier lives at
`internal/api/v1/middleware/cachecontrol.go::policyForPath`. If
a CDN deployment differs from the conventions here, override per
path at the CDN layer rather than patching the middleware — origin
behaviour stays universal.

## Provider choice

Three are equivalent for our needs; pick by ops familiarity, not
technical criteria:

| Provider | Strengths | Pricing (rough) | Notes |
| --- | --- | --- | --- |
| **CloudFront (AWS)** | Mature, cheap egress at scale, integrates with Route53 if hosted there | ~$0.085/GB | r2 already runs on AWS — natural choice if multi-region wants to share AWS infra |
| **Bunny CDN** | Cheap, fast TTFB in EU, simple config | ~$0.005–0.020/GB | Best price/performance for v1; manual config via web UI |
| **Cloudflare** | Free tier covers most of v1 launch; deepest DDoS posture | Free–$20/mo | Zero TLS work; good if status-page also lives here |

**Recommendation for v1: Cloudflare.** Free tier covers projected
launch traffic, the same panel handles DNS + TLS, and the routing
rules mirror what we'd build on Bunny / CloudFront if we
graduate later.

## Step-by-step (Cloudflare)

```
0. Pre-reqs
   - DNS for stellarindex.io is already in Cloudflare (move there
     first if not — separate runbook).
   - Origin is reachable at the per-region HAProxy frontends
     (api-r1.stellarindex.io etc., per multi-region-topology.md).

1. Create the proxied DNS record
   - Type: CNAME (or A if pointing at a single region pre-multi-region)
   - Name: api
   - Target: <origin host>
   - Proxy status: Proxied (orange cloud)

2. Set up an SSL/TLS mode
   - SSL/TLS → Overview → Mode: Full (strict)
   - Edge cert auto-provisions via Cloudflare; origin uses our
     existing HAProxy TLS chain.

3. Configure caching
   - Caching → Configuration → Browser Cache TTL: Respect Existing Headers
     (origin sends max-age, don't override at edge).
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
   - SSE-specific: Cloudflare's auto-buffer for long-lived
     responses can break SSE. Set the per-route `cache-control`
     bypass + add a Page Rule with `Disable Performance` for
     `/*/stream` so HTTP/2 push optimisations don't intercept.
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

# 4. Origin gets a cache-key signal
# Check the origin's http_requests_total metric — historical
# endpoints should show a much lower request rate than they
# would unproxied (CDN absorbs the bulk).
```

## Rollback

If the CDN misbehaves (caching too aggressively, breaking SSE,
masking 5xx as 200), the rollback is a single DNS change:

```
DNS → api → Proxy status: DNS only (grey cloud)
```

The record stays the same; traffic just stops going through the
CDN. Origin behaviour is unchanged.

## Cross-references

- Origin middleware: `internal/api/v1/middleware/cachecontrol.go`
- Per-surface policy decisions: [ADR-0018](../adr/0018-api-consistency-surfaces.md)
- Multi-region origin layout: [multi-region-topology.md](../architecture/infrastructure/multi-region-topology.md)
- Launch-readiness row: L3.14 in [launch-readiness-backlog.md](../architecture/launch-readiness-backlog.md)
