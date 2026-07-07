# Pricing methodology

Public, customer-facing documentation of how Stellar Index turns raw
on-chain and off-chain trades into the prices served by the API. These
pages are written to be **honest about what a number means** — including
where a value is undefined, structurally delayed, or deliberately
excluded.

| Page | Covers |
|---|---|
| [vwap-aggregation.md](vwap-aggregation.md) | VWAP computation, the source-class policy (only exchange trades vote), stablecoin fiat-proxy late-binding, σ-outlier filtering, triangulation, closed-bucket serving, and the two freshness contracts (`/v1/price/tip` ≤5s vs `/v1/price` 30–150s) |
| [twap-ohlc.md](twap-ohlc.md) | TWAP + OHLC computation and the "no trades in window" contract (404, not a fabricated/LKG value) |
| [xlm-circulating-supply.md](xlm-circulating-supply.md) | How XLM `circulating_supply` / `market_cap_usd` are computed (total − SDF non-circulating holdings), and the live reconciliation showing 0.03% agreement with CoinGecko + the Stellar Network Dashboard |

Related, non-public references:

- `docs/architecture/aggregation-plan.md` — the internal binding spec
  (policy chain, config surface, metrics, alerts).
- `docs/architecture/freshness-definition.md` — the two-contract
  freshness design.
- [Per-protocol verification pages](../protocols/README.md) — which
  sources feed the exchange class, and the contract-identity gating
  (ADR-0035) that makes each trade trustworthy.
