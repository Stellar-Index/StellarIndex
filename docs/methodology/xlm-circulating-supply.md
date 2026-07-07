---
title: XLM circulating-supply methodology
last_verified: 2026-07-07
status: current
---

# How Stellar Index computes XLM circulating supply

This is the public methodology for the native-lumen supply figures
served on `/v1/assets/native` (`total_supply`, `circulating_supply`,
`max_supply`, and the `market_cap_usd` derived from them). It exists
because the circulating figure is the single most-viewed number on the
explorer and is periodically questioned against CoinGecko / the Stellar
Network Dashboard — this page records exactly what the number means and
shows the live reconciliation that proves it is market-comparable.

## The formula (Algorithm 1, ADR-0011 §1)

```
total_supply       = 50,001,806,812 XLM        (frozen constant, see below)
circulating_supply = total_supply − Σ(SDF non-circulating account balances)
max_supply         = total_supply              (XLM is hard-capped)
market_cap_usd     = circulating_supply × VWAP price
```

`supply_basis` on the response is `xlm_sdf_reserve_exclusion` when the
exclusion is applied (the production state), or `xlm_total_only` if no
reserve accounts are configured (an honest self-evident misconfig
signal — we never label a non-exclusion as an exclusion).

### `total_supply` is the frozen post-2019 figure, NOT on-chain `total_coins`

The on-chain `total_coins` ledger field is ~105.4B lumens and is
reproduced faithfully by the explorer's ledger view. The **50,001,806,812**
supply constant is the post-inflation-vote total the whole market uses:
50 B genesis lumens + the inflation pool, frozen by network vote in
October 2019. `total_coins` was never lowered on-chain by that vote, so
the two figures legitimately differ. Total matches CoinGecko to 5
significant figures.

### The exclusion set = the SDF non-circulating holdings

`circulating` subtracts the balances of the accounts the Stellar
Development Foundation controls and has not yet distributed — the same
set the Stellar Network Dashboard's `lumens.js` treats as
non-circulating (SDF Mandate + Upgrade Reserve). The account list is
network-wide truth, identical for every region, and lives in
`configs/ansible/.../defaults/main.yml` as `stellarindex_sdf_reserve_accounts`
(16 accounts: 15 mandate + the single Upgrade-Reserve account). It is
config, not on-chain-derivable — SDF publishes the set; we
version-control it and update it when SDF announces a change. Balances
are read live from `account_observations` (the LCM AccountEntry
observer), with the operator-static `reserve_balances_stroops` map as a
bootstrap fallback (see [supply-pipeline.md](../architecture/supply-pipeline.md)).

The market's "circulating" definition, from the [Stellar lumen supply
metrics](https://developers.stellar.org/docs/learn/fundamentals/lumens):
*"Lumens in the Total Supply, but not in the SDF Mandate, Upgrade
Reserve, or Fee Pool are assumed to be circulating."*

## Reconciliation against the market (live, 2026-07-07)

The number is not an independent redefinition — it tracks the
authoritative sources to within **0.03%**:

| Source | XLM circulating | Basis |
|---|---|---|
| **Stellar Index** (`/v1/assets/native`) | **34,076,383,903.5** | total − Σ SDF mandate + upgrade reserve |
| Stellar Network Dashboard (`dashboard.stellar.org`) | 34,066,264,854.0 | total − mandate − upgrade reserve − **fee pool** |
| CoinGecko (`api.coingecko.com`) | 34,066,264,947.4 | sources the Dashboard figure |

Component-level agreement (live r1 `account_observations` vs the
Dashboard's published breakdown):

| Component | Stellar Index | Stellar Dashboard | Δ |
|---|---|---|---|
| SDF Mandate (15 accounts) | 15,666,537,061.0 XLM | 15,666,537,059.9 XLM | ~1 XLM (observation-ledger skew) |
| Upgrade Reserve (1 account) | 258,885,847.5 XLM | 258,885,847.5 XLM | exact to the stroop |

The entire ~10.1 M XLM (0.03%) residual between our figure and the
Dashboard's is explained by two deliberate, immaterial differences:

1. **Fee Pool (~10.10 M XLM)** — the Dashboard subtracts the fee pool;
   we do not (it is 0.03% of circulating and would require plumbing a
   fee-pool reader for a sub-basis-point correction).
2. **Total-supply constant (~0.02 M XLM)** — we use the canonical frozen
   `50,001,806,812`; the Dashboard uses a live `total_coins`-derived
   `50,001,786,840`.

**Verdict: the served figure is correct and market-comparable.** A
recurring instinct is to compare it against a much lower number (e.g.
~29–30 B) and read a bug; that figure is stale — as of mid-2026 both
CoinGecko and the Stellar Dashboard report XLM circulating at ~34 B
because SDF's undistributed holdings have fallen to ~15.9 B. If our
figure ever drifts materially from the Dashboard's, the cause is a
**stale account list** (SDF distributed from, or added, an account we
don't track) — update `stellarindex_sdf_reserve_accounts` +
`stellarindex_reserve_balances_stroops` in one PR and re-run the
supply refresh, per [supply-pipeline.md](../architecture/supply-pipeline.md).

## Refreshing the served value

The figure is refreshed by the aggregator-resident `supply.Refresher`
goroutine (or the `stellarindex-ops supply snapshot` timer) writing
`asset_supply_history`; `/v1/assets/native` serves the latest row via
`Store.LatestSupply`. No re-derive is needed for a correct list — only
an account-set change requires a config update + refresh.

## References

- [ADR-0011](../adr/0011-supply-algorithm.md) — the three-algorithm
  supply spec (XLM is Algorithm 1).
- [supply-pipeline.md](../architecture/supply-pipeline.md) — the
  observer → reader → snapshot → API data flow.
- [Stellar lumen supply metrics](https://developers.stellar.org/docs/learn/fundamentals/lumens)
  — SDF's own circulating definition.
