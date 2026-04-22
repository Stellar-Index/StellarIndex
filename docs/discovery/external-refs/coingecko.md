# CoinGecko

**Status:** ⚠️ Reference / cross-check source. Not a VWAP contributor.

**Related:** existing connector `rate_source_coingecko.go`.

## RFP context

Proposal, §"Industry Reference Providers":

> "Reference pricing from industry aggregators such as CoinGecko and
> CoinMarketCap is ingested for validation and anomaly detection.
> These sources are not treated as primary trade feeds. Instead,
> they provide cross-check signals when on-chain or exchange-derived
> prices diverge materially."

So CoinGecko is in the divergence-detector category, same as
Chainlink HTTP feeds. We poll periodically, compare to our
aggregated prices, flag > threshold divergences.

## API surface (verified from existing connector)

From `rate_source_coingecko.go`:

| Endpoint | Purpose | Auth |
| -------- | ------- | ---- |
| `https://api.coingecko.com/api/v3/exchange_rates` | Global fiat + crypto rates vs. BTC | Public (free tier) |
| `https://api.coingecko.com/api/v3/coins/{id}?tickers=false&market_data=true&community_data=false&developer_data=false&sparkline=false` | Per-coin market data | Public |

### Additional endpoints relevant to us but not in the existing code

- `/api/v3/simple/price?ids=stellar,usd-coin,lumens,…&vs_currencies=usd,eur,gbp,jpy,btc,eth`
  — multi-coin multi-currency price fetch. **Ideal for our batch-
  validation call.**
- `/api/v3/coins/{id}/market_chart?vs_currency=usd&days=365` —
  historical OHLC (hourly granularity for ≤90 days, daily for
  more). **Useful for since-inception historical XLM / USDC
  reference prices.**
- `/api/v3/coins/{id}/tickers` — per-coin per-venue tickers. Shows
  us which CEXes trade a given asset and at what current price.
  **Useful for venue discovery**.
- `/api/v3/coins/{id}/ohlc?vs_currency=usd&days=30` — candlestick
  data, daily / hourly per `days`.

## Rate limits (known publicly)

- **Free (Demo) tier**: ~10-30 calls/min, unauthenticated.
- **Analyst tier**: paid, ~500 calls/min.
- **Lite / Pro tier**: higher; ask for pricing.

Free tier is enough for validation polling if we batch symbols.
A `simple/price` call with 50 coin IDs + 10 currencies is one call
every minute = well inside free. For historical backfill of years
of hourly data at a pair-level we'd need the paid tier.

## Coin-ID mapping (relevant to Stellar)

The existing connector (`rate_source_coingecko.go:73`) uses:

```go
"Lumens": Currency("XLM")    // CoinGecko ID -> our ticker
```

More broadly, CoinGecko uses their own slug IDs (`stellar`,
`usd-coin`, `bitcoin`, `ethereum`, etc.) — not Stellar-native
identifiers. For classic Stellar assets (e.g. specific
`USDC:GA5Z…` vs other `USDC` issuers) CoinGecko just has one
`usd-coin` entry covering the Circle issuance across all chains.

**Implication**: CoinGecko can cross-check `XLM/USD`, `USDC/USD`,
`BTC/USD`, `ETH/USD`, and a handful of other cross-chain tokens.
It **cannot** price most Stellar-native classic assets (AQUA,
small Stellar token issuances) — those we rely on SDEX / Soroban
DEX data only.

## XLM coverage in the existing connector

From `rate_source_coingecko.go:160`:

```go
coins := []Currency{DASH, XLM, USDC}
```

Existing system fetches XLM + USDC from CoinGecko. We preserve
at minimum that set and extend with Stellar-native assets that
CoinGecko lists (AQUA, yXLM, yUSDC, etc. — verify per-asset).

## Licensing

CoinGecko's free tier allows non-commercial usage. Paid tiers
explicitly allow redistribution in dashboards / apps. Our use
case (exposing as a divergence signal in our public API, with
attribution to CoinGecko) almost certainly requires their paid
tier. Verify terms before production.

## Role in our aggregation

**Divergence detector, not VWAP contributor** — identical role to
Chainlink HTTP feeds:

```
For each major asset (XLM, USDC, BTC, ETH, USD):
    our_price = aggregated_from_on_chain + CEX sources
    coingecko_price = fetch from /simple/price
    if abs(our_price - coingecko_price) / coingecko_price > 2%:
        flag in API response: "cross_check_divergent: true"
        log with source breakdown
    if > 5%:
        drop divergent sources from this window, alert ops
```

Specific thresholds tunable per asset; 2%/5% are placeholders.

## Historical data for since-inception OHLC

The RFP requires "since inception" XLM / USDC / etc. daily OHLC.
CoinGecko's `/market_chart` endpoint can cover this:

- XLM daily data back to ~2014 (Stellar launch).
- USDC daily data back to ~2018 (Circle launch).

We fetch this once as a **backfill seed** and never call it again
for historical windows — our own pipeline produces OHLC from
ledger 2 onwards via Galexie data-lake replay. CoinGecko fills
the gap only for the **off-chain CEX portion** of the historical
picture.

## Open items

- [ ] Confirm current CoinGecko free-tier rate limits for our
      intended use pattern (`simple/price` every 60 s with ~50
      coin IDs).
- [ ] Decide whether we pay for an Analyst or Lite tier for
      licensing + redistribution rights.
- [ ] Produce the Stellar-asset → CoinGecko-slug mapping table
      for every Stellar asset we care about. Maintain as a
      configurable file.
- [ ] Settle the divergence thresholds (2% flag / 5% drop) via
      historical data analysis.

## Related

- [coinmarketcap.md](coinmarketcap.md) — the other main reference
  aggregator.
- [../oracles/chainlink.md](../oracles/chainlink.md) — same
  divergence-detector role.
- [../existing-ctx-rates.md](../existing-ctx-rates.md) — existing
  connector as vendor-spec reference.
