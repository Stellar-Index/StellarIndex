# API Route Inventory

Generated during audit execution 2026-05-26 22:30 UTC.

## Routes in server.go (~55 total)

Auth gate column: `public` (no auth) / `auth-key` (API key) /
`admin` (dashboard) / `sep10` (federated Stellar) / `webhook`
(Stripe HMAC). Auditor verifies live behaviour per route in W11.

### Health / version / status

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/healthz | handleHealthz | public | liveness |
| GET | /v1/readyz | handleReadyz | public | readiness |
| GET | /v1/version | handleVersion | public | binary version |
| GET | /v1/status | handleStatus | public | rollup status |
| GET | /metrics | loopbackOnly | loopback-only ✓ | Prometheus |
| GET | / | handleRoot | public | root | landing |
| GET | /robots.txt | handleRobotsTxt | public | |
| GET | /.well-known/security.txt | handleSecurityTxt | public | |

### Diagnostics — F-0026 concern

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/diagnostics/cursors | handleCursors | **public?** | exposes 20-day-old backfill cursors per F-0026 — gate admin? |
| GET | /v1/diagnostics/ingestion | handleDiagnosticsIngestion | **public?** | same concern |

### Pricing surface

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/price | handlePrice | public | last-closed bucket (ADR-0015) |
| GET | /v1/price/tip | handlePriceTip | public | tip bucket (in-progress) — verify per ADR-0015 |
| GET | /v1/price/tip/stream | handlePriceTipStream | public | SSE |
| GET | /v1/price/stream | handlePriceStream | public | SSE |
| GET | /v1/price/batch | handlePriceBatch | public | |
| POST | /v1/price/batch | handlePriceBatchPost | public | |
| GET | /v1/history | handleHistory | public | |
| GET | /v1/history/since-inception | handleHistorySinceInception | public | |
| GET | /v1/chart | handleChart | public | |
| GET | /v1/ohlc | handleOHLC | public | candle data |
| GET | /v1/vwap | handleVWAP | public | first-class VWAP endpoint (CG/CMC don't have this) |
| GET | /v1/twap | handleTWAP | public | first-class TWAP endpoint |
| GET | /v1/markets | handleMarkets | public | per-source markets |
| GET | /v1/pools | handlePools | public | DEX pools |
| GET | /v1/pairs | handlePairs | public | trading pairs |

### Assets / identity

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/assets | handleAssetList | public | |
| GET | /v1/assets/verified | handleAssetsVerified | public | catalogue |
| GET | /v1/assets/{asset_id} | handleAssetGet | public | dual-shape GlobalAssetView vs AssetDetail |
| GET | /v1/assets/{asset_id}/metadata | handleAssetMetadata | public | |
| GET | /v1/assets/{asset_id}/{network} | handleAssetByNetwork | public | multi-network |
| GET | /v1/issuers | handleIssuersList | public | |
| GET | /v1/issuers/{g_strkey} | handleIssuer | public | |
| GET | /v1/sac-wrappers | handleSACWrappers | public | |
| GET | /v1/changes/{entity_type}/{id} | handleChangeSummary | public | % change summary |
| GET | /v1/network/stats | handleNetworkStats | public | network depth/freshness |
| GET | /v1/ledger/tip | handleLedgerTip | public | live ledger tip |
| GET | /v1/ledger/stream | handleLedgerStream | public | SSE of ledger closes |

### Oracle

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/oracle/latest | handleOracleLatest | public | recent oracle pushes |
| GET | /v1/oracle/streams | handleOracleStreams | public | |
| GET | /v1/oracle/lastprice | handleOracleLastPrice | public | SEP-40 `lastprice` |
| GET | /v1/oracle/prices | handleOraclePrices | public | SEP-40 `prices` |
| GET | /v1/oracle/x_last_price | handleOracleXLastPrice | public | SEP-40 `x_last_price` |
| GET | /v1/observations | handleObservations | public | |
| GET | /v1/observations/stream | handleObservationsStream | public | SSE |

### Lending / TVL

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/lending/pools | handleLendingPools | public | blend/etc lending pools |

### Sources / methodology / incidents

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/sources | handleSources | public | source list + classes |
| GET | /v1/methodology | handleMethodology | public | aggregation methodology |
| GET | /v1/incidents | handleIncidents | public | incident history |
| GET | /v1/incidents.atom | handleIncidentsAtom | public | RSS/Atom |

### Account / billing surface

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/account/me | handleAccountMe | auth-key | own account |
| GET | /v1/account/usage | handleAccountUsage | auth-key | usage metering |
| GET | /v1/account/keys | handleAccountKeysList | auth-key | list own keys |
| POST | /v1/account/keys | handleAccountKeysCreate | auth-key | create key |
| DELETE | /v1/account/keys/{keyID} | handleAccountKeysRevoke | auth-key | revoke key |
| POST | /v1/signup | handleSignup | public | rate-limited |
| GET | /v1/signup/verify | handleSignupVerify | public (token-gated) | email-link verify |
| POST | /v1/webhooks/stripe | handleStripeWebhook | webhook (HMAC) | Stripe signature verify (W33) |

### Auth (SEP-10 federated)

| Method | Path | Handler | Auth | Notes |
| --- | --- | --- | --- | --- |
| GET | /v1/auth/sep10/challenge | handleSEP10Challenge | public | |
| POST | /v1/auth/sep10/token | handleSEP10Token | sep10-signed | |

## Coverage notes

- 53 routes mounted (`Mount` ServeMux calls in server.go).
- `/metrics` is `loopbackOnly` (line 957) — Prometheus scrape
  bound to 127.0.0.1 ✓
- `/v1/account/*` + `/v1/webhooks/stripe` are the money-touching
  surface (W19, W33).
- `/v1/diagnostics/*` exposes operational state — F-0026 flags
  the possible information-leak vector.
- `/v1/oracle/*` is the SEP-40 compatibility surface.
- `/v1/price/tip*` serves IN-PROGRESS buckets — needs explicit
  ADR-0015 carve-out review (does serving tip violate the
  closed-bucket invariant for THESE specific endpoints?
  Or are they intentional carve-outs that flag staleness?).

## Per-route audit pass (W11)

Each route needs the 11-check loop from `02-protocol.md` §8:
OpenAPI presence, envelope conformance, auth gate, rate-limit
identity, cache headers, pagination, empty/404 shape, latency
budget, test coverage, removed-route hygiene, prewarm drift.

Status: `todo` per route. Audit populates the per-route results
in workstream evidence.
