---
title: Blend — contract & event verification
last_verified: 2026-06-12
status: current
---

# Blend — contract & event verification

> **For the Blend team:** this is the complete set of Blend contracts and
> events Stellar Index ingests and attributes to Blend. Please confirm the
> two pool factories and the pool list are correct and complete (especially
> any factory or pool **not** listed here).
>
> - **Enumeration method:** lake deploy-graph (decoded every `deploy`
>   event in our certified lake, ledgers 50.46M–62.99M).
> - **Last verified:** 2026-06-12 (r1 lake).
> - **Gate status:** ✅ enforced (`internal/sources/blend`, ADR-0035).

## Interest revenue: `GET /v1/lending/pools/{pool}/revenue`

Interest per reserve of a Blend V1, Blend V2 or BLNT pool, per hour or day, in the reserve token's base units, with
its value in USD beside it. It is summed from the derived hourly interest record (`lending_interest_hourly`); hours that record does not cover, or
that a pending re-derive will rewrite, are left out and named.

This endpoint prices over the window you ask for: one price walk covers the whole window, and each hour takes that walk's
price. The daily totals the rollup stores (`lending_revenue_usd_daily`) are priced day by day instead: each hour takes the
price a walk of its own UTC day gives. The two agree exactly, except for a market that trades on some days and not on
others. There, a walk over a long window finds the market's own prices and leaves the quiet days unpriced, while a walk of
one quiet day finds none and may fall back to the price derived through XLM. That day is then priced in the daily totals
and not on this endpoint. A day whose walk failed or was cut short is not stored and is retried, and the rollup's coverage
stops before the first such day it meets, so inside coverage a missing day means no interest. A reserve refused by a
current verdict (a scam issuer, a thin latest market, an operator withholding) is stored unpriced with the reason
`withheld`, never as zero, and the daily sweep re-checks it. A re-derive that only deletes an hour's superseded rows
leaves no newer `derived_at`, so the rollup sees it at the daily sweep, not within a cycle.

| Field | What it means |
|---|---|
| `interest_accrued` | Interest borrowers owe. It is booked when the reserve is next touched on chain, so it lands in the hour of that touch, not spread over the hours it built up in. |
| `supplier_share`, `backstop_take`, `protocol_fee` | How the pool splits it; the three add up to `interest_accrued`. `protocol_fee` is BLNT's fee, with its rate inferred from chain data (`fee_rate_inferred`); it is 0 on Blend. |
| `supplier_credited` | What the b-token rate actually credited suppliers. Rounding leaves a little in the pool, so it can sit just below `supplier_share`. |
| `backstop_take` | On Blend this goes to backstop depositors, not to a treasury. |
| `realised.backstop_drawn` | Backstop credit actually paid out by interest auctions. Always a lower bound (`backstop_drawn_lower_bound`): Blend V1 fills do not state their lot, and a draw inside a version nothing explains is left out. |
| `realised.fee_withdrawn` | Always null: BLNT fee withdrawals are not derived yet (`not_derivable`); Blend has no fee (`not_applicable`). |
| `default_loss`, `unexplained_supply_loss` | Losses cut from suppliers, as positive amounts: Blend V2 defaults that reproduce exactly, and what no event explains (Blend V1 and BLNT defaults among it). `unexplained_supply_gain` is the unexplained move the other way. |

Interest borrowers paid in cash is not served: a repayment does not say how much of it is interest. `lower_bound` is true
whenever `excluded` names anything left out; a reserve whose summed part is nothing has `totals: null`, never `"0"`.

**USD.** Each hour's amounts are valued at that hour's price: the newest hourly price at or before the hour, at most six
hours old (`price_max_age_hours`), never a later one and never today's. A daily bucket is the sum of its hours valued
one by one, not the day's total at one price. The sums are exact and rounded to cents once, when served; each point
and each total is rounded on its own, so rounded points need not add up to the rounded total. A reserve's
`price_basis` (or `price_basis_hours` when hours mix) says when prices were crossed through XLM or taken from a
stablecoin-quoted book, as on `/history`. In `totals_usd` the hour counts are reserve-hours. Prices come
from the same series `/history` values reserves with, under the same gates: a token with no USD market, unknown
decimals, a scam-flagged issuer or a thin market (whose price is display-only) is not valued at all (`usd_reason`).

`totals_usd` adds the reserves together in USD, the one unit in which they can be added. An hour with an amount but no
price is left out and counted (`unpriced_hours`), so the sum is a lower bound (`lower_bound`, and `totals_usd.excluded`
names the reserve with `price_missing`). A reserve with amounts but no price at all is null, never `"0.00"`, and is named and left
out of `totals_usd`. Anything the base-unit sums leave out is missing from the USD sums too, so `totals_usd` is a lower
bound whenever the response's `lower_bound` is.

## Protocol revenue: `revenue` on `GET /v1/lending/protocols`

Each protocol carries a `revenue` block: `interest_accrued_usd`, `supplier_share_usd`, `backstop_take_usd`,
`protocol_fee_usd`, `backstop_drawn_usd` and `default_loss_usd` over the windows `1d`, `7d`, `30d` and `all_time`. Each
is summed exactly from the stored daily totals above and rounded to cents once. There is no single "protocol revenue"
figure: the legs go to different parties. `blend` is one row for V1 and V2 pools together, and its `versions` splits each
window into `v1` and `v2`. The versions are summed exactly but each is rounded on its own, so `v1` plus `v2` can differ
from the window by a cent: read the total from the window, never from the versions. `backstop_drawn_lower_bound` is always true, for the reason given
for `realised.backstop_drawn` above.

**Windows.** A window is whole UTC days, never rolling hours. Every window ends on the same `through_day`: the latest day
any of the protocol's pools is valued through. `1d` is that day, `7d` and `30d` the days ending on it. `all_time` starts
at the earliest pool's first valued day (`from_day`); interest from before the derived interest record begins is in no
window. A pool valued through an earlier day, or whose first valued day falls after a window's `from_day`, is named
`days_not_valued` with the number of days it lacks. So the newest day is shown as soon as one pool reaches it, and a slow
pool makes the window a lower bound instead of holding it back.

**Lower bounds.** A window is `lower_bound` whenever `excluded` names anything. A version is `lower_bound` when anything
of its pools is named.

| `reason` | Cause | `count` |
|---|---|---|
| `not_valued` | The pool has no valued day yet. | — |
| `days_not_valued` | Window days outside the pool's valued days. | days |
| `redo_pending` | A pending re-derive will rewrite the pool's days. | — |
| `valuation_stale` | Days stored at an older valuation or derive revision. They are left out, not summed, until the rollup re-values them. | days |
| `price_missing` | Hours with an amount but no price. | hours |
| `withheld` | A read-time verdict refuses the token now, or refused the stored day. Nothing of it is summed. | days |
| `price_read_failed` | The verdict could not be read, so the reserve is left out. | days |
| `amounts_excluded` | Hour rows or versions the interest record left out of a summed day. | rows + versions |
| `pool_read_failed`, `pool_version_unknown`, `revenue_read_failed` | The pool, or the stored days, could not be read or classified. | — |
| `revenue_timed_out` | The block's reads ran past their 5 s budget; the block is null and the rest of the response is served. | — |
| `pool_list_read_failed` | The protocol's pool list could not be read (`pool` is null). | — |

A read failure or timeout sets the block's `degraded`; it does not stop the rest of the response being cached.

The verdicts are applied when the block is built, not only when a day is stored, and they are the ones the revenue
endpoint applies: each token with stored days is walked once, through the same price walk, over its latest stored day
that carries an amount. A scam-flagged issuer, an operator withholding or a thin latest market refuses it (`withheld`),
and it then adds nothing to any window, whatever was stored for it. The live reserves page's own verdict is not used.
This costs one bounded day walk per distinct token per build (the build is cached for a minute), plus the coverage read
and one `GROUP BY` over the daily table on its primary key.

**Before the rollup has run.** With no stored days, every amount is null with `usd_reason: not_valued` and every pool is
named, never `"0.00"`. When something is valued but every day carrying an amount was left out, the amounts are null with
`usd_reason: all_excluded`.

## Bad debt: `GET /v1/lending/pools/{pool}/bad-debt`

When a borrower's collateral is gone but debt remains, the pool moves that debt onto its backstop's own position. From
there it is either bought out at auction, written off against suppliers, or it stays on the backstop. The endpoint shows,
per reserve, how much went each way, in d-tokens (the pool's debt units), in the underlying token and in USD.

| Part | What it means |
|---|---|
| `moved_to_backstop` | Debt moved onto the backstop (`bad_debt` events). |
| `covered_by_backstop` | Debt bidders took over in bad-debt auctions, in return for backstop tokens. A late fill can carry an empty bid (the bid decays to nothing after enough ledgers): it covered 0 and is counted in `zero_bid_fills`. Blend V1 fills do not say what they took, so on a pool with one this part is null (`bid_not_recorded`), never estimated. |
| `socialised_to_suppliers` | Debt written off (`defaulted_debt`): the d-tokens burnt. Suppliers' loss in underlying is `default_loss` on `/revenue`. |
| `repaid_by_backstop` | Repays of the backstop's own debt. |
| `outstanding` | Debt the backstop still carries now, at today's rate and price. |

The backstop is the address the pool's own storage names. On BLNT the backstop is split into loss tiers and which
address carries the debt is not verified, so `repaid_by_backstop` and `outstanding` are null there. What a BLNT bad-debt
auction bid is made of is not proven either, so `covered_by_backstop` is null there too (`leg_kind_unknown`).

Each event is valued when it happened: the d-token rate its own transaction wrote, and the last hourly price at or
before it. A value that cannot be worked out is null with a reason, never `"0"`. A `"0"` over `events: 0` is a real zero.
If any one event lacks a rate, `underlying` is null (`underlying_reason`); if any one lacks a price, `usd` is null
(`usd_reason`). Neither is ever the sum of only the events that could be valued.

`reconciled` checks that the debt moved equals the other four parts added up, in d-tokens, over the events up to the
ledger the backstop's position was last written (`position_ledger`). `residual_d_tokens` is the gap. It is null when a
part is missing; when the event tables are not yet fully written up to that ledger (`events_written_through`, the
slowest writer's cursor, is behind it), because a half-written ledger would show a false gap; or when no recorded
event is at that ledger. In `totals_usd` a
part's sum over reserves is a lower bound when `excluded` names a reserve left out.

## Emissions history: `GET /v1/lending/pools/{pool}/emissions/history`

A pool's emissions per hour or UTC day, in base units of the emission token (BLND for Blend), each with `usd`. Four
streams describe the same tokens at different stages, so they are separate fields and are never added together, in
tokens or in USD.

USD is valued as `/revenue` values interest: each hour's tokens at that hour's BLND price through the same walk (at most
6 h old, never a later price), summed exactly and rounded once per figure; a `1d` bucket is valued from its hours. It
is served only when the pool's emission token is the SAC of the verified BLND asset (code and issuer). V1's token is
not read, so V1 USD is null (`emission_token_unread`). A figure with unpriced hours is a lower bound; one with nothing
valued is null with `usd_reason`, never `0.00`.

| Stream | What it means |
|---|---|
| `reserves` | **Scheduled**: per reserve token id, the announced rate times time, `Σ eps × seconds / 10^7`, exact. Rebuilt from `reserve_emission_update` events; the rate in force when the window opens is each id's last update at or before `from`. For V2 the event rate equals the stored rate exactly. Holders accrue it only while the side has supply; hours with an empty side are not detected yet, so it can include tokens nobody accrued. V1: null (`v1_emission_schedule_unverified`), because V1 does not store eps the V2 way and its scale is unverified. |
| `pool_claims` | **Claimed** from the pool: claims, `amount_claimed` and distinct claimers per bucket. `by_reserve` holds only claims naming exactly one reserve id. Claims naming several (79.6% of the V2 amount, 93.2% of V1) are in the pool total but listed in `excluded` for `by_reserve` (`claim_names_several_reserves`), never split. V1 and V2. |
| `backstop_alloc` | **Allocated**: the backstop's `gulp_emissions` naming the pool, `new_backstop_emissions` and `new_pool_emissions`. V2 only; V1 is null (`v1_backstop_unaudited`). |
| `backstop_claims` | Null (`backstop_claim_names_no_pool`): the backstop `claim` names no pool, and its amount is backstop LP tokens, not BLND. |

A token id with no update at or before `to` is not listed. A listed id's `points` holds only buckets with tokens
scheduled, so a missing bucket is a zero rate, not a gap. Any exclusion names itself in `excluded` and sets
`lower_bound` on its stream: the writer's projection not verified back to its first ledger, unread amounts, a window
past the lake tip (`window_past_lake_tip`) or an unread tip (`lake_tip_unknown`).

A bucket is served as complete only when both the lake and the stream's own Postgres writer have passed it. Each stream
ends at its own `served_through`: the earliest of `to`, now, the lake tip and its writer's committed ledger (the `blend`
projection for `reserves` and `pool_claims`, `blend_backstop` for `backstop_alloc`). A writer short of the lake is
`writer_behind_lake`; one whose cursor cannot be placed is `writer_cursor_unread`, and then the schedule is withheld,
because past an unknown cursor a missed update would run the old rate on. Blend records no transaction order within a
ledger, so two updates to one token id from different transactions in one ledger are ordered by transaction hash and the
line is flagged `approximate` (`same_ledger_update_order_unknown`).

## Health factor

How the pool contract values an account, read from the upstream source at two pinned revisions:

- **V2:** `blend-contracts-v2` commit `c19abee5b9be4f49e0cda9057e87d343e5dcc095` (`v2.0.0`, the source of the audited
  pool WASM, [wasm audit](../operations/wasm-audits/blend.md)). Cited below as `v2:<path>:<line>`.
- **V1:** `blend-contracts` tag `v1.0.0`, commit `1c1dabab2a6a71f6354d6d59f4307b99760ef3eb`. Cited as `v1:<path>:<line>`.

Paths are under `pool/src/`. The fixed-point helpers are `soroban-fixed-point-math` (V2 pins 1.3.0, V1 1.0.0, each
repo's `Cargo.toml:31-32`): `fixed_mul_floor(x, y, d)` is `floor(x·y/d)`, `fixed_mul_ceil` the ceiling, and
`fixed_div_floor(x, y, d)` is `floor(x·d/y)` (`src/i128.rs:6-56` in both versions). V2's `SorobanFixedPoint` widens to
I256 on overflow (1.3.0 `src/i128.rs:78-110`); V1's `FixedPoint` returns `None`, which the pool unwraps into a panic.
`internal/sources/blend/health.go` implements these steps in `*big.Int`.

### Per reserve, then summed (identical in V1 and V2)

`PositionData::calculate_from_positions` (`v2:pool/health_factor.rs:27-78`, `v1:pool/health_factor.rs:27-82`) walks the
pool's reserve list. A reserve where the account holds neither collateral nor liability is skipped without loading its
price (`v2:…:38-40`, `v1:…:38-40`). Only the `collateral` and `liabilities` maps are read; non-collateral `supply`
never counts. For each remaining reserve, with `price` the oracle's raw `lastprice` and `scalar = 10^decimals` of the
reserve (`v2:pool/reserve.rs:40`, `v1:pool/reserve.rs:53`):

| Step | Collateral (b-tokens) | Liability (d-tokens) | Source |
|---|---|---|---|
| 1. to underlying | `floor(b·b_rate / R)` | `ceil(d·d_rate / R)` | `v2:pool/reserve.rs:182-183, 174-175`; `v1:pool/reserve.rs:169-171, 159-161` |
| 2. factor | `floor(u·c_factor / 1e7)` | `ceil(u·1e7 / l_factor)` | `v2:pool/reserve.rs:201-203, 191-193`; `v1:pool/reserve.rs:192-195, 180-183` |
| 3. to oracle base | `floor(price·e / scalar)` | `ceil(price·e / scalar)` | `v2:pool/health_factor.rs:46-48, 58-60`; `v1:pool/health_factor.rs:46-49, 60-63` |
| 4. sum | `collateral_base = Σ` | `liability_base = Σ` | same functions |

`R` is the rate scale: `SCALAR_12 = 1e12` on V2 (`v2:constants.rs:4`), `SCALAR_9 = 1e9` on V1 (`v1:constants.rs:4`).
Factors are `SCALAR_7 = 1e7` in both (`v2:constants.rs:7`, `v1:constants.rs:7`). Each step rounds on its own, per
reserve, before summing: collateral always down, liability always up.

The health factor itself is `floor(collateral_base · oracle_scalar / liability_base)`, at the oracle's decimals
(`v2:pool/health_factor.rs:81-84`, `v1:pool/health_factor.rs:85-89`). `oracle_scalar = 10^decimals()` of the pool's
oracle, read once per call (`v2:pool/health_factor.rs:28`, `v2:pool/pool.rs:102-109`; V1 the same lines). The
liquidation gate below compares the two bases directly and never uses this rounded ratio.

Because step 3 rounds per reserve, oracle decimals cancel only away from the boundary: the same real price at a
different number of decimals can decide an account within a few base units of `collateral_base = liability_base`
differently (`TestAccountHealth_OracleDecimalsMatterAtBoundary`).

### Thresholds

| Rule | V2 | V1 |
|---|---|---|
| User liquidation allowed (auction creation) | `liability_base > collateral_base`; otherwise `InvalidLiquidation` (`v2:auctions/user_liquidation_auction.rs:49-52`) | `liability_base >= collateral_base`; refuses only `liability_base < collateral_base` (`v1:auctions/user_liquidation_auction.rs:35-38`). At exactly equal bases V1 liquidates and V2 does not. |
| After a borrow, withdraw or other health-checked request | Refused with `InvalidHf` when the account has liabilities and `HF < floor(oracle_scalar · 1.0000100)`; also `MinCollateralNotMet` when `collateral_base < min_collateral` (`v2:pool/submit.rs:186-194`, `is_hf_under` at `v2:pool/health_factor.rs:101-110`) | Same `InvalidHf` rule, no `min_collateral` (`v1:pool/submit.rs:41-49`, `v1:pool/health_factor.rs:106-118`) |
| Post-liquidation size bounds | HF must end at most 1.15 and at least 1.03 (`v2:auctions/user_liquidation_auction.rs:193-201`) | Same (`v1:auctions/user_liquidation_auction.rs:109-117`) |

An account with no liabilities passes the borrow check (`is_hf_under` returns false) and is never user-liquidatable:
V2's gate refuses it, and V1's sizing divides by the zero liability and collateral totals
(`v1:auctions/user_liquidation_auction.rs:43-51`).

### Oracle price

- **Staleness.** `Pool::load_price` refuses a price whose `timestamp + 86400` is before the ledger's close time:
  `InvalidPrice` on V2, `StalePrice` on V1 (`pool/pool.rs:119-131` and the check at `:126` in both). A price exactly
  24 h old is accepted.
- **Non-positive price.** V2 also refuses `price <= 0` with `InvalidPrice` (`v2:pool/pool.rs:126`). V1 does not check.
  `health.go` refuses a non-positive price on both, so a V1 account priced at zero is excluded rather than valued.
- **Caching.** Prices and the oracle's decimals are cached for the rest of the invocation (`pool/pool.rs:120-122` and
  `:103-105` in both). Every reserve in one health check is valued at the same oracle and the same decimals.
- **Asset.** The oracle is asked for `Asset::Stellar(<reserve asset>)` (`v2:pool/pool.rs:124-125`).

### Still open

- **Interest since the last write.** The contract accrues each reserve to the current ledger when it loads it
  (`v2:pool/reserve.rs:35-77`) before converting tokens. Stored `b_rate`/`d_rate` are as of `last_time`, so health
  computed from storage is as of each reserve's last write, not now.
- **Behavioural check.** These rules are read from source, not yet compared with the contract's own decision on a
  sample of mainnet liquidation auctions.
- **Overflow.** An account whose intermediate products exceed i128 makes V1 panic; `health.go` computes it without
  panicking. No mainnet account is known to reach that size.
- **BLNT** is closed source; see [blnt.md](blnt.md#health-factor).

## Pool factories (trust roots)

A contract's events are attributed to Blend only if the contract is one of
these factories or a pool one of them deployed. Both emit only `deploy`
(pure factory signature), body = the new pool's `Address`.

| Factory | `deploy` count | first → last ledger | Notes |
|---|---:|---|---|
| `CCZD6ESMOGMPWH2KRO4O7RGTAPGTUPFWFQBELQSS7ZUK63V3TZWETGAG` | 17 | 51,499,915 → 55,857,910 | **Earlier factory — not in our discovery docs; found via lake verification.** |
| `CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU` | 10 | 56,615,475 → 62,967,177 | Pool Factory V2 (documented). |

Also tracked: **Backstop V2** `CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7`.

## Pools (27 — every contract the factories deployed)

The 9 pools that have emitted liquidation auctions (`new_auction` /
`fill_auction`) are marked **active**; the rest were deployed but have no
auction activity in the lake (they may still have supply/borrow flow).

### Deployed by `CCZD6ESM…` (17)

```
CADP6E57HEJOAWHBSTEDJYFJSRU5C5D7YBHFEET23CAHD2KGD4XKCFMS
CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S   active
CB22FIF722FWWHKDX6URY2LHTOS6TWLPXL2IOGY5QS6YNQXTRBDCNPD3
CB7V7T52OLKMBC5QPL7GH2OKR4XV6YWDURUXSAAAFCPSNX7EPBYF5DJE
CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB   active
CBQPFUWOMGTGC5X65J52Z2OHFWYWFCA3TMYCVY6G2T2SB326WW45HF2G
CBVOPI6QC6OWVCOEZDCFELAGQNAOHUS4CWOKAVADKQZXVSWR2R5IAKO7
CCTZXMW3DJIKDI3UVDUJR6PM4WFFEB5RIWDXJBGIEFBD5XFHI26LZ5BU
CDAKUFO3WOUG2DLY6XTNRKBSK53VJTJXMTOUEMPKOWN4R756OFICXWID
CDE65QK2ROZ32V2LVLBOKYPX47TYMYO37Z6ASQTBRTBNK53C7C6QF4Y7   active
CDIUMS2ZNGNGHDRBKFXS4QU23ATPYCTDBUHGZ6FS2MPAEY37FAC4JD3R
CDJ6Q3A2NUK3ANWFGXCHUBPQJXKAXBHNUVILGTEOTSEH2NDZC4FI632B
CDJD2PFCHD2R4SHP3WJ4C6JEF445ODSO74WOCKNFS25I4XI7HMLK3VYO
CDK4KXOYG332TO7VDARUJ66RMQTEADFSZY3RDJZQBS7ZFCD25RV52NXP
CDL3EQ4P3DQH5Q6BT3AINZCCJKUHSXPJAOF7YP3JE7MFJX7FGXHPT27B
CDU4RTOYFZERUD727WW6VRXH5IK35GLCXCPK5ILUYRLLYYMTYSCJXUEA
CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP   active
```

### Deployed by `CDSYOAVX…` (10)

```
CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY
CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC   active
CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD   active
CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI
CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED
CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3
CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF   active
CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS   active
CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI   active
CDZVHCO7LDUJZSME3PJPJXAKT7F6W5IXSOXTJ2QEK3Y2X2CDUREBUMUY
```

## Events decoded (from pool contracts)

Verified against `blend-contracts-v2` `pool/src/events.rs` /
`pool-factory/src/events.rs`.

| Event topic[0] | Where it lands |
|---|---|
| `deploy` (factory) | registers the new pool (pool registry) |
| `new_auction`, `fill_auction`, `delete_auction` | `blend_auctions` |
| `supply`, `withdraw`, `supply_collateral`, `withdraw_collateral`, `borrow`, `repay`, `flash_loan` | `blend_positions` |
| `gulp`, `claim`, `reserve_emission_update`, `gulp_emissions`, `bad_debt`, `defaulted_debt`, `update_emissions` (V1) | `blend_emissions` |
| `set_admin`, `update_pool`, `queue_set_reserve`, `cancel_set_reserve`, `set_reserve`, `set_status`, `new_liquidation_auction` (V1), `delete_liquidation_auction` (V1) | `blend_admin` |

## V1 pool-factory's 3 extra topics — HANDLED (ROADMAP #89, 2026-07-10)

A read-only topic census against the 27 gated pools + 2 factories
found 778 real events (0.14% of this source's lake volume) across
3 topics `classifyAny` didn't recognize, all specific to the V1
pool-factory (`CCZD6ESM…`)'s simpler/different vocabulary — real-lake-
bytes verified at ledgers 51,524,668 / 51,611,821 / 54,890,906:

- `update_emissions` (543 events) — 1 topic, bare-i128 body (a
  pool-wide emissions total, not V2's per-reserve
  `reserve_emission_update` map) → `blend_emissions`.
- `new_liquidation_auction` (234 events) — 2 topics (no
  `auction_type`), body `Map{bid, lot, block}` — the SAME
  `AuctionData` shape `decodeAuctionData` already parses for V2's
  `new_auction`, just missing the `auction_type` topic + `percent`
  field. Lands in `blend_admin` (not `blend_auctions`, which requires
  a non-null `auction_type`) — `Target`=user, `attributes`={bid, lot,
  block}.
- `delete_liquidation_auction` (1 event) — 2 topics, `ScvVoid` body →
  `blend_admin`.

Migration 0097 widened `blend_emissions.event_kind` /
`blend_admin.event_kind`. Full evidence trail + golden tests:
`internal/sources/blend/README.md` + `v1_pool_factory_test.go`.
Historical replay from the source genesis (`blend.FactoryGenesisLedger`,
51,499,546 — 369 ledgers before the V1 factory's first deploy at
51,499,915, so no V1 pool's events are skipped) has run
(`projector-replay -source blend -from 51499546`): the daily ADR-0033
pass reports `blend` `complete=true` with substrate verified over
[51,499,546, 64,688,438] (r1, 2026-09-30).

## Backstop singletons (2 — decoded by the `blend_backstop` source)

Like the pool factories, the Backstop was redeployed — V1 + V2 both have
on-chain activity. Their event surface (different from the pools) is
decoded by the dedicated **`blend_backstop`** source (migration 0063 +
0095; 12 event kinds), gated on the two contract addresses below. Both
contracts now fold into this protocol page (roster **module** rows +
the lake event breakdown + `events_24h`) and back the "Backstop volume
/ events" KPIs in the lending analytics block. Lake-verified
2026-06-12; independently corroborated by the community Dune dashboards
(mootz12/blend-v2-events shows the V2 backstop's ~92k event rows).

| Backstop | Address | Lake events (top kinds) |
|---|---|---|
| V1 (**previously undocumented**) | `CAO3AGAMZVRMHITL36EJ2VZQWKYRPWMQAPDQD5YEOF3GIF7T44U4JAL3` | queue_withdrawal ×1,374 · gulp_emissions ×209 · dequeue_withdrawal ×143 · claim ×6 · rw_zone ×5 (51.49M→62.08M) |
| V2 (documented) | `CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7` | queue_withdrawal ×1,312 · deposit ×634 · claim ×570 · distribute ×247 · dequeue_withdrawal ×163 · donate ×132 · withdraw ×30 · gulp_emissions ×11 · rw_zone_add ×5 |

> **Why they're a separate source, not the pool gate:** backstop event
> bodies differ from the pool events sharing the same topic names
> (`claim`, `withdraw`, `gulp_emissions`) — routing them through the pool
> decoder would mis-decode. So the contract-id gate (not the topic
> symbol) disambiguates a backstop event from a pool event, and the
> backstop lands in its own `blend_backstop_events` table via the
> `blend_backstop` source.
>
> **Provenance:** the event schemas were originally reverse-engineered
> from mainnet lake samples (2026-06-15). On **2026-07-09** a read-only
> lake audit cross-checked the V2 shapes directly against the Blend
> team's published source (`blend-contracts-v2`
> `backstop/src/events.rs`) and found + fixed six decode bugs: V1
> `gulp_emissions` was 100% mis-decoded (wrong topic arity + body
> shape); the V1 reward-zone topic (`rw_zone`, distinct from V2's
> `rw_zone_add`) was never classified at all, silently dropping 5 real
> events; `rw_zone_add`'s second body element is `Option<Address>`, not
> a `u32` index; `gulp_emissions`' topic[1] is the pool address, not a
> "token"; and `withdraw`'s body order (`shares, tokens`) is opposite
> `deposit`'s (`tokens, shares`), which was being promoted
> positionally. `rw_zone_remove` was added (zero lake occurrences ever)
> per the EVERY-event principle but remains unverified against real
> bytes. See `internal/sources/blend_backstop/decode.go` +
> `CHANGELOG.md` for the full detail. The source remains
> **live-capture only** (no historical backfill / `BackfillSafe`-style
> flip) until the required historical replay
> (`projector-replay -source blend_backstop -from 51499923`) lands —
> that replay both backfills V1 genesis→now and corrects previously
> mis-mapped `withdraw` / `gulp_emissions` / `rw_zone_add` rows written
> under the pre-fix decoder. **Blend team:** please confirm the V1
> backstop address and the event schemas.
>
> **Update 2026-07-09:** an Emitter-contract event surface DOES exist —
> the Emitter contract
> (`CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR`) is now
> covered by the dedicated `blend_emitter` source (`distribute` /
> `drop` / `q_swap` / `swap`, gated on contract identity — its
> `distribute` topic collides with this Backstop's own `distribute`
> event above). See [`docs/protocols/blend_emitter.md`](blend_emitter.md).

## Emission data inventory

What the repo can establish about BLND emission data without the lake, read
2026-11-27 from the decoders, goldens and audits cited in each row. Nothing
here is served as a historical emission figure yet. BLNT's equivalent is in
[`blnt.md`](blnt.md#emission-data-inventory).

### Events

| Contract | Event | Topics | Body | Identifies | Does not identify |
|---|---|---|---|---|---|
| V2 pool | `reserve_emission_update` | `[sym]` | `(res_token_id u32, eps u64, expiration u64)` (`internal/sources/blend/decode_money_market.go:260`) | pool (emitter); reserve and side through `res_token_id`: `2·i` is reserve `i`'s dToken (borrow), `2·i+1` its bToken (supply), resolved against the pool's `ResList` (`internal/storage/clickhouse/lending_emissions_reader.go:222`) | the stream's start time other than the event's ledger; whether any supply/borrow exists to receive it |
| V2 pool | `gulp_emissions` | `[sym]` | `i128 emissions` (`decode_money_market.go:299`) | pool; the allocation it received | the split across reserves |
| V2 pool | `claim` | `[sym, from]` | `(reserve_token_ids Vec<u32>, amount_claimed i128)` (`decode_money_market.go:213`) | pool, claimer, the set of reserve token ids | the amount per reserve when the set has more than one id |
| V1 pool | `reserve_emission_update` | `[sym]` | the V2 shape (`res_token_id u32, eps u64, expiration u64`); lake body-shape census 2026-11-27 | as V2 | as V2; the eps scale (see Still open) |
| V1 pool | `claim` | `[sym, from]` | the V2 shape (`reserve_token_ids Vec<u32>, amount_claimed i128`); lake body-shape census 2026-11-27 | as V2 | as V2 |
| V1 pool | `update_emissions` | `[sym]` | bare `i128` (`decode_money_market.go:646`; lake-verified at ledger 51,524,668, `internal/sources/blend/README.md:194`) | pool; one pool-wide total | reserve, side, rate, duration |
| V2 backstop | `gulp_emissions` | `[sym, pool]` | `(new_backstop_emissions i128, new_pool_emissions i128)` (`internal/sources/blend_backstop/README.md:33`) | pool; the backstop's and the pool's share of the cycle | — |
| V1 backstop | `gulp_emissions` | `[sym]` | bare `i128` (`README.md:34`) | an amount | the pool (stored NULL, never guessed) |
| V2 backstop | `distribute` | `[sym]` | `i128` (`README.md:32`) | an amount | the pool |
| V1, V2 backstop | `claim` | `[sym, user]` | `i128` (`internal/sources/blend_backstop/decode.go:164`) | the claimer; on V2 the amount is LP tokens, with the per-pool split in same-tx `deposit` events | the pool; the V1 unit |
| Emitter | `distribute` | `[sym]` | `(backstop_id, amount)` ([`blend_emitter.md`](blend_emitter.md)) | which backstop received BLND | anything below the backstop |
| Emitter | `drop` | `[sym]` | list of `(recipient, amount)` | recipients of the one-off drops | — |

Storage: each pool holds `EmisData(res_token_id)` with `eps`, `expiration`
and `last_time` (`lending_emissions_reader.go:238`), and the backstop holds
`BEmisData(pool)`. The API reads only their current values. Their history is
not in `lending_state_hourly` (`deploy/clickhouse/lending_state.sql:8`), but
every write is a raw `stellar.ledger_entry_changes` row.

### Known holes

- A pool `claim` naming several reserve token ids is one total; it cannot be
  attributed per reserve and must be excluded, never split.
- A backstop `claim` names no pool, so backstop emissions claimed cannot be
  attributed per pool. `/emissions` refuses it as
  `blend_backstop_claim_names_no_pool` (`internal/api/v1/lending_emissions.go:269`).
  On V2 the per-pool LP is in the same transaction's backstop `deposit`
  events (their sum may be 1 below the claim); BLND appears only as the
  claim's total transfer to the Comet pool `CAS3FL6T…`.
- V1 backstop `gulp_emissions` names no pool, and the V1 backstop WASM
  `62f61b32…` is unaudited, so its replays are refused
  ([wasm audit](../operations/wasm-audits/blend.md#backstop-v1-wasm--unaudited-replays-refused-2026-10-05)).
- V1 pools emit `reserve_emission_update` (5 pools, 2,804 events) beside
  the pool-wide `update_emissions`, but V1 eps is absent from `EmisData` (24 of 24 sampled keys) and its
  scale is unverified. V1 emission storage is refused as
  `blend_v1_emission_storage_unverified` (`lending_emissions.go:162`).
- The pool decoders dispatch on topic and arity within the gated pool set, not
  on WASM hash. A `claim` or `reserve_emission_update` in any other body shape
  fails decode and is never written (`decode_money_market.go:213`, `:260`).

### Settled by the lake census (2026-11-27)

A production census of the ClickHouse lake and Postgres answered the
questions this section used to list as open.

- **A. Unit of the V2 backstop `claim` amount: LP tokens.** Of 26,313 V2
  backstop claims, all a bare `i128`, 26,295 have a same-transaction backstop
  `deposit` by the claimer: 25,742 equal the deposit's `tokens_in`, 553 are
  exactly 1 above the sum of the deposits, and the 18 without a deposit are
  zero-amount claims. Five traced transactions show BLND moving from the
  backstop to the Comet pool `CAS3FL6T…`, a Comet join, and a `deposit` whose
  `tokens_in` equals the claim. So the claimed emissions arrive as a deposit:
  the account earnings route counts them in `net_flow`, not `accrued`.
- **B. WASM lineage and V1 topics.** In the lake's contract lineage every V1
  pool with events (16 of the 17 V1-factory pools; the 17th has none in the
  census) runs `baf978f1…`, and all 10 V2 pools, including `CDZVHCO7…`, run
  `a41fc53d…`. V1 pools emit `reserve_emission_update` (5 pools, 2,804
  events) and `claim` (4 pools, 16,626 events), both in the V2 body shape;
  `update_emissions` (5 pools, 561 events) is the V1 counterpart of
  `gulp_emissions`. Lake and Postgres counts agree, except V2 pool claims
  (15,752 in the lake, 15,754 in Postgres). The V1 backstop never emits
  `distribute`.
- **C. Event eps against storage, V2: equal.** In 31 sampled V2 and BLNT
  transactions the event's eps and expiration equal the `EmisData` written in
  the same transaction; every sampled eps change had the event; 20 of 20
  current V2 keys equal their last event. V2 eps is at the 10^7 scale the
  reader assumes. Events rebuild the V2 schedule exactly. V1 eps is absent
  from `EmisData` in 24 of 24 sampled keys, though `last_time` decodes.
- **D. Pool claims by reserve-id count** (share of claims / share of amount):

  | | one id | two or more ids | none |
  |---|---|---|---|
  | V1 | 28.1% / 6.8% | 71.9% / 93.2% | 4 claims |
  | V2 | 48.3% / 20.4% | 51.4% / 79.6% | 45 claims |

  Single-reserve claims, the only ones attributable per reserve, carry 7–20%
  of the claimed amount.

### Still open

- **V1 eps scale.** V1 keeps eps outside `EmisData`, and dividing V1 event
  eps by 10^7 gives implausible rates, so the scale differs by version.
- **V1 backstop.** Its WASM `62f61b32…` is unaudited (see Known holes), and
  the unit of its `claim` amount was not checked.
- **The +1.** Why 553 claims exceed their same-transaction deposits by
  exactly 1. Likely each pool's floor of one LP total, which would make all
  553 multi-pool claims; not checked.
- **Unprovable from events:** the per-pool BLND of a backstop claim (only the
  total transfer is observable; a per-pool figure is a pro-rata derivation),
  and the per-reserve amount of a multi-reserve pool claim.
