---
title: DEX TVL methodology
last_verified: 2026-09-03
status: current
---

# How Stellar Index computes DEX TVL

This is the public methodology for the pooled-liquidity figures served
as the `tvl` block on each `/v1/protocols` row and as the headline
`tvl_total` on the same response. It documents what goes into the
number, what is deliberately left out, and the check that stops a
wrong total from being served.

Implementation: `internal/api/v1/dex_tvl_cache.go` (per-protocol) and
`internal/api/v1/dex_tvl_total.go` (the total and its reconciliation).

## What is measured

TVL is the current USD value of the reserves sitting in a protocol's
pools — an absolute state figure, never a flow. Each protocol's
reserves come from the source that actually holds absolute state:

| Protocol | Reserve source |
|---|---|
| soroswap | Current pair reserves from each pair's instance storage in the certified lake, scoped to the `soroswap_pairs` registry |
| aquarius | Each pool's latest post-state reserve snapshot from `aquarius_reserves` (trailing 90 days) |
| phoenix | Current `ReserveA`/`ReserveB` persistent entries in the lake, with token identities from the pool's CONFIG entry, scoped to the ADR-0035 curated pool set |
| comet | Current per-token pool balance records in the lake, scoped to the curated allowlist |

Phoenix and Comet events carry flow deltas rather than post-state
reserves, which is why their figures come from pool storage: a window
net-flow is not TVL and is not dressed up as one.

Reserves are `*big.Int` throughout, `NUMERIC` in Postgres and decimal
**strings** on the wire (ADR-0003). No amount in this path is ever an
`int64` or a float.

## How a reserve leg is valued

Every leg is valued through the **same** USD price tiers that stamp
`trades.usd_volume` — operator-declared peg, then direct VWAP, then the
XLM bridge. There is no bespoke TVL price lookup, so a protocol's TVL
and the volume, supply and market-cap figures on neighbouring surfaces
are all the same methodology applied to different quantities.

Before any valuation, each leg's asset is put through the serving trust
gates at the same chokepoint every other served price uses — the
substance gate and the scam-directory gate. The gate is asked about the
asset's **canonical** identity, so a configured classic↔SAC wrapper is
collapsed to its classic twin first; without that collapse the scam arm
would be a no-op on the C-strkey addresses pool legs actually carry.
The trust check runs **before** the declared-peg shortcut, so a token an
operator once declared 1:1-USD cannot re-enter through the peg after its
issuer has been flagged.

## Exclusion rules

These are the rules that decide what does **not** contribute. Each one
makes the served number smaller, never larger.

### Within a covered protocol

1. **A leg the trust gates withhold contributes exactly 0** and its pool
   is counted in `unpriced_pools`. A number an attacker can author is
   not a lower bound.
2. **A leg with no served USD price contributes exactly 0**, same
   accounting. We do not substitute a stale, modelled or single-trade
   price to make a pool look complete.
3. **A pool whose captured storage shape is unrecognised contributes 0**
   and is counted in both `pools_total` and `unpriced_pools`. It is
   never partially decoded or guessed — a contract upgrade that changes
   a storage layout shows up as a counted gap, not a silent shrink.
4. **A pool absent from the reserve read is excluded entirely** — not
   counted, not zeroed. Archived or uncaptured pools are honestly
   "unavailable"; absence is not a reserve of zero.
5. **Aquarius legs whose token address never resolved** are unpriceable
   (`update_reserves` carries positions, not addresses) and count their
   pool unpriced.
6. **Phoenix stake contracts are excluded** from the pool set: they hold
   LP shares, and counting them would double-count the underlying.

Consequence: whenever `unpriced_pools > 0` the figure is a **lower
bound**, published as such on the wire (`lower_bound`) and rendered with
a `≥` prefix and a hatched bar tail in the explorer.

### From the headline total

The total covers exactly the protocols named in its `protocols` array.
The following are indexed by Stellar Index and consciously **not**
summed into it; each is published in the response's `excluded` array
with its reason, so the figure can never be read as a whole-network
claim by omission:

| Excluded | Why |
|---|---|
| Classic (CAP-38) liquidity pools | Indexed and served per-pool at `/v1/liquidity-pools` with two-sided reserves and an `as_of_ledger`, but not yet valued into a protocol row. Which protocol they attach to is an open product decision (#338) |
| SDEX order book | Holds offers, not pooled reserves. Resting depth is a different quantity from locked value and is served separately at `/v1/sdex/orderbook` |
| Blend, Sorocredit (lending) | Supplied-value is a different quantity from AMM pooled liquidity. It is published per-protocol as `bespoke.tvl_usd`; adding the two together would flatter the headline |
| DeFindex (vaults) | Vault capital is deployed into Blend strategy contracts, so counting vault AUM alongside the protocols holding those positions would double-count it |

## As-of

Each `tvl` block carries an `as_of_ledger`: the highest ledger at which
any contributing pool's reserves changed — that protocol's chain
high-water, the same convention `/v1/liquidity-pools` stamps per pool.
A pool untouched since an earlier ledger is equally current; every
reserve reader's contract is "current as of this ledger and unchanged
since". `as_of` alongside it is the wall-clock instant the snapshot was
computed (the snapshot refreshes every 10 minutes).

## The reconciliation check

The total is the **exact sum of the per-protocol `tvl_usd` strings
published on the same response**. Add up the rows you can see and you
land on the headline byte-for-byte. A total derived from the underlying
rationals would round-trip to a different figure than the published
parts, and a caller would have no way to tell which of us was wrong.

A per-protocol figure is admitted into that sum only when its own claims
hold. Three refusals:

- **Carried forward.** When a protocol's reserve read fails, its
  PREVIOUS figure is kept so a transient hiccup doesn't blank a healthy
  number — but that figure is a cycle old and cannot honestly be
  published under this snapshot's `as_of`.
- **Pool accounting doesn't balance.** Every pool is counted priced XOR
  unpriced, so `pools_total` must equal their sum. When it doesn't, the
  money and the coverage claim came from different accountings and the
  lower-bound story is no longer provable.
- **Unusable figure.** A `tvl_usd` that is not a non-negative decimal
  has no defined contribution to an exact sum.

A refused protocol is **dropped from the total and named in `excluded`
with the reason**. The headline narrows and says why; it never absorbs a
figure we cannot stand behind. Operators see the same verdict as
`stellarindex_dex_tvl_reconcile_total{outcome="divergent"}`.

## Reconciling against our own data

The figures are meant to agree with the rest of the API, and that is
checkable:

- A protocol's `tvl_usd` is the sum of its pools' priced reserve legs.
  The per-pool reserves behind soroswap, phoenix and comet are the same
  lake current-state entries `/v1/pools/reserves` serves.
- The headline `tvl_total.tvl_usd` is the sum of the `tvl_usd` values on
  the `protocols` rows of the same response.
- A leg's valuation uses the same price `/v1/assets/{id}` serves for that
  asset. If an asset shows `price_usd: null` there, it contributes 0
  here — the two surfaces cannot disagree, because they consult the same
  gates and the same price tiers.

`TestDEXTVLTotal_ReconcilesAgainstPoolReserves` in
`internal/api/v1/dex_tvl_total_internal_test.go` asserts the first two
of those against an independently-summed statement of the same pool
reserves.
