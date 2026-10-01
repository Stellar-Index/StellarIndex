---
title: Data-licensing register — every third-party feed and the clause that governs it
last_verified: 2026-09-30
status: living doc
---

# Data-licensing register

This page lists every off-chain third-party feed the code fetches, what we keep from it, where it
reaches a reader, and the vendor clause that governs that use. On-chain venues and oracles read from
the ledger are not listed; processors (hosting, CDN, email) are not feeds. The rule: **adding a
feed = adding a row**, in the same PR as the connector. Risk reads as: **High** = we serve or display
data today (or would the moment the flag flips) against a clause that bars it; **Med** = conditional
or unread terms; **Low** = public/open data or internal use only. "Prod" = the r1 Ansible template.

| Feed | Code path | What we ingest | Where it is served | Key/tier (config key) | Terms URL (fetched YYYY-MM-DD) | Governing clause (quoted) | Redistribution | Attribution required | Risk + why | Action before launch |
|---|---|---|---|---|---|---|---|---|---|---|
| Binance | `internal/sources/external/binance` (WS `aggTrade`, REST `/api/v3/klines`) | per-trade price/amount/ts; candles as synthetic trades → `trades` | raw: `/v1/observations`, `/v1/history`, `/v1/markets[/sources]` (incl. `source=binance`); VWAP input; `/exchanges` page (per-venue pairs + chart) | `external.binance.enabled` (prod on); no key | binance.com/en/terms (2026-09-30) | unfetchable (empty 202 / no terms text) | unknown | unknown | **High** — raw source-attributed rows served; terms unread | Human read of Binance terms; INV-1198 accepted the risk for now |
| Coinbase | `internal/sources/external/coinbase` (WS `matches`, REST `/products/{p}/candles`) | as Binance | as Binance | `external.coinbase.enabled` (prod on); no key | coinbase.com/legal/market_data (2026-09-30) | unfetchable (HTTP 403) | unknown | unknown | **High** — raw rows served; market-data terms unread | Human read of the market-data terms |
| Kraken | `internal/sources/external/kraken` (WS v2 `trade`, REST `/0/public/OHLC`, `/Trades`) | as Binance, incl. 946 days of history fills | as Binance | `external.kraken.enabled` (prod on); no key | docs-legacy.kraken.com/api/docs/guides/global-intro/ (2026-09-30) | "You must seek our prior permission for certain uses … any non-personal commercial use of data from publicly accessible endpoints" | needs permission | not stated | **High** — commercial use of public endpoints needs prior permission | Ask marketdata@kraken.com |
| Bitstamp | `internal/sources/external/bitstamp` (WS `live_trades`, REST `/api/v2/ohlc`) | as Binance | as Binance | `external.bitstamp.enabled` (prod on); no key | bitstamp.net/api/ (2026-09-30) | "Bitstamp allows the incorporation and redistribution of our exchange data for commercial purposes"; users must "receive and sign a commercial use Data License Agreement" | yes, with signed DLA | not stated | **Med** — allowed, but no DLA signed | Sign DLA via partners@bitstamp.net |
| CoinGecko | `internal/sources/external/coingecko`; `internal/divergence/{coingecko,supply}.go`; `internal/ops/ingest/listing_sync.go` | USD/fiat price per ticker → `oracle_updates`; listing price → `asset_listing_directory` | beside other sources: unfiltered `/v1/oracle/latest` (`source=coingecko` returns 400); `reference`/`valuation` on `/v1/assets`; `/v1/divergence`; `/aggregators`, `/oracles` pages | `external.coingecko.{api_key,demo_api_key}` (env `COINGECKO_API_KEY`, `COINGECKO_DEMO_API_KEY`); Pro host iff Pro key set (prod on) | coingecko.com/en/api_terms (2026-09-30) | cl. 4.1.6 "You are not permitted to sell, rent, lease, sub-license, re-distribute or syndicate access to the CoinGecko API or part thereof" | Enterprise only | yes — "Powered by CoinGecko" | **High** — raw prices re-served through our API | Buy Enterprise or drop the remaining surfaces; add attribution |
| CoinMarketCap | `internal/sources/external/coinmarketcap` (`/v2/cryptocurrency/quotes/latest`) | price per ticker → `oracle_updates` | beside other sources: unfiltered `/v1/oracle/latest` when enabled (`source=coinmarketcap` returns 400) | `external.coinmarketcap.api_key` (env `COINMARKETCAP_API_KEY`); paid host only (prod off) | coinmarketcap.com/api/pricing/ (2026-09-30) | "you may not redistribute or resell it as a standalone service, whether through your own API or as part of a data distribution product" | Enterprise only | not found | **Med** — off in prod; enabling would breach | Keep off, or exclude from `/v1/oracle/*` |
| CryptoCompare (CCData) | `internal/sources/external/cryptocompare` (`/data/pricemultifull`) | price per ticker → `oracle_updates` | beside other sources: unfiltered `/v1/oracle/latest` when enabled (`source=cryptocompare` returns 400) | `external.cryptocompare.api_key` (env `CRYPTOCOMPARE_API_KEY`), optional (prod off) | cryptocompare.com/api-licence-agreement (2026-09-30) | cl. 2.4: not "for Display purposes"; not "for developing, reproducing, or distributing to Customers or any other person, any Products" | negotiated only | yes — "Attribution Requirements" | **Med** — off in prod; licence bars display at any tier | Keep off |
| Chainlink (Ethereum RPC) | `internal/sources/external/chainlink`; `internal/divergence/chainlink.go` (`eth_call latestRoundData`) | feed answer + decimals → `oracle_updates`, `divergence_observations` | beside other sources: unfiltered `/v1/oracle/latest` (`source=chainlink` returns 400); `ref_price` on `/v1/divergence`, `/v1/anomalies` | `external.chainlink.rpc_url`, `divergence.chainlink.rpc_url` (env `CHAINLINK_RPC_URL`); default public RPC (prod on) | chain.link/terms (2026-09-30) | not found (page carries no data-feed licence text) | not stated | not stated | **Low** — public on-chain values | Human read of Chainlink data-feed terms |
| Tiingo (fund NAV: WisdomTree and other mutual-fund tickers) | `internal/sources/external/tiingo`; bindings `internal/rwa/oracle_reference.go` | daily NAV close for 12 fund tickers → `oracle_updates` (`raw:<TICKER>`) | `reference` block on `/v1/rwa/assets`; `/rwa` page | `external.tiingo.api_key` (env `TIINGO_API_KEY`) (prod off) | app.tiingo.com/tos/ (2026-09-30) | "All data via the API is for internal consumption only"; "Redistribution is only available upon special request and permission, and comes with additional fees" | paid licence | yes — "Data sourced by Tiingo" | **High** — the RWA reference surface displays it once enabled | Buy a redistribution licence before enabling |
| Massive (ex-Polygon.io) FX | `internal/sources/external/forex/{client,worker}.go` (`/v2/aggs/grouped/.../fx/{date}`) | daily FX rate per ticker → `fx_quotes` | derived: fiat rows of `/v1/assets`, fiat:fiat `/v1/chart`, fiat cross in `/v1/price` | `external.massive.api_key` (env `MASSIVE_API_KEY`); paid; no enable flag (always on) | massive.com/legal/businesses-terms-of-service (2026-09-30) | "use, redistribute, … display, disseminate … or otherwise make available any portion of the Information to anyone other than Customer" | not permitted | not found | **High** — FX rates reach every fiat price we publish | License display/redistribution or move serving to ECB |
| ECB reference rates | `internal/sources/external/ecb`; `forex/fallback.go` (`eurofxref-daily.xml`) | daily EUR reference rates → `oracle_updates`, `fx_quotes` | beside other sources: unfiltered `/v1/oracle/latest` (`source=ecb` returns 400); fiat fallback | `external.ecb.enabled` (prod on); fallback always on; keyless | ecb.europa.eu/services/disclaimer (2026-09-30) | "When such information is distributed or reproduced, it must appear accurately and the ECB must be cited as the source." | yes | yes — cite ECB | **Low** — free reuse with citation | Add ECB citation where fiat rates appear |
| exchangeratesapi.io | `internal/sources/external/exchangeratesapi` (`/v1/latest`) | FX rates → `oracle_updates` | beside other sources: unfiltered `/v1/oracle/latest` when enabled (`source=exchangeratesapi` returns 400) | `external.exchangeratesapi.api_key` (env `EXCHANGERATESAPI_KEY`) (prod off) | exchangeratesapi.io/agreement/ (2026-09-30) | "Customer shall not reproduce the SaaS Services or the Licensed Material … of Apilayer, except as provided in this Agreement" | not stated | not found | **Low** — off in prod | Re-read before enabling |
| Open Exchange Rates | `internal/sources/external/forex/openexchangerates.go` (`/latest.json`) | nothing stored; corroborator only | not served | `external.openexchangerates.{enabled,app_id}` (env `OPENEXCHANGERATES_APP_ID`) (prod off) | openexchangerates.org/terms (2026-09-30) | "You must not profit directly from the sale or provision of information obtained on or through our site … without obtaining a licence" | licence needed | not found | **Low** — internal cross-check only | Keep internal; re-read if ever served |
| Frankfurter | `internal/sources/external/frankfurter`; `scripts/ops/fx-history-backfill` | FX history to 1999 → `fx_quotes` (one-off) | derived: fiat history in `/v1/chart` | none; keyless | frankfurter.dev (2026-09-30) | "The rates themselves fall under each provider's terms" (code MIT; data from ECB) | per ECB | yes — cite ECB | **Low** — ECB data | Covered by the ECB citation |
| Dune (public query results) | `internal/ops/ingest/curated_rwa_sync.go` (`/api/v1/query/{id}/results`) | curated monthly RWA series → `curated_rwa_published_series` | `curated.published` on `/v1/rwa/assets`; `/rwa` page | `external.dune.api_key` (env `DUNE_API_KEY`); ops job | dune.com/terms (2026-09-30) | not found (no clause on reuse of query results) | not stated | not stated | **Med** — displayed; rights sit with the query's curator | Get curator consent; credit them |
| StellarExpert directory | `internal/ops/ingest/directory_sync.go` (GitHub tarball `stellar-expert/public-directory`) | account labels, scam flags → `account_directory` | `/v1/directory`; scam gating on assets/issuers | `-url`, `-sha256` flags; keyless | github.com/stellar-expert/public-directory (2026-09-30) | licence: MIT | yes | MIT notice | **Low** — MIT | Keep the MIT notice with the copy |
| StellarExpert API | `internal/ops/chops/verify_served_values.go` (`/explorer/public/asset/{id}`) | supply, compared in memory | not served (ops cross-check) | none; keyless | stellar.expert/openapi (2026-09-30) | "publicly available for developers, free of charge" | n/a | not found | **Low** — internal only | None |
| SDF dashboard lumens API | `internal/divergence/supply.go` (`dashboard.stellar.org/api/v3/lumens`) | XLM circulating supply → divergence check | not served (internal reference) | `divergence.supply.dashboard.{enabled,base_url}`; keyless | github.com/stellar/dashboard (2026-09-30) | not found (repo has no licence) | n/a | not stated | **Low** — internal only | None |
| SEP-1 `stellar.toml` (issuer class) | `internal/metadata/sep1.go`; `stellarindex-ops sep1-refresh` | org name, currency name/description/conditions/image URL → `issuers.sep1_payload` | `/v1/assets/{id}[/metadata]`, `/v1/issuers[/{g}]`, `/v1/rwa/assets`; icons hot-linked from issuer hosts | `[metadata]` `issuer_home_domains`; keyless | SEP-0001 spec (2026-09-30) | "The `stellar.toml` file is used to provide a common place where the Internet can find information about your organization's Stellar integration." | published for this purpose | not stated | **Low** — issuer-published for discovery | None; keep icons hot-linked, not re-hosted |
| Stellar history archives | `internal/archivecompleteness/cross_anchor_fill.go`; `internal/ops/archive/verify_archive.go` | ledger checkpoints (SDF, LOBSTR, publicnode, Franklin Templeton, others) | not served; `/v1/diagnostics/archive` reports state only | `stellar.history_archive_url`; keyless | n/a — public network data | not applicable | n/a | n/a | **Low** — public ledger | None |
| AWS Public Blockchain Data | `internal/pipeline/coldstore.go` (cold tier) | Stellar ledger meta, when cold tiering is on | not served directly | `storage.s3_cold_*` (off by default) | registry.opendata.aws/aws-public-blockchain (2026-09-30) | "AWS Public Blockchain Data was accessed on `DATE` from https://registry.opendata.aws/aws-public-blockchain." | not stated | citation requested | **Low** — open data, off by default | Cite if enabled |

## Open questions for the owner

- **CEX raw rows (Binance, Coinbase, Kraken, Bitstamp).** INV-1198 kept the raw source-attributed
  rows as an accepted risk, and the owner decided exchange venues stay selectable with `source=`
  (`/v1/markets?source=<cex>` is the only way to list a venue's pairs); data vendors stay refused. Kraken asks for prior permission (marketdata@kraken.com); Bitstamp
  requires a signed commercial Data License Agreement (partners@bitstamp.net). Neither publishes a
  price. Binance's and Coinbase's terms pages could not be read by machine and need a human read.
- **CoinGecko.** Unfiltered `/v1/oracle/latest` (beside other sources; `source=coingecko`
  returns 400) and the `/v1/assets` listing price re-serve CoinGecko data through our API, which
  cl. 4.1.6 routes to an Enterprise agreement at a custom price. Self-serve Analyst ($129/mo)
  covers display with attribution only. Drop those surfaces, or license them?
- **Massive FX.** Every fiat price we publish is derived from Massive rates. The business terms
  bar display to anyone outside the customer, and derivative works "unless licensed". No business or
  redistribution price is published. License it, or make ECB (free with citation) the serving source?
- **Tiingo fund NAV (off in prod).** The terms say data is "for internal consumption only", with
  redistribution licensed by request "with additional fees". No price is published. This has to be
  settled before `external.tiingo` is enabled for the `/v1/rwa/assets` reference block.
- **Dune curated series.** Dune's terms say nothing about reuse of query results. The rights sit with
  the query's curator. Get their consent and credit them?
- **CoinMarketCap / CCData (both off).** CMC's commercial tiers ($79–$699/mo, one product, ≤100k
  users) exclude serving through our own API; CCData's licence excludes display at any price. Keep
  both off, or exclude them from `/v1/oracle/*` before anyone enables them.
- **Attribution not yet shown.** "Powered by CoinGecko", ECB cited as source, and "Data sourced by
  Tiingo" once it is on. Where should they appear: the terms page, a sources page, or per panel?
- **Pre-2017 external history (INV-1215).** Every vendor examined routes serving through our API to
  a negotiated licence; see
  [history-completeness-plan §9.6](history-completeness-plan.md#96-the-paid-aggregators).

## Related

- Terms of service §4, third-party feeds: `web/explorer/src/app/terms/page.tsx` ("A venue's own
  terms may limit your reuse of raw per-venue observations").
- INV-0933 — legal/vendor review (CEX redistribution, CoinGecko terms, SBOM licensing).
- INV-1198 — gating raw source-attributed CEX endpoints; decided 2026-09-30: keep, accepted risk.
- [history-completeness-plan §9.5–9.6](history-completeness-plan.md#95-free-and-open-sources--every-one-fails-on-terms)
  — vendor terms for historical XLM data, quoted in full.
- [add-cex-connector procedure](../contributing/procedures/add-cex-connector.md) — the vendor
  ToS note every new connector must satisfy.
