# CoinGecko / CoinMarketCap Parity Matrix

CG and CMC are the consumer benchmarks. This matrix enumerates
every feature one of them ships and records whether we ship the
equivalent. Closure rule: every row must be filled before the
audit closes (no blank `?` cells).

Scoring:

- `covered` — we ship it, with proof (file:line or live URL)
- `partial` — we ship some of it; specify the gap
- `gap` — we don't ship it; finding required
- `non-goal` — explicit product decision; cite the decision doc
- `n/a` — feature is structurally impossible for our scope

## Coin / Asset metadata

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Asset detail page (description, links, contracts) | ✅ | ✅ | `/v1/assets/usdc` lives (EV-0033 — description, class, verified_issuer, networks[]) | `covered` | live curl EV-0033 |
| Verified-currency badge | ✅ | ✅ | `/v1/assets/verified` + per-asset `verified_issuer` field | `covered` | `internal/currency/verified.go` + EV-0033 |
| Social links | ✅ | ✅ | SEP-1 toml resolution surfaces `home_domain` + `documentation` URL | `covered` | `internal/metadata/sep1.go`; CG/CMC use API-stored fields, we use the issuer's own SEP-1 (more authoritative) |
| Whitepaper link | ✅ | ✅ | SEP-1 toml `org_description_url` / `documentation_url` | `covered` (CG/CMC-equivalent) | `internal/metadata/sep1.go` |
| Tags / categories | ✅ | ✅ | `asset_class` taxonomy (fiat/stablecoin/crypto/rwa) + class on `/v1/sources` (exchange/oracle/aggregator/lending/router/bridge/authority_sanity) | `covered+` (richer than CG's free-form tags) | EV-0045 |
| ATH / ATL | ✅ | ✅ | `/v1/assets/{slug}` extension fields | `covered?` | rc.43..rc.46 features — needs live re-verify under cascade |
| Circulating supply | ✅ | ✅ | `/v1/assets/native` returns `circulating_supply: 500018068120000000` | `covered` | live EV-0032 |
| Total supply | ✅ | ✅ | same response: `total_supply: 500018068120000000` | `covered` | EV-0032 |
| Max supply | ✅ | ✅ | same: `max_supply: 500018068120000000` | `covered` | EV-0032 |
| Market cap | ✅ | ✅ | `/v1/assets/native` returns `market_cap_usd: 7553783305.96` | `covered` | EV-0032 |
| Fully-diluted valuation | ✅ | ✅ | `/v1/assets/native` returns `fdv_usd: 7553783305.96` | `covered` | EV-0032 |
| Sparkline | ✅ | ✅ | `/v1/assets/native` returns `price_history_24h: [{t:...}]` array | `partial` | EV-0032; only 24h, CG also offers 7d/30d/90d/1y |
| Top markets / pairs | ✅ | ✅ | `/v1/markets?limit=50` 48/50 fresh markets (F-0063 nit accepted) | `covered` | EV-0030 |
| Tickers (per-exchange) | ✅ | ✅ | `/v1/markets` data includes `source` per row | `covered` | EV-0030 + EV-0043 |
| Image / logo | ✅ | ✅ | SEP-1 `image_url` overlay | `covered` | `assets_sep1.go` |
| Holder distribution | ✅ | ✅ | not implemented | `gap` | Stellar Expert + horizon's `/accounts?asset=...` could feed this; current scope is price not holder topology |
| Top holders | ✅ | ✅ | not implemented | `gap` | same as above |
| Sentiment % up/down | ✅ | ✅ | not implemented | `non-goal` | community signal, not authoritative pricing surface |
| User ratings | ✅ | ✅ | not implemented | `non-goal` | community signal |

## Price / Market data

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Spot price (USD) | ✅ | ✅ | `/v1/price?base=native&quote=fiat:USD` | `BROKEN` (F-0060) | live 2026-05-27 returns JSON nulls HTTP 200 |
| Spot price in any fiat | ✅ | ✅ | `/v1/price?base=X&quote=fiat:Y` | `BROKEN` (F-0060) | same cascade |
| Spot price in any crypto | ✅ | ✅ | `/v1/price?base=X&quote=crypto:Y` | `BROKEN` (F-0060) | same cascade |
| 24h volume | ✅ | ✅ | `/v1/markets?base=X` aggregates | `stale 40h+` (F-0064) | live 2026-05-27 |
| 24h price change | ✅ | ✅ | (no dedicated endpoint) | `gap` (F-0062) | `/v1/changes/...` is an entity-change feed |
| 7d/30d/1y price change | ✅ | ✅ | (no dedicated endpoint) | `gap` | clients must compute from `/v1/history` |
| OHLC candles (1m..1mo) | ✅ | ✅ | `/v1/ohlc?base=native&quote=fiat:USD` | `covered` (no-data 404 contract) | live 2026-05-27 returns `errors/no-trades` |
| Historical price ranges | ✅ | ✅ | `/v1/history` | `covered (no-data)` | live 2026-05-27 returns 0 rows for today |
| TWAP / VWAP (explicit endpoint) | ❌ | ❌ | `/v1/twap` (no separate `/v1/vwap` — VWAP is the default method of `/v1/price`) | `covered+` (deeper than CG/CMC) | live 2026-05-27 `/v1/twap` returns proper `errors/no-trades` |
| Triangulated quotes | ⚠️ | ⚠️ | XLM/GBP via XLM/USD * USD/GBP | `covered?` | `internal/aggregate/triangulate.go` |
| Confidence score on price | ❌ | ❌ | `confidence` field in envelope | `covered+` | `internal/aggregate/confidence/` |
| Freeze flag on anomaly | ❌ | ❌ | `freeze: true` in envelope | `covered+` | `internal/aggregate/freeze/` |
| Divergence warning | ❌ | ❌ | `divergence_warning: true` | `covered+` | `internal/divergence/` |
| Triangulation provenance | ❌ | ❌ | `triangulated: true` + path | `covered+` | — |
| Streaming prices (WS/SSE) | ⚠️ | ⚠️ | `/v1/price/stream` | `covered?` | `internal/api/v1/price_stream.go` |

## Exchange / Pair coverage

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Major CEX coverage (Binance, Coinbase, etc.) | ✅ | ✅ | 4 CEXes: binance, coinbase, kraken, bitstamp | `covered` | EV-0045 /v1/sources lists all 4 with `class:exchange subclass:cex include_in_vwap:true` |
| Aggregator coverage (CG, CMC themselves) | ⚠️ | ⚠️ | CG + CMC as `class:aggregator include_in_vwap:false` (informational, no double-counting) | `covered+` | F-0096 methodology section; we surface them but explicitly DON'T double-count |
| Oracle coverage (Chainlink etc.) | ❌ | ❌ | 4 oracles: reflector-dex/cex/fx + redstone + band + chainlink (all `include_in_vwap:false`) | `covered+` | F-0123 (Band) + F-0124 (Redstone) + F-0044 (Chainlink) + EV-0009 (Reflector) |
| Stellar SDEX | ⚠️ | ⚠️ | `internal/sources/sdex` first-class with multi-claim fanout | `covered+` | F-0128 POSITIVE + live `/v1/pools` 35,082 24h trades |
| Stellar Soroban DEXes (Soroswap, Phoenix, Aquarius, Comet, etc.) | ❌ | ❌ | First-class — 6 sources fully audited | `covered+` | F-0117 (Phoenix) + F-0118 (Comet) + F-0119 (Soroswap) + F-0120 (Aquarius) + F-0121 (Blend 21+ events) + F-0079 (sorobanevents catch-all) |
| FX feeds (USD/EUR/GBP etc.) | ⚠️ | ⚠️ | 2 paid + 1 sanity: polygon-forex + exchangeratesapi + ecb (`class:authority_sanity`) | `covered` | EV-0045 /v1/sources |
| Bridge events (CCTP-style) | ❌ | ❌ | CCTP v2 + Rozo (`class:bridge include_in_vwap:false`) | `covered+` | F-0125 (CCTP) + F-0127 (Rozo) + WASM audits |
| Lending events | ❌ | ❌ | Blend (21+ events: money-market + admin + auctions) | `covered+` | F-0121 |
| Router events (intent-level swaps distinct from per-pair swaps) | ❌ | ❌ | Soroswap-router + DeFindex (`class:router default_weight:0`) | `covered+` (DeFindex partial per F-0018) | F-0129 (soroswap-router) + F-0018 (DeFindex 5 gaps) |

## Global / Network stats

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Global market cap (all crypto) | ✅ | ✅ | not implemented — we focus on per-asset, not global aggregation | `non-goal` | scope-limited to Stellar |
| 24h global volume | ✅ | ✅ | `/v1/network/stats volume_24h_usd: $1.04B` (Stellar-scoped) | `covered` (Stellar scope only) | EV-0031 |
| BTC dominance | ✅ | ✅ | not implemented — not Stellar-relevant | `non-goal` | — |
| Network stats (block height, ledger close time) | ❌ | ❌ | `/v1/network/stats latest_ledger + markets_count_24h + assets_indexed` | `covered+` | EV-0031 |
| Active addresses 24h | ⚠️ | ⚠️ | not implemented (would require accounts-observer aggregation) | `gap` | Stellar-specific would be `unique accounts that opened a trustline in 24h` — Wave 2 candidate |
| Tx count 24h | ⚠️ | ⚠️ | not implemented (could derive from `/v1/network/stats` extension) | `gap` | Wave 2 candidate; data is available in ledger meta |

## Account / Identity

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Free anonymous API | ✅ | ✅ | yes (with rate limit — though F-0050 fail-OPEN currently bypassable) | `covered` (with F-0050 caveat) | `internal/ratelimit/` |
| Paid tier with API key | ✅ | ✅ | yes (Stripe-billed) | `covered+` (textbook Stripe integration per F-0115) | `internal/auth/apikey.go` + F-0115 |
| API key in dashboard UI | ✅ | ✅ | `web/dashboard` Next.js 15 | `covered` | EV-0016 frontend deps modern |
| Usage metering | ✅ | ✅ | `internal/usage/counter.go` Redis-backed | `covered` (F-0050 caveat under cascade) | — |
| Webhooks | ⚠️ | ⚠️ | customerwebhook fanout + SSRF-safe dial + HMAC + exponential backoff | `covered+` | F-0006 + F-0114 POSITIVE |
| SEP-10 federation login | ❌ | ❌ | implemented but currently 503 in production per F-0093 (validator unwired) | `partial` | F-0076 (POSITIVE design) + F-0093 (config not wired) |

## Operational / Trust

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| Status page | ✅ | ✅ | `web/status` Next.js static export (status.ratesengine.net) | `covered` (F-0055 self-consistency caveat) | EV-0015 + F-0055 |
| Incident history | ✅ | ✅ | `/v1/incidents` full markdown post-mortems served publicly | `covered+` | F-0098 POSITIVE EV-0045 |
| Methodology disclosure | ✅ | ✅ | `/v1/methodology` with ADR references | `covered+` | F-0096 POSITIVE |
| SLA disclosure | ⚠️ | ⚠️ | `cmd/ratesengine-sla-probe` + `docs/operations/sla-probe.md` | `covered` | binary ships + doc exists |
| Transparent source list | ⚠️ | ⚠️ | `/v1/sources` exposes 26 sources with full classification | `covered+` | F-0097 POSITIVE |

## Developer

| Feature | CG | CMC | Us | Status | Evidence |
| --- | --- | --- | --- | --- | --- |
| OpenAPI spec | ⚠️ | ⚠️ | `openapi/rates-engine.v1.yaml` source-of-truth + docs-lint round-trip | `covered+` | F-0113 (docs-lint enforces openapi↔handlers) |
| Postman collection | ✅ | ✅ | `examples/postman/` auto-generated | `covered` | — |
| curl examples | ✅ | ✅ | `examples/curl/` | `covered` | — |
| SDK | ✅ (multiple langs) | ✅ | `pkg/client/` Go SDK (single language) | `partial` | Go-only; CG/CMC ship multiple languages |
| Webhook signing | ⚠️ | ⚠️ | customer webhooks signed HMAC-SHA256 (F-0006 SSRF-safe) | `covered+` | F-0006 POSITIVE; payload signing per `internal/customerwebhook/` |

## Audit pass output

Each `?` cell above must be resolved to one of `covered` /
`partial` / `gap` / `non-goal` / `n/a` before the matrix is
closed. `partial` and `gap` cells generate findings.

Where we ship `covered+` (deeper than CG/CMC), that's a launch
narrative win and should appear in the Stellar coverage matrix
([09-stellar-coverage-matrix.md](09-stellar-coverage-matrix.md))
as an explicit moat.
