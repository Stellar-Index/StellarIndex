# Centralized Exchange feeds

**Status:** 🧪 **Planning** — drawn from the existing CTX Rates
(`~/code/rates`) connector set and from RFP requirements. No
greenfield code yet.

**Related:** [../existing-ctx-rates.md](../existing-ctx-rates.md)
is the reference implementation we read this from. Every connector
described below exists as production-tested Go in that codebase.

## RFP context

Our proposal, §"Centralized Exchanges":

> "Trade and ticker data are ingested from selected centralized
> exchanges using WebSocket streams where available, with REST
> polling as a fallback. Only exchanges that meet defined liquidity,
> reliability, and API stability criteria are included.
>
> CEX data provides deep liquidity references for major pairs,
> improves price stability for thin on-chain markets, and enables
> cross-asset triangulation when direct pairs are unavailable."

So CEX data is **input to VWAP** — not validation-only. It
contributes liquidity weight to our aggregation alongside on-chain
DEX trades.

## The 7 live CEX connectors in the existing codebase

Verified endpoints, auth, and transport from the existing
`rate_source_*.go` files:

| Exchange | REST | WebSocket | Auth | Source file |
| -------- | ---- | --------- | ---- | ----------- |
| **Binance** | `https://api.binance.com/api/v1/exchangeInfo` | `wss://stream.binance.com:9443/ws/!ticker@arr` | public | `rate_source_binance.go` |
| **Bitfinex** | `https://api.bitfinex.com/v1/symbols` | via `bitfinexcom/bitfinex-api-go/v2/websocket` | public | `rate_source_bitfinex.go` |
| **Bitstamp** | `https://www.bitstamp.net/api/v2/trading-pairs-info/` | raw WS (`gorilla/websocket`) | public | `rate_source_bitstamp.go` |
| **Kraken** | `https://api.kraken.com/0/public/AssetPairs` | raw WS | public | `rate_source_kraken.go` |
| **HitBTC** | `https://api.hitbtc.com/api/2/public/symbol` | raw WS | public | `rate_source_hitbtc.go` |
| **Huobi / HTX** | `https://api.huobi.pro/v1/common/symbols` | raw WS | public | `rate_source_huobi.go` (rebrand risk — see below) |
| **Poloniex** | `https://poloniex.com/public?command=returnTicker` | raw WS | public | `rate_source_poloniex.go` |

### XLM pair coverage in the existing connectors

Pulled from the existing source files:

- **Binance**: `XLMUSDT`, `XLMEUR`, `XLMBTC`, `XLMETH` (four pairs,
  from `rate_source_binance.go:63-67`).
- **Bitstamp**: `XLMBTC`, `XLMUSD`, `XLMEUR`, `XLMGBP` (four pairs).
- **Kraken**: `XLM/AUD`, `XLM/CAD`, `XLM/CHF`, `XLM/EUR`, `XLM/GBP`,
  plus others (partial grep).
- **Bitfinex / HitBTC / Huobi / Poloniex**: auto-enumerate pairs via
  their REST `symbols` endpoint. XLM coverage depends on
  current-day listing at each venue — need to verify live.

For Day-1 coverage parity we preserve at minimum the XLM pairs the
existing system fetches.

## Liveness risk — confirmed dead and suspected broken

### Confirmed dead

- **`bitcoinAverage`** — service shut down circa 2021. Already
  commented out of the existing registry.
- **`localbitcoins`** — service shut down **February 2023**. Still
  in the existing registry. Will error every reconnect cycle.
  Remove from our new system.

### Probable rebrand / API churn

- **Huobi → HTX** — Huobi rebranded as **HTX** in 2023; their API
  base URL moved. The existing connector hits `api.huobi.pro`,
  which may still redirect (the old domain is often kept alive
  during a rebrand) but we should target `api.htx.com` in the new
  implementation.
- **Poloniex** — alive but has changed API keys / endpoints
  multiple times since the existing connector was written.
  Verify the `returnTicker` command still works; the v2 API is
  what's current.

### Low-volume / niche (include if useful, easy to cut)

- `bitfinex`, `bitstamp`, `hitbtc`, `kraken` — all still operating,
  still serving public WS. Low risk.

## CEXes from the RFP worth adding

Our proposal says "selected centralized exchanges" without naming
them. For Day-1 coverage of major Stellar-listed assets we should
include at least:

- **Binance** — largest global volume; XLM is listed.
- **Coinbase** (`wss://ws-feed.exchange.coinbase.com`) — the
  existing system does **not** integrate Coinbase. We should add it.
  XLM-USD on Coinbase is meaningful US-market price discovery.
- **Kraken** — strong XLM-EUR/GBP/CAD/AUD coverage.
- **Bitstamp** — another strong XLM venue.
- **OKX / Bybit / KuCoin** — large Asian volume; depending on
  Stellar-asset listings may add coverage for specific tokens.
- **LOBSTR** — Stellar-native CEX-like front-end, mostly SDEX-
  backed, but may offer additional market data.

## Provider selection criteria (derived from proposal)

Per the proposal, a venue is included only if:

- Defined **minimum USD volume** (per window).
- **API stability** (no undocumented changes breaking our
  connectors).
- **Reliability** (uptime track record).

Our Phase-2 audit of each candidate will include:

- 30-day uptime on their public WS, sampled.
- Self-reported daily volume per XLM pair, sanity-checked against
  CoinGecko / CoinMarketCap aggregates.
- Historical data availability (do they offer trades-since-
  timestamp over REST?).

## Transport strategy

The existing codebase already uses the right patterns:

- **WebSocket first** — every major exchange above exposes a
  public WS stream of trades or tickers. That's our live feed.
- **REST fallback** — for the initial pair list, for reconnect
  after a WS drop, and for historical backfill.
- **Per-source goroutine + channel** pattern (from
  `rate_source.go:81-108` in the existing system).

What the new system adds:

- **Persistent cursor** per venue — the current system loses
  trades on reconnect; we preserve our last-seen seq and
  reconcile on reconnect (similar to how we plan to handle
  stellar-rpc event reconnect).
- **Per-trade storage** (not just ticker) — the existing system
  aggregates to a single latest ticker; we persist individual
  trades to compute VWAP over rolling windows.
- **Prometheus metrics** per source (`cex_trades_total{source=…}`,
  `cex_ws_reconnects_total`, `cex_last_trade_lag_seconds`).

## Historical backfill

For since-inception OHLC we need historical CEX trades per pair.
Options per venue:

- **Binance** — REST `/api/v3/aggTrades` gives trades by time
  range. Capped to 1000 per call; paginate.
- **Kraken** — REST `/0/public/Trades` with `since` parameter.
- **Bitstamp** — REST `/v2/transactions/<pair>/` with a time-window.
  Limited history.
- **Bitfinex** — REST `/v2/trades/<symbol>/hist` with a ms
  timestamp range.

Many venues only keep months, not years, of trade history over REST.
For genuinely since-inception we also lean on **CoinGecko market_chart**
and **CoinMarketCap historical** (which aggregate multiple venues);
raw CEX trade history reach varies wildly.

## i128 / precision

CEX prices are typically `float64` strings or decimal strings in the
JSON responses. We parse them as `decimal.Decimal` and store as
`NUMERIC`. No i128 issue here — but our **pipeline must normalise
early** so classic CEX doubles never mix with Soroban i128 in the
same math.

## Open items (Phase 2)

- [ ] Enumerate the exact XLM and Stellar-token pairs currently
      listed at each target venue (Binance, Coinbase, Kraken,
      Bitstamp, etc.). Catalogue assetA-assetB + quote currency.
- [ ] Audit each existing connector's liveness against its current
      vendor API. Produce a "works / 4xx / 5xx / dead" table.
- [ ] Decide which non-live providers from the existing set are
      worth reviving (`bitfinex`, `hitbtc`, `huobi-rebranded`) vs
      cutting entirely.
- [ ] Decide whether to use a cross-venue library like `ccxt`
      (Python) or **roll our own** per-venue connectors in Go.
      Existing system rolls its own — we likely continue that.
- [ ] Capacity plan: if we subscribe to `!ticker@arr` on Binance
      we get every pair at 1s cadence. Backpressure / filter
      design.

## Related

- Existing connectors: [../existing-ctx-rates.md](../existing-ctx-rates.md)
- Non-CEX reference feeds: [coingecko.md](coingecko.md),
  [coinmarketcap.md](coinmarketcap.md).
- FX-only feeds (for fiat-denominated pairs): [fx-feeds.md](fx-feeds.md).
