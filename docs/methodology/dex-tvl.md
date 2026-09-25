---
title: DEX TVL methodology
last_verified: 2026-09-23
status: current
---

# How Stellar Index computes DEX TVL

This is the public methodology for the pooled-liquidity figures served
as the `tvl` block on each `/v1/protocols` row, as the headline
`tvl_total` on the same response, and pool by pool on
`/v1/protocols/{name}/tvl`. It documents what goes into the number,
what is deliberately left out, and the check that stops a wrong total
from being served.

Implementation: `internal/api/v1/dex_tvl_cache.go` (per-protocol),
`internal/api/v1/dex_tvl_pools.go` (the per-pool breakdown and its
drill-down endpoint) and `internal/api/v1/dex_tvl_total.go` (the total
and its reconciliation).

## What is measured

TVL is the current USD value of the reserves sitting in a protocol's
pools — an absolute state figure, never a flow. Each protocol's
reserves come from the source that actually holds absolute state:

| Protocol | Reserve source |
|---|---|
| soroswap | Current pair reserves from each pair's instance storage in the certified lake, scoped to the `soroswap_pairs` registry |
| aquarius | Each pool's latest post-state reserve snapshot from `aquarius_reserves` (trailing 90 days). Token identities are recovered by position from the pool's deposit/withdraw events (`update_reserves` carries none), and the table records no transaction application order, so a pool touched by two transactions at the same operation and event index in its latest ledger reads either one's post-state |
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
substance gate and the scam-directory gate, **whichever of the two this
deployment has wired**. The gate is asked about the asset's
**canonical** identity, so a configured classic↔SAC wrapper is
collapsed to its classic twin first; without that collapse the scam arm
would be a no-op on the C-strkey addresses pool legs actually carry.
The trust check runs **before** the declared-peg shortcut, so a token an
operator once declared 1:1-USD cannot re-enter through the peg after its
issuer has been flagged.

A deployment can disable either screen independently
(`[pricing_guard]`); every response's `basis` field names exactly the
screens THIS wiring actually ran (`internal/api/v1/dex_tvl_cache.go`'s
`basisTail`, narrowed 2026-09-03), never a fixed sentence claiming
both regardless of what is wired. Read `basis` before trusting a TVL
figure against a non-default configuration.

## Per-pool drill-down

`GET /v1/protocols/{name}/tvl` publishes every pool a protocol's figure
was summed from, and every reserve leg of each pool, so the figure can
be checked against the reserves rather than taken on trust. Per leg:

| Field | Meaning |
|---|---|
| `token` | The token contract id exactly as the pool's storage carries it. Absent when the position's address never resolved |
| `reserve` | The captured reserve in base units (i128 decimal string). Absent when nothing was captured |
| `asset` | The **canonical** identity the served price path values the leg under — the same id `/v1/assets/{id}` answers for. A configured classic↔SAC wrapper collapses to its classic twin here, exactly as the trust gates were asked about it |
| `basis` + `usd` | Present when the leg was valued. `declared_usd_peg`: $1 per whole unit at the token's declared decimals, applied only after the trust gates. `served_usd_price`: reserve × the same served USD rate `/v1/assets/{asset}` publishes. `empty_reserve`: the reserve is zero, so the leg is worth exactly $0 and no price was consulted |
| `excluded` | Present when the leg was **not** valued, naming the rule below that excluded it: `withheld` (rule 1), `unverified_asset` (rule 2), `no_served_price` (rule 3), `unresolved_token` (rule 6), `malformed_token`, `invalid_reserve` |

Exactly one of `basis`/`usd` or `excluded` is present on a leg: a leg
that contributed nothing says why on the wire and is never a silent
zero. A pool whose captured storage did not decode (rule 4) is
published with `excluded: undecodable_storage`, no legs and
`tvl_usd: "0.00"`.

**Money is rounded once, at the leaf.** Each valued leg's `usd` is
published to the cent; a pool's `tvl_usd` is the exact sum of its legs'
published `usd`; the protocol's `tvl_usd` is the exact sum of its
pools'; and `tvl_total` is the exact sum of the protocols'. Add the rows
at any level and you land on the level above byte-for-byte. Rounding at
every level instead would let a pool's legs sum to a cent more or less
than the pool, and the whole point of the drill-down is that the rows
reconcile with the figure above them. The cost is bounded: at most half
a cent per valued leg.

A protocol whose reserve read failed on the latest refresh is served
with its previous cycle's figure and pools, labelled
`carried_forward: true` with the envelope's `flags.stale` set. The
headline total refuses that figure (see below); the drill-down is where
to see what was refused.

The drill-down is current state only. Reserve history is not persisted
anywhere in the API, so there is no historical TVL series and none is
fabricated from event flows.

## Exclusion rules

These are the rules that decide what does **not** contribute. Each one
makes the served number smaller, never larger.

### Within a covered protocol

1. **A leg the trust gates withhold contributes exactly 0** and its pool
   is counted in `unpriced_pools`. A number an attacker can author is
   not a lower bound.
2. **A leg whose asset is not identified contributes exactly 0**, same
   accounting (`unverified_asset`). Only native XLM, an
   operator-declared USD peg, or an entry of the hand-vetted verified
   currency catalogue (`internal/currency`) — reached by its classic id
   or through its SAC, derived on the configured network — is valued.
   Pools are permissionless: whoever deploys a token authors both its
   reserve and, with a few dollars of self-trading, its VWAP, which
   clears the price tiers' one-cent floor and the substance floor
   alike. The screen has no off switch; a deployment that wires no
   catalogue values native XLM and declared pegs only.
3. **A leg with no served USD price contributes exactly 0**, same
   accounting. We do not substitute a stale, modelled or single-trade
   price to make a pool look complete.
4. **A pool whose captured storage shape is unrecognised contributes 0**
   and is counted in both `pools_total` and `unpriced_pools`. It is
   never partially decoded or guessed — a contract upgrade that changes
   a storage layout shows up as a counted gap, not a silent shrink.
5. **A pool absent from the reserve read is excluded entirely** — not
   counted, not zeroed. Archived or uncaptured pools are honestly
   "unavailable"; absence is not a reserve of zero.
6. **Aquarius legs whose token address never resolved** are unpriceable
   (`update_reserves` carries positions, not addresses) and count their
   pool unpriced.
7. **Phoenix stake contracts are excluded** from the pool set: they hold
   LP shares, and counting them would double-count the underlying.

Consequence: whenever `unpriced_pools > 0` the figure is a **lower
bound**. At the per-protocol level (each `/v1/protocols` row's `tvl`
block and `/v1/protocols/{name}/tvl`) this is DERIVED, not a dedicated
wire field: `unpriced_pools > 0` is the whole test, the same one the
explorer's `TvlCell` runs client-side to prefix the figure with `≥`.
The headline `tvl_total` additionally publishes a dedicated
`lower_bound` boolean, because it has a second, non-derivable reason to
be one — a refused protocol (see "The reconciliation check" below).

### From the headline total

The total covers exactly the protocols named in its `protocols` array.
The following are indexed by Stellar Index and consciously **not**
summed into it; each is published in the response's `excluded` array
with its reason, so the figure can never be read as a whole-network
claim by omission:

| Excluded | Why |
|---|---|
| Classic (CAP-38) liquidity pools | Indexed and served per-pool at `/v1/liquidity-pools` with two-sided reserves and an `as_of_ledger`, but not yet valued into a protocol row. Which protocol they attach to is an open product decision (#338) |
| SushiSwap V3 (concentrated liquidity) | Not constant product. A V3 pool spreads its depth across per-position tick ranges, so its token balances are not a two-sided reserve and their sum is not the pool's value — that needs the live sqrt price read against every open position's bounds, which is not captured as current state. Running the reserve path over the balances anyway would produce a number with no meaning, so **no figure is derived at all** rather than a wrong one (#350) |
| Soroswap router | Holds no liquidity of its own. Its rows are multi-hop swap intents routed through the same Soroswap pairs already summed under `soroswap`; adding it would double-count those pools |
| SDEX order book | Holds offers, not pooled reserves. Resting depth is a different quantity from locked value and is served separately at `/v1/sdex/orderbook` |
| Blend (lending) | Supplied-value is a different quantity from AMM pooled liquidity. Blend's current-state figure is served per-pool as `tvl_usd` on `/v1/lending/pools/{pool}/reserves` (ADR-0039, decoded from contract storage); adding lending into an AMM headline would flatter it |
| Sorocredit (lending) | Excluded on the same basis as Blend. No current-state figure exists for it — its protocol block carries event and user counts only |
| DeFindex (vaults) | Vault capital is deployed into Blend strategy contracts, so counting vault AUM alongside the protocols holding those positions would double-count it |
| Upshift (vaults) | No current-state total-assets figure exists for it — the only on-event total is the DEPLOYED leg (`deployed_assets_changed`), the idle leg is unobservable from indexed events, and a TVL reader must not zero-fill the missing leg; its protocol block carries event and user counts only |

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

A protocol whose TVL is derived on this deployment but that has **no
figure at all** this cycle is named in `excluded` the same way, and
sets `lower_bound`: either its first reserve read failed with no
earlier figure to carry (the refresh also counts as failed), or its
reader returned no pools — which is unavailable, never a TVL of zero,
so no `"0.00"` is published for it. Its `/v1/protocols/{name}/tvl` 404
gives the same reason rather than calling the derivation unwired.

## Reconciling against our own data

The figures are meant to agree with the rest of the API, and that is
checkable:

- A protocol's `tvl_usd` is the exact sum of the `tvl_usd` of its pools
  on `/v1/protocols/{name}/tvl`, and each pool's `tvl_usd` is the exact
  sum of its legs' published `usd`. For Soroswap specifically, the
  per-pool reserves are the same lake current-state entries
  `/v1/pools/reserves` serves (`source=soroswap`; it is the only venue
  that endpoint accepts today). Phoenix and Comet reserves are not
  cross-checkable against `/v1/pools/reserves` — reconcile those two
  against the pool storage entries the drill-down cites instead.
- The headline `tvl_total.tvl_usd` is the sum of the `tvl_usd` values on
  the `protocols` rows of the same response.
- A leg's valuation uses the same price `/v1/assets/{id}` serves for the
  leg's `asset`. If an asset shows `price_usd: null` there, the leg is
  `excluded` here — the two surfaces cannot disagree, because they
  consult the same gates and the same price tiers.

`TestDEXTVLTotal_ReconcilesAgainstPoolReserves` in
`internal/api/v1/dex_tvl_total_internal_test.go` asserts the protocol
and headline figures against an independently-summed statement of the
same pool reserves, and `TestDEXTVLPools_ReconcileAtEveryLevel` in
`internal/api/v1/dex_tvl_pools_internal_test.go` asserts the leg → pool
→ protocol chain byte-for-byte on the published strings.
