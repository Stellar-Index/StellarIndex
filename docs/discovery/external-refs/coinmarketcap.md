# CoinMarketCap

**Status:** ⚠️ Reference / cross-check source. Same role as
[coingecko.md](coingecko.md).

**Related:** existing connector `rate_source_coinmarketcap.go`.

## API surface (verified from existing connector)

From `rate_source_coinmarketcap.go`:

| Endpoint | Purpose | Auth |
| -------- | ------- | ---- |
| `https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest?CMC_PRO_API_KEY=<K>&slug=dash&convert=AED` | Per-asset per-convert-currency quote | **API key required** (Pro tier) |

Also referenced but disabled:
`https://api.coinmarketcap.com/v1/ticker/dash/?convert=JOD` — this
is the **old free public API, shut down around 2018**. The existing
system has a comment pointing to it but actual calls go to
`pro-api.coinmarketcap.com` with the Pro API key.

### Additional relevant endpoints

- `/v1/cryptocurrency/quotes/latest?id=<ids>&convert=<currencies>` —
  batch version. Multiple coin IDs in a single call.
- `/v1/cryptocurrency/listings/latest?limit=N` — top-N by market
  cap. Useful for asset-discovery.
- `/v1/cryptocurrency/ohlcv/historical?id=<id>&time_period=daily&time_start=<ISO>&time_end=<ISO>`
  — historical OHLC. **Tier-gated — not on free/basic.**
- `/v1/cryptocurrency/market-pairs/latest?id=<id>` — per-asset
  per-venue pair listings. Similar to CoinGecko `/coins/{id}/tickers`.

## Tier structure

CMC's Pro API tier structure (public info, verify current pricing):

| Tier | Monthly calls | Historical OHLC | Commercial use |
| ---- | ------------- | --------------- | -------------- |
| Free | 10,000 | ✗ | ✗ |
| Hobbyist | 40,000 | minimal | ✗ |
| Startup | 120,000 | ✓ (2y) | limited |
| Standard | 500,000 | ✓ | ✓ |
| Professional | 3M | ✓ | ✓ |
| Enterprise | custom | ✓ | ✓ |

For our cross-check use (batched quote calls every minute for ~50
assets × 10 fiat currencies = 500 "credits" per call per minute),
we need at minimum the Standard tier if we're hitting it live.

## Auth

The existing system uses the `CMC_PRO_API_KEY` query parameter.
Header form is also supported (`X-CMC_PRO_API_KEY`). Header is
preferred to avoid leaking the key in URLs / logs.

## XLM / Stellar coverage

CoinMarketCap has Stellar (XLM) with ID `512` and most Stellar-
listed tokens (AQUA, yUSDC, etc.) with dedicated IDs. Mapping table
is bigger than CoinGecko's but similarly asset-issuer-agnostic —
CMC lists `USDC` as one entry across all chains, not per-issuer.

## Licensing

Commercial-use clauses are tier-dependent. Startup tier and above
permit commercial redistribution with attribution. **Confirm in
writing** before we ship our API with CMC values attributed.

## Role

Same as CoinGecko: **divergence detector**, not VWAP contributor.

Having two independent aggregators (CoinGecko + CMC) gives us
redundancy in the cross-check layer. If both agree that our
aggregated price is within tolerance, we're probably good. If
they disagree with each other, that's its own signal (the
reference layer is unreliable for that moment).

## Open items

- [ ] Pick a tier (likely Standard or Professional) based on
      projected call volume.
- [ ] Build the Stellar-asset → CMC-ID mapping table.
- [ ] Decide whether CMC or CoinGecko is primary cross-check and
      which is secondary, or whether we use both equally.

## Related

- [coingecko.md](coingecko.md) — sibling aggregator.
- [../oracles/chainlink.md](../oracles/chainlink.md) — same
  divergence-detector role.
