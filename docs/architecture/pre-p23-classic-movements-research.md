---
title: Pre-P23 classic-asset-movement reconstruction (+ pre-P18 ClaimAtom coverage) — research
last_verified: 2026-10-05
status: research — consumed by ADR-0047 (Accepted); Phase 1 shipped 2026-07-10
---

# Pre-P23 classic-asset-movement reconstruction — research

Evidence base for [ADR-0047](../adr/0047-pre-p23-classic-movement-reconstruction.md)
(Accepted), which holds the decisions (storage, writer, `movement_kind`,
phasing, verification, precedent). This page keeps the evidence and
reasoning. Pipeline basics: [ingest-pipeline.md](ingest-pipeline.md).

Shipped: Phase 1 (Payment + CreateAccount), 2026-07-10 (`13b6db0d`), as
`stellarindex-ops classic-movements-backfill`, writing ClickHouse
`stellar.account_movements` only (Postgres `classic_movements`, migration
0105, was superseded by ADR-0048 D2 and dropped in migration 0113).

Later check (INV-0967/INV-1128, r1 ClickHouse, 2026-09-27): `min(ledger_seq)` of
`stellar.ledger_entry_changes` is 3 and `min(ledger)` of `stellar.account_movements`
is 3, so both reach genesis; the §3.2 table is the earlier (2026-07) measurement
that showed the gap.

Method: `go-stellar-sdk@v0.6.0` XDR, our code, and bounded read-only
queries on r1's ClickHouse (`stellar` DB, HTTP `:8123`).

## 0. TL;DR

- **The gap is narrower than it looks.** Of 27 classic `OperationType`s, 15
  move value. **11 reconstruct from `stellar.operations` +
  `stellar.operation_results` alone** (populated to genesis). Only
  `LiquidityPoolDeposit`/`LiquidityPoolWithdraw` (and one CAP-0038 revocation
  edge) need `ledger_entry_changes`; their results carry no amounts.
- **`stellar.ledger_entry_changes` is not backfilled pre-P23.** The extractor
  exists and is wired into `ch-backfill`; it has not been run over history.
  Live-fidelity rows start at about ledger 61,996,000 (~2026-04-06). A
  scheduling gap, not an engineering gap, and the top prerequisite (§3.2).
- **ClaimableBalance create/claim/clawback needs no new substrate**
  (`CreateClaimableBalanceResult` carries `BalanceId`; correlate against our
  create table). **`AccountMerge` neither**
  (`AccountMergeResult.SourceAccountBalance` is the exact XLM).
- **Trap (§4):** `stellar-etl`'s effects code descends from Horizon's internal
  processors; reuse breaches ADR-0001's spirit. Build on
  `go-stellar-sdk/ingest`, already used in
  `internal/storage/clickhouse/extract.go`.
- **Volume:** pre-P23 (ledger < 58,762,517) is exactly **20,297,622,756**
  operations (`sum(op_count)` over `stellar.ledgers`); the per-type split is
  sampled, order-of-magnitude only (§5).

---

## 1. Problem statement recap

From P23 (Whisk, mainnet 2025-09-03, ledger **58,762,517**) every classic
movement emits a CAP-67 transfer/mint/burn event that
`internal/sources/sep41_transfers` and `sep41_supply` decode. Before P23,
classic movements are implicit in operation results and ledger-entry deltas
(what Horizon "effects" read). ADR-0001 bans Horizon, so the history must be
reconstructed from our raw lake (ADR-0034).

Boundaries (`stellar.ledgers`, one row per ledger):

| Protocol | First ledger | First close (UTC) |
| --- | --- | --- |
| P17 (CAP-0035 `SetTrustLineFlags`) | 35,687,508 | 2021-06-01 |
| P18 (CAP-0038 AMM / liquidity pools) | 38,115,806 | 2021-11-03 |
| P19 | 41,232,715 | 2022-06-08 |
| **P20 (Soroban)** | **50,457,424** | **2024-02-20** |
| P21 | 52,180,958 | 2024-06-18 |
| P22 | 54,700,475 | 2024-12-05 |
| **P23 (unified CAP-67 events)** | **58,762,517** | **2025-09-03** |
| P24 | 59,501,299 | 2025-10-22 |

Lake tip then: ledger 63,426,244; pre-P23 is **92.6%** of the lake.

**ClaimAtom ("pre-P18") framing.** All three variants are classic DEX
primitives already decoded by `internal/sources/sdex`; not part of this gap:
`V0` (pre-CAP-27, inherited from the `decode.go` comment, no boundary ledger
re-derived; `internal/sources/sdex/decode.go:148-165`, seller G-address from
raw ed25519), `OrderBook` (post-CAP-27, `decode.go:126-133`), `LiquidityPool`
(P18, 2021-11-03, `decode.go:134-146`, pool ID hex stored as `Maker`). Not
covered: LP deposit/withdraw (liquidity entering/leaving a pool, as opposed
to a trade against it), which needs `ledger_entry_changes`.

---

## 2. Movement-type inventory

All 27 `xdr.OperationType`s (`go-stellar-sdk@v0.6.0`
`xdr/xdr_generated.go:26523-26549`).

Path key: **(a)** op body alone (after success code); **(b)** body+result
(amounts live in the result); **(c)** `ledger_entry_changes` (before/after
entry deltas are the only truth); **(b+own-index)** body+result correlated
against our own previously derived record of a related op; **none**.

| Operation | Moves value? | Path | Notes |
| --- | --- | --- | --- |
| `CreateAccount` | Yes (source to new account) | (a) | `CreateAccountOp{Destination, StartingBalance}`; result is a bare code. |
| `Payment` | Yes | (a) | `PaymentOp{Destination, Asset, Amount}`; result is a bare code. |
| `PathPaymentStrictReceive` | Yes | (b) | `Success{Offers []ClaimAtom, Last SimplePaymentResult}`; dest amount exact (`Last.Amount`); body `SendMax` is a ceiling. SDEX already decodes the `Offers` for trade legs; the payment framing (X sent, Y received) is the new piece. |
| `PathPaymentStrictSend` | Yes | (b) | Symmetric; body `DestMin` is a floor, actual from `Last.Amount`. |
| `ManageSellOffer`, `ManageBuyOffer`, `CreatePassiveSellOffer` | Yes, if crossing | (b) | Already decoded by `internal/sources/sdex` into `trades`; not a new movement type. |
| `CreateClaimableBalance` | Yes (source to escrow) | (a) | `{Asset, Amount, Claimants}`; `Result.BalanceId` populated on success (`xdr_generated.go:43755-43757`), no correlation needed. |
| `ClaimClaimableBalance` | Yes (escrow to claimer) | (b+own-index) | Body only `BalanceId`, result a bare code. Amount/asset come from our derived create record; fallback and cross-check: the `removed` `ClaimableBalanceEntry` row in `ledger_entry_changes` (has `Asset`+`Amount`). |
| `AccountMerge` | Yes (all remaining XLM, account destroyed) | (b) | `AccountMergeResult.SourceAccountBalance Int64` (`xdr_generated.go:42698,42710`) is the exact amount. |
| `Clawback` | Yes (holder to issuer, destroyed) | (a) | `{Asset, From, Amount}`; result a bare code. |
| `ClawbackClaimableBalance` | Yes (escrow destroyed) | (b+own-index) | Body only `BalanceId`; same correlation as claim. |
| `LiquidityPoolDeposit` | Yes (two assets, depositor to pool) | **(c), no other path** | Body is bounds only (`MaxAmountA/B`, `MinPrice/MaxPrice`). Result is a bare code with zero data fields (`xdr_generated.go:45790-45792`; confirmed by direct inspection). Truth: `LiquidityPoolEntryConstantProduct{ReserveA, ReserveB, TotalPoolShares}` before/after. |
| `LiquidityPoolWithdraw` | Yes (pool to withdrawer, two assets) | **(c), no other path** | Body `Amount` is pool shares burned; `MinAmountA/B` are floors; result a bare code (`xdr_generated.go:46089-46091`). |
| `AllowTrust` (deprecated by `SetTrustLineFlags` at P17; still observed at a low rate through 2025) | Usually no, except the CAP-0038 edge | (c), rare | If revocation deauthorizes an account holding LP-share trustlines, CAP-0038 auto-redeems the shares into **two new `ClaimableBalanceEntry` rows** as a side effect; the amount is only in `ledger_entry_changes` (`created` rows, same op_index). |
| `SetTrustLineFlags` | Same CAP-0038 edge | (c), rare | Same mechanism, modern op. |
| `SetOptions`, `ChangeTrust`, `ManageData`, `BumpSequence`, `Begin`/`EndSponsoringFutureReserves`, `RevokeSponsorship` | No | none | `ChangeTrust` moves the reserve requirement, not a balance; `RevokeSponsorship` changes who pays the reserve. |
| `Inflation` | Historically; disabled since ~P12 | (a) if seen | Dead for P17+; 29 occurrences in one 20k-ledger window near ledger 20M; out of scope. |
| `InvokeHostFunction`, `ExtendFootprintTtl`, `RestoreFootprint` | Soroban, not classic | none | Covered by event decoders (`sep41_transfers`, `sep41_supply`, per-protocol sources); never the gap. |

### 2.1 Fee charges (every transaction, not an operation)

Every successful transaction debits `fee_charged` from the fee source (tx
source, or the fee-bump's fee account). This is `tx.FeeChanges`, tagged
`op_index = -1` in `ledger_entry_changes`
(`internal/storage/clickhouse/extract_entry_changes.go:36-38`), so path (c). Fees accumulate into `stellar.ledgers.fee_pool` with no
redistribution (inflation disabled), so a fee is not a two-party transfer.
It would touch all **8.8B** pre-P23 transactions for near-zero product
value. ADR-0047 D3: not movement rows; serve from
`stellar.transactions.fee_charged`.

---

## 3. Lake sufficiency check

### 3.1 `stellar.operations` + `stellar.operation_results`: full fidelity, genesis to tip

`body_xdr` and `result_xdr` are populated structurally, independent of any
decoder:

- 67 ops in the first 100k ledgers (ledger 3-29,355).
- Exact 1:1 operations-to-results parity in every sampled window, e.g.
  15,420,231 rows in each for ledger 50,000,000-50,020,000.
- `sum(op_count)` from `stellar.ledgers` for `ledger_seq < 58,762,517` is
  **20,297,622,756**: an exact aggregate over the one-row-per-ledger table,
  not a scan of the 10B+-row `operations` table.

Verdict: sufficient for every path-(a)/(b) type (11 value-moving types)
without `ledger_entry_changes`.

### 3.2 `stellar.ledger_entry_changes`: the one gap

**The extractor is correct and shipped.** `extractEntryChanges`
(`internal/storage/clickhouse/extract_entry_changes.go`, called from
`ExtractLedger` at `extract.go:108`) walks `tx.FeeChanges`,
`TxChangesBefore`, every operation's `Changes` and `TxChangesAfter` for
every entry type (account, trustline, offer, data, claimable_balance,
liquidity_pool, contract_data, contract_code, ttl, config_setting) and kind
(created/updated/removed/state); fee and tx-level changes get
`op_index = -1`.

**As measured 2026-07 the historical backfill had not run (since closed, see the note at the top).** Binary search across windows with a
bounded `countIf(op_index >= 0)`:

| Ledger window start | Rows in window | Rows with `op_index >= 0` |
| --- | --- | --- |
| 40,000,000 | 70,074 | 0 |
| 50,000,000 | 24,005 | 0 |
| 55,000,000 | 28,064 | 0 |
| 58,762,517 (P23 start) | 13,778 | 0 |
| 60,000,000 | 22,576 | 0 |
| 61,990,000 | 2,128 | 0 |
| 61,995,000 | 2,764 | 0 |
| 61,999,000 | 9,569,287 | 6,758,348 |
| 62,000,000 | 18,395,229 | 12,823,440 |
| 63,000,000 (near tip) | 29,513,681 | 23,413,804 |

Cutover is between ledger 61,995,000 and 61,999,000 (about **2026-04-06**).
Below it: a sparse legacy feed, `change_type` exclusively `state`, empty
`tx_hash`, `op_index` always `-1`, only trustline/offer/claimable_balance/
data/liquidity_pool entries (no `account` rows); a periodic census from the
classic-supply observers (ADR-0011/0022), not per-operation capture. Above
it: all four change types, populated `tx_hash`/`op_index`, `account` entries
(recent window: 7.79M `account` `state` and 7.79M `account` `updated` rows,
plus full `contract_data`/`ttl`/`contract_code`).

Whole-table `change_type` (one `GROUP BY`, ~4.6 s): `updated` 1.44B,
`state` 1.37B, `created` 159M, `removed` 78M; total **3.05B** rows.
`created`/`updated`/`removed` are post-2026-04-06; pre-boundary rows are
almost all legacy census.

**Sanity check.** `entry_type = 'LIQUIDITY_POOL'` on ledger
50,000,000-50,020,000 (1,056 `LiquidityPoolDeposit` + 168
`LiquidityPoolWithdraw` ops) returned **zero rows**: absent, not under-sampled.

**Verdict.** Sufficient once backfilled (schema and extractor already capture
`ReserveA`/`ReserveB` and before/after balances), but a bulk historical run is
a hard prerequisite for any path-(c) type and a free cross-check for the rest.
Tool: `stellarindex-ops ch-backfill -config PATH -from N -to N [-parallel N]`
(`internal/ops/chops/ch_backfill.go`) walks galexie and calls
`clickhouse.ExtractLedger`; idempotent (`ReplacingMergeTree`). Multi-day, `run-heavy-job.sh`-wrapped, operator-gated.
ADR-0047 D3 sets the range `[38115806, 61999000]`.

### 3.3 Not in the lake

Nothing found; ADR-0033's hash-chained substrate covers pre-P23.

---

## 4. Precedent scan

`VERSIONS.md` pins `stellar/stellar-etl` (`v2.8.18`) and
`withObsrvr/cdp-pipeline-workflow` as reference-only; the latter has known
i128-decoding and SDEX-extraction bugs (AGENTS.md) and stays out entirely.

**4.1 `stellar-etl` / Hubble: the trap.** `stellar-etl` reads
`LedgerCloseMetaBatch` XDR directly (no live Horizon). But `export_effects`
shares lineage with Horizon's `services/horizon/internal/ingest/processors`
(same monorepo, archived 2025-12-16; `internal/` path, not importable).
Borrowing its effects code is a soft ADR-0001 violation.
- Copy: the published effect taxonomy and algorithm shape (diff before/after
  `LedgerEntryChanges`, read cheap amounts from results).
- Avoid: vendoring or porting the code; reimplement on our own tables.
- Hubble (SDF's BigQuery warehouse) is built on `stellar-etl`, same trap; its
  "Transactional Data" vs "Ledger State" split names `stellar.operations` vs
  `stellar.ledger_entries_current` well.

**4.2 `stellar-expert/tx-meta-effects-parser`: the clean precedent.** MIT npm
package; ~80 effect types (`accountDebited`/`accountCredited`, `trade`,
`claimableBalanceCreated`/`Removed`, `liquidityPoolDeposited`/`Withdrew`,
`assetMinted`/`Burned`, ...) from envelope + result + meta XDR, no Horizon.
- Copy: it requires full `LedgerEntryChanges` meta as input, confirming §3.2.
- Copy: its taxonomy (generic debit/credit with `trade`, `liquidityPool*`,
  `claimableBalance*` overlays) matches §2; vocabulary for `movement_kind`.
- Avoid: direct reuse (JavaScript); its input maps 1:1 onto our triple.

**4.3 `go-stellar-sdk/ingest`: already ours.** `LedgerTransactionReader`,
`LedgerTransaction.GetChanges()`, `GetChangesFromLedgerEntryChanges`; already
used by `internal/storage/clickhouse/extract.go`,
`extract_entry_changes.go` and `internal/dispatcher`. Build on it: in
`go.mod`, proven at scale, no Horizon baggage. (`stellar.expert`'s own
indexer is MongoDB-backed and not public; nothing further to take.)

---

## 5. Volume estimate

Order-of-magnitude only: density is wildly non-uniform (bot-driven offer eras
dwarf organic-payment eras). The total is exact (§3.1); the split is
extrapolated from seven 20,000-ledger windows (140,000 ledgers, ~0.24% of the
range) at ledgers 3M / 10M / 20M / 30M / 40M / 50M / 57M, chosen to span
eras, not to be representative (each `GROUP BY op_type` query ran in well
under a second). Sample excluding Soroban: 47.7M classic ops, scaled to the
exact 20.297B.

| Operation | Sample share | Extrapolated pre-P23 |
| --- | --- | --- |
| `ManageSellOffer` | 26.8% | ~5.4B |
| `ManageBuyOffer` | 18.9% | ~3.8B |
| `Payment` | 19.5% | ~4.0B |
| `PathPaymentStrictReceive` | 12.6% | ~2.6B |
| `CreateClaimableBalance` | 7.3% | ~1.5B |
| `ClaimClaimableBalance` | 6.5% | ~1.3B |
| `PathPaymentStrictSend` | 4.4% | ~0.9B |
| `ChangeTrust` (no movement) | 2.6% | ~0.5B |
| `SetTrustLineFlags` (rarely a movement) | 0.33% | ~66M |
| `Clawback` | 0.18% | ~36M |
| `AllowTrust` (rarely a movement) | 0.13% | ~27M |
| `CreateAccount` | 0.12% | ~24M |
| `AccountMerge` | 0.05% | ~9.9M |
| `ClawbackClaimableBalance` | 0.05% | ~9.8M |
| `CreatePassiveSellOffer` (SDEX-covered) | 0.02% | ~3.8M |
| `LiquidityPoolDeposit` | 0.007% | ~1.4M |
| `LiquidityPoolWithdraw` | 0.0017% | ~343K |

- Offers and path payments dominate the count but the trade side is solved
  SDEX territory. New work: `Payment` (~4.0B) + claimable-balance pair
  (~2.8B) + `AccountMerge`/`Clawback`/`ClawbackClaimableBalance` (~76M) is
  roughly **7-8B rows**, not 20B (ADR-0047: archive 10-11B rows).
- LP deposit/withdraw (~1.7M combined) is the only type gated on the
  `ledger_entry_changes` backfill: small blast radius, hard prerequisite.

---

## 6. Phasing

Each phase ships alone; order is by need for the `ledger_entry_changes`
backfill x product value x row cost. Adopted in ADR-0047 D3 (Phase 0 range
narrowed to P18 onward).

- **Phase 0, prerequisite:** `ch-backfill` over the §3.2 gap (`[2, 61999000]`
  here; ADR `[38115806, 61999000]`). No new code; multi-day,
  `run-heavy-job.sh`-wrapped, one job at a time. Unblocks Phase 4 and gives
  every phase a derived-amount vs balance-delta cross-check.
- **Phase 1, `Payment` + `CreateAccount`** (shipped 2026-07-10). Path (a), no
  Phase 0 dependency; ~4.0B + ~24M rows; highest product value. Verification:
  substrate reconcile only (every such op in `stellar.operations` processed).
- **Phase 2, path payments.** Path (b) on SDEX's decoded `ClaimAtom`s; new
  piece is the payment framing keyed to SDEX's `(ledger, tx_hash,
  op_index)`; ~3.5B rows. Dest leg exact from `Last.Amount`; source leg
  cross-checks against the first `ClaimAtom` after Phase 0 (not blocking).
- **Phase 3, ClaimableBalance + `Clawback`.** (a) for create/clawback,
  (b+own-index) for claim/CB-clawback via a `BalanceId -> (asset, amount,
  creator)` index, no `ledger_entry_changes`; ~2.9B rows. Every claim/clawback
  resolving to a known create is a data-quality signal.
- **Phase 4, `AccountMerge` + LP deposit/withdraw + CAP-0038 edge.**
  `AccountMerge` (~10M rows) needs no Phase 0; LP (~1.7M rows) and the edge
  are **hard-gated on Phase 0**. First real ADR-0033-style reconcile: sum of
  derived LP-deposit amounts vs the pool's `ReserveA`/`ReserveB` delta.
- **Phase 5, `fee` rows (~8.8B):** not materialized (§2.1).
- **Cross-cutting:** the enum is closed (27 values): ship a type's whole
  coverage within its phase or not at all.

---

## 7. Constraints and architecture alignment

### 7.1 One writer per data domain (ADR-0031)

Lake-derived, not via `soroban_events` (ADR-0029) or the projector; shaped like
`internal/sources/sdex`: a non-projected `OpDecoder` +
`LedgerEntryChangeDecoder` hybrid (SDEX's `OpContext` pattern) that is the
sole writer of its destination, as `sdex` and the projected Soroban DEXes are
disjoint writers into `trades`. Realized as `internal/sources/classicmovements`
(ADR-0047 D2).

### 7.2 ClickHouse-lake-derived, not a MinIO walk

Every path reads `stellar.operations`/`operation_results`/
`ledger_entry_changes` (the `ch-rebuild` shape). Phase 0 walks galexie, but
that populates the lake's Tier-1 substrate, not a per-decoder dependency.

### 7.3 NUMERIC-exact (ADR-0003)

Classic amounts are `Int64` (7-decimal stroops) and fit `int64`; the i128
concern is Soroban-only and already `*big.Int`-safe in the SEP-41 paths.
Still store exact (NUMERIC/decimal) and serve decimal strings in JSON, with no
raw-integer special case.

### 7.4 Same table (provenance-discriminated) vs parallel table

**Decision: a new table, not a literal extension of `sep41_transfers`,
following the multi-writer shared-destination pattern `trades` proves, not
the strict one-projector-one-table pattern.**

Against writing into `sep41_transfers`:
- Soroban-shaped schema (`ContractID`, SCVal amounts, event-index key). A
  classic `Payment` has an `Asset` and key `(ledger, tx_hash, op_index)`;
  forcing it leaves `ContractID` empty or synthetic.
- It is exclusively projector-written (ADR-0031). A lake-derived,
  non-projected writer there blurs that boundary, though the ranges are
  disjoint (no double-write race).

For keeping the pattern (not disconnected tables):
- Account-activity consumers (account page, `/v1/accounts/{g}/movements`)
  want ONE chronological feed, not a client-side UNION across P23 (Horizon's
  era-shifting feed is the trap).
- `canonical.Trade.Source` proves one table, many writer-sources and a
  provenance column works at scale (`sdex`, `soroswap`, `phoenix`, `blend`,
  `comet`, ...). Here: `movement_kind` plus `provenance` (`classic_derived`
  pre-P23, `cap67_event` post-P23) on a new two-party-movement table.
- `sep41_transfers` stays as is for kinds with no classic equivalent
  (`approve`/`set_admin`/`set_authorized`); the read side merges the two
  into one feed with neither aware of the other at write time.

Adopted in ADR-0047 D1 and its read-time-merge invariant (ADR-0048 D5).

### 7.5 Completeness verification for derived-not-event data

ADR-0033's three legs, simplified by the closed enum:

1. **Substrate continuity:** inherited free. `stellar.ledgers` is contiguous
   and hash-chained to genesis; `operations`/`operation_results` counts
   reconcile to `stellar.ledgers.op_count` (§3.1).
2. **Recognition:** a static test that the decoder's `switch` covers exactly
   the phase's in-scope op types (shape of `matchesTradeOp` in
   `internal/sources/sdex/decode.go`); no ADR-0035-style allowlist, since
   classic ops have no contract-identity question.
3. **Projection reconciliation:** after Phase 0, a periodic job sums derived
   movements per `(account, asset, epoch)` against the balance delta in
   `ledger_entry_changes` (or the result-XDR amount for simple types).
   Phases 1-3: cross-check only; Phase 4 needs it.

---

## 8. Open questions

Status per ADR-0047 D3/D4:

1. **Pre-P18 history for LP?** AMMs did not exist before P18; the question is
   whether Phase 0 reaches ledger 2 or stops at 38,115,806. *Resolved:* ADR
   uses `[38115806, 61999000]`.
2. **CAP-0038 revocation edge: first cut or deferred?** Sample data suggests
   a small share of a small op count, but no query measured it (needs decoding
   `ledger_entry_changes` `entry_xdr`). *Resolved in scope:* Phase 4. Its
   frequency is still unmeasured.
3. **Fee rows in scope?** *Resolved:* no; serve from
   `stellar.transactions.fee_charged`.
4. **New table name/shape?** Depends on what the account-activity page
   renders per row. *Resolved:* `stellar.account_movements`, two rows per
   movement (ADR-0047 D1).
5. **Phase 0 window and cost.** The row cost of a full-history backfill was
   not estimated (the table is 3.05B rows for the ~1.4M ledgers it covers;
   ~62M ledgers is far larger and needs an operator sizing pass). *Window
   resolved* (P18 onward); *cost still open*.

For the account-page lifetime-authority program (INV-2140/INV-2141 A1/A2
movements and entry-changes views): §2 inventory and paths, §3.2 cutover
(before ledger ~61,996,000 `ledger_entry_changes` is census-only), §7.4
single-feed merge.
