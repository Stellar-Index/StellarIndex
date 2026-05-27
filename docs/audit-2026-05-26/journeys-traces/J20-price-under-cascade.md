# J20 — Consumer hits `/v1/price?asset=native&quote=fiat:USD` under F-0039 cascade

## Inputs

- HTTP GET `https://api.ratesengine.net/v1/price?asset=native&quote=fiat:USD`
- No auth (public surface)
- System state at trace time (2026-05-26 23:36 UTC):
  - `/v1/readyz` reports `status:degraded` + redis ok=false (MISCONF)
  - Redis `stop-writes-on-bgsave-error` engaged; BGSAVE failing on
    /var/lib/redis (root disk full per F-0001)
  - Last successful aggregator write was ~17:20 UTC (~6h ago)

## Hops

| # | Stage | File:line | Notes |
| --- | --- | --- | --- |
| 1 | Caddy reverse proxy | `configs/caddy/Caddyfile.r1` | TLS termination, HTTP→HTTPS redirect, request_id header injection |
| 2 | Router | `internal/api/v1/server.go:981` | `GET /v1/price` → `s.handlePrice` |
| 3 | Param parse | `internal/api/v1/price.go:242-281` | asset + quote canonicalisation; reject identity pairs |
| 4 | LatestPrice (closed-bucket read) | `internal/api/v1/price.go:283` | `reader.LatestPrice(ctx, asset, quote)` |
| 5 | Redis VWAP lookup | (`reader` impl behind interface) | `vwap:native:fiat:USD:300` cache key; STALE due to F-0039 — last write ~17:20 |
| 6 | Returns `ErrPriceNotFound` | price.go:285 | branches into priceFallback |
| 7 | priceFallback chain | price.go:287, 483 | last-trade → stablecoin proxy → triangulation |
| 8 | Returns LKG snapshot | (varies by branch) | `Price = 0.15107020700989818388` observed at 2026-05-25T18:26:00Z; sources=["sdex"] |
| 9 | `stale=true` is set | price.go:300 | "ok" branch fires; cascade signal preserved |
| 10 | Confidence enrichment | price.go:338 | best-effort; cache miss is OK |
| 11 | Flags assembly | price.go:340-364 | `stale:true, triangulated:true, single_source:true` |
| 12 | writeJSON envelope | `internal/api/v1/envelope.go:90` | data + as_of + sources + flags wrapped |
| 13 | Cache-Control middleware | `internal/api/v1/middleware/cache_control.go` | applies route-specific TTL |
| 14 | HTTP 200 response | | Customer receives properly-flagged stale envelope |

## Sinks

- DB table(s): none on this path (closed-bucket read is cache-only;
  fallback chain may touch `trades` hypertable in PG)
- Redis keys read: `vwap:native:fiat:USD:300`,
  `confidence:native:fiat:USD:300`, `freeze:native:fiat:USD`,
  `divergence:firing:native`
- HTTP response shape:
  ```json
  {"data":{"asset_id":"native","quote":"fiat:USD",
   "price":"0.15107020700989818388","price_type":"vwap",
   "observed_at":"2026-05-25T18:26:00Z","window_seconds":60},
   "as_of":"2026-05-26T23:36:59Z","sources":["sdex"],
   "flags":{"stale":true,"reduced_redundancy":false,
            "triangulated":true,"divergence_warning":false,
            "single_source":true}}
  ```
- Log lines: typically silent on successful fallback path; WARN
  on divergence lookup failure (Redis blip)
- Metrics: `obs.APIRequestDurationSeconds{route="/v1/price",
  status="200"}` updated; NOT
  `obs.PriceStalenessSeconds` (the aggregator owns that —
  see comment at price.go:324)
- Alerts: none directly fired from this handler

## Failure modes

| Hop | Bad input | Behaviour | Defence |
| --- | --- | --- | --- |
| 3 | Missing `asset` | 400 `errors/missing-asset` RFC-7807 | param-parse handler |
| 3 | Invalid asset ID | 400 `errors/invalid-asset-id` | canonical.ParseAsset returns err |
| 3 | Identity pair (asset==quote) | 400 `errors/identity-price` | explicit check |
| 4 | LatestPrice internal err | 500 `errors/internal` | logged + returned |
| 5 | Redis MISCONF (current!) | falls through to priceFallback | cache-aside design |
| 6 | priceFallback ok=false | 404 `errors/price-not-found` | nothing-to-serve case |
| 7-8 | Stale LKG | served with `stale:true` flag | ADR-0018 staleness contract |
| 9 | F-1254 bug (pre-2026-05-12) | stale flag was NOT set | rc-pre-fix served stale data with stale:false; FIXED |

## Tests

- Unit tests: `internal/api/v1/price_test.go`, `price_internal_test.go`
- Integration tests: `test/integration/` (testcontainers Postgres)
- Adversarial cases: F-0049 fail-open ratelimit means a bot can hammer
  this route under F-0039 without throttle penalty

## Live R1 trace (captured 2026-05-26 23:36 UTC)

Raw request:
```
GET https://api.ratesengine.net/v1/price?asset=native&quote=fiat:USD
```

Raw response body:
```json
{"data":{"asset_id":"native","quote":"fiat:USD",
"price":"0.15107020700989818388","price_type":"vwap",
"observed_at":"2026-05-25T18:26:00Z","window_seconds":60},
"as_of":"2026-05-26T23:36:59.026719075Z","sources":["sdex"],
"flags":{"stale":true,"reduced_redundancy":false,
"triangulated":true,"divergence_warning":false,
"single_source":true}}
```

**Audit verdict:** the handler degrades CORRECTLY under F-0039.
ADR-0018's staleness contract is honoured. Customer receives a
truthful response — stale data marked as stale, with single-source
indicator and triangulation provenance. The F-0060 retraction
(iteration 8) was the right call.

**Cross-references:**
- F-0039 (root cause: Redis MISCONF)
- F-0042 POSITIVE (envelope contract honoured)
- F-0049, F-0050 (fail-open rate-limit means bots aren't throttled)
- F-0060 RETRACTED (this trace is the evidence)
- F-0074 (TWAP/OHLC lack the same fallback chain)
