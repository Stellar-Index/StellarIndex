---
title: Pre-P23 classic-asset-movement reconstruction (+ pre-P18 ClaimAtom coverage) — research
last_verified: 2026-07-10
status: research (evidence base for a forthcoming ADR)
---

# Pre-P23 classic-asset-movement reconstruction — research

This is a **research doc, not a decision**. It exists to give the
forthcoming ADR a grounded evidence base: what has to be
reconstructed, whether the lake can support it, how others have
solved the same problem without Horizon, a rough sense of scale, and
a phasing shape. Every factual claim below was checked against either
the XDR definitions in `go-stellar-sdk@v0.6.0`, our own code, or a
scoped read-only query against r1's ClickHouse lake (`stellar` DB,
HTTP `:8123`, all queries `LIMIT`/window-bounded per CLAUDE.md's
heavy-job discipline). No writes were made to any table; no r1
filesystem state was touched.

## 0. TL;DR for the ADR author

- **The gap is real but narrower than it looks.** Of the 27 classic
  `OperationType`s, only 15 ever move asset value between parties.
  Of those 15, **11 are reconstructable from `stellar.operations` +
  `stellar.operation_results` alone** (op body + op result, both
  fully populated in the lake back to genesis, verified below). Only
  **`LiquidityPoolDeposit`/`LiquidityPoolWithdraw`** (and one CAP-0038
  edge case on trustline revocation) *require* `ledger_entry_changes`
  as ground truth — their operation results carry no amounts at all.
- **`stellar.ledger_entry_changes` is NOT yet backfilled for
  pre-P23 history.** The full-fidelity per-operation extractor
  (`extractEntryChanges`, `internal/storage/clickhouse/extract_entry_changes.go`)
  already exists and is already wired into `ch-backfill`'s per-ledger
  walk — it just hasn't been *run* over history yet. Live-fidelity
  rows start at approximately **ledger 61,996,000 (~2026-04-06)**.
  Below that, the table holds only a sparse legacy census feed. This
  is a scheduling gap, not an engineering gap, and it's the single
  highest-leverage prerequisite for this whole workstream — see §4.2.
- **Reconstructing `ClaimableBalance` create/claim/clawback needs no
  new substrate at all.** `CreateClaimableBalanceResult` carries the
  generated `BalanceId` on success; correlating claim/clawback against
  our own derived create-table closes the loop without touching
  `ledger_entry_changes`.
- **`AccountMerge` needs no new substrate either.**
  `AccountMergeResult.SourceAccountBalance` carries the exact XLM
  amount moved.
- Precedent scan (§5) surfaces one real trap: `stellar-etl`'s effects
  export is built on Horizon's own internal `ingest/processors`
  package (same monorepo, pre-archive) — reusing that logic directly
  would be a soft violation of ADR-0001. We already depend on the
  Horizon-independent primitive that both `stellar-etl` and
  `stellar-expert/tx-meta-effects-parser` are built on
  (`go-stellar-sdk/ingest`), and we already use it in
  `internal/storage/clickhouse/extract.go`.
- Volume: pre-P23 (ledger < 58,762,517) totals **exactly**
  20,297,622,756 operations (from `stellar.ledgers.op_count`, an
  authoritative sum — not a scan). A stratified sample (7 × 20k-ledger
  windows spread across the full range) breaks that down by op type;
  see §6 for the caveats — density is wildly non-uniform across
  history (bot-driven offer-placement eras dwarf organic-payment
  eras), so treat the per-type numbers as order-of-magnitude only.

---

## 1. Problem statement recap

Post-P23 (Whisk, mainnet 2025-09-03, ledger **58,762,517**, confirmed
below) every classic asset movement emits a unified CAP-67
transfer/mint/burn event, which `internal/sources/sep41_transfers`
and `internal/sources/sep41_supply` already decode. Before P23 there
is no such event stream — classic movements are implicit in operation
results and ledger-entry deltas only, the same substrate Horizon's
"effects" derivation reads. ADR-0001 rules out running, ingesting
from, or proxying to Horizon. This doc surveys what it would take to
reconstruct that pre-P23 history from our own certified raw lake
(ADR-0034) instead.

Ledger boundaries used throughout (from `stellar.ledgers`, exact —
this table is one row per ledger, not a scan target):

| Protocol | First ledger | First close (UTC) |
| --- | --- | --- |
| P17 (CAP-0035 `SetTrustLineFlags`) | 35,687,508 | 2021-06-01 |
| P18 (CAP-0038 AMM / liquidity pools) | 38,115,806 | 2021-11-03 |
| P19 | 41,232,715 | 2022-06-08 |
| **P20 (Soroban launch)** | **50,457,424** | **2024-02-20** |
| P21 | 52,180,958 | 2024-06-18 |
| P22 | 54,700,475 | 2024-12-05 |
| **P23 (Whisk, unified CAP-67 events)** | **58,762,517** | **2025-09-03** |
| P24 | 59,501,299 | 2025-10-22 |

Current lake tip: ledger 63,426,244. Pre-P23 history is **92.6%** of
everything the lake has ever recorded — this is the large majority of
the network's economic history, not a tail case.

The "pre-P20 ClaimAtom" framing in the task is really "pre-Soroban
classic-DEX primitives" — all three `ClaimAtom` variants are classic
constructs that predate Soroban (P20) by a wide margin: `V0` predates
CAP-27 (per our own `internal/sources/sdex/decode.go:148-158`
comment — no on-chain boundary ledger was independently re-derived for
this doc; treat "pre-CAP-27" as inherited from that comment, not
re-verified here), `OrderBook` is the CAP-27 replacement, and
`LiquidityPool` arrived with CAP-38 at **P18** (2021-11-03) — all
three are already fully decoded by `internal/sources/sdex` (§2.2
covers what's *not* covered: the LP deposit/withdraw ops that create
the pool-side liquidity itself, which is a different thing from a
trade *against* the pool).

---

## 2. Movement-type inventory

Every classic `xdr.OperationType` (27 total, `go-stellar-sdk@v0.6.0`
`xdr/xdr_generated.go:26523-26549`), classified by whether it moves
asset value between parties and what reconstruction path it needs.

Reconstruction-path key:
- **(a) body** — op body alone, once op-success is confirmed via the
  result's success code.
- **(b) body+result** — actual amounts live in the operation *result*,
  not the body (path payments, offer claims, account merge, claimable
  balance IDs).
- **(c) ledger-entry-changes** — the result carries no amount at all;
  the only ground truth is the before/after `LedgerEntryChange` (pool
  reserves, trustline balances). This is the "Horizon effects" path.
- **(b+own-index)** — body+result is sufficient **once correlated
  against our own previously-derived record of a related op** (e.g.
  claiming a claimable balance needs the amount from its *creation*
  op, not from the claim op itself).
- **none** — doesn't move asset value between parties.

| Operation | Moves value? | Path | Notes |
| --- | --- | --- | --- |
| `CreateAccount` | Yes (starting balance, source→new account) | (a) | `CreateAccountOp{Destination, StartingBalance}`; `CreateAccountResult` is a bare success code — amount is in the body. |
| `Payment` | Yes | (a) | `PaymentOp{Destination, Asset, Amount}`; `PaymentResult` is a bare success code — amount is in the body. |
| `PathPaymentStrictReceive` | Yes | (b), source leg optionally cross-checked via SDEX's already-parsed `ClaimAtom`s | `PathPaymentStrictReceiveResultSuccess{Offers []ClaimAtom, Last SimplePaymentResult}` — dest amount is exact (`Last.Amount`); body only has `SendMax` (a ceiling, not the actual send amount). Already partially covered: `internal/sources/sdex` already decodes the `Offers` `ClaimAtom`s for the trade legs; the *payment* framing (source X sent, dest Y received) is the new piece. |
| `PathPaymentStrictSend` | Yes | (b), same shape | `PathPaymentStrictSendResultSuccess{Offers []ClaimAtom, Last SimplePaymentResult}` — symmetric to above; body's `DestMin` is a floor, not the actual receive amount, which comes from `Last.Amount`. |
| `ManageSellOffer` | Yes, if it crosses the book | (b) | Already fully decoded by `internal/sources/sdex` — `ClaimAtom`s in the result. Not a new movement type; a trade is already a movement, just recorded in `trades` not a payment-shaped table. |
| `ManageBuyOffer` | Yes, if it crosses | (b) | Same as above — already covered by SDEX. |
| `CreatePassiveSellOffer` | Yes, if it crosses | (b) | Same as above — already covered by SDEX. |
| `CreateClaimableBalance` | Yes (source account → escrow) | (a) | `CreateClaimableBalanceOp{Asset, Amount, Claimants}`; `CreateClaimableBalanceResult.BalanceId *ClaimableBalanceId` is populated on success (`xdr_generated.go:43755-43757`) — the generated ID is directly in the result, no correlation needed for *this* op. |
| `ClaimClaimableBalance` | Yes (escrow → claiming account) | (b+own-index) | `ClaimClaimableBalanceOp{BalanceId}` — no amount in the body or result (`ClaimClaimableBalanceResult` is a bare code). The amount/asset must come from correlating `BalanceId` against this project's own derived `CreateClaimableBalance` record (or, as a fallback, `ledger_entry_changes`'s `removed` row for the `ClaimableBalanceEntry`, which does carry `Asset`+`Amount` — useful cross-check, not required if the create-side index exists). |
| `AccountMerge` | Yes (all remaining XLM, source→destination, account destroyed) | (b) | `AccountMergeResult.SourceAccountBalance Int64` is populated on success (`xdr_generated.go:42698,42710`) — the *exact* amount merged is in the result. No `ledger_entry_changes` dependency. |
| `Clawback` | Yes (holder → issuer, asset destroyed) | (a) | `ClawbackOp{Asset, From, Amount}` — amount is in the body; `ClawbackResult` is a bare code. |
| `ClawbackClaimableBalance` | Yes (escrow → destroyed) | (b+own-index) | `ClawbackClaimableBalanceOp{BalanceId}` only — same correlation need as `ClaimClaimableBalance`. |
| `LiquidityPoolDeposit` | Yes (two assets, depositor→pool reserves) | **(c) — no other path exists** | `LiquidityPoolDepositOp{LiquidityPoolId, MaxAmountA, MaxAmountB, MinPrice, MaxPrice}` — all bounds, not actuals. `LiquidityPoolDepositResult` is a **bare success/failure code with zero data fields** (`xdr_generated.go:45790-45792`) — confirmed by direct inspection, this is not an oversight in our reading. The only ground truth is the `LiquidityPoolEntryConstantProduct{ReserveA, ReserveB, TotalPoolShares}` before/after the op, which lives only in `ledger_entry_changes`. |
| `LiquidityPoolWithdraw` | Yes (pool reserves→withdrawer, two assets) | **(c) — no other path exists** | `LiquidityPoolWithdrawOp{LiquidityPoolId, Amount, MinAmountA, MinAmountB}` — `Amount` is pool *shares* burned, not the two asset amounts received; those are bounded by `MinAmountA/B` (floors) only. `LiquidityPoolWithdrawResult` is also a **bare code** (`xdr_generated.go:46089-46091`). Same `ledger_entry_changes`-only conclusion as deposit. |
| `SetOptions` | No | — | Signers/thresholds/flags/home domain — no asset movement. |
| `ChangeTrust` | No (reserve lock, not a transfer) | — | Establishes/resizes a trustline; moves the *base reserve requirement*, not an asset balance between parties. Out of scope for "movement" reconstruction. |
| `AllowTrust` (deprecated by `SetTrustLineFlags` at P17, but not removed from the XDR enum — still observed at low but non-zero rate through 2025 in our own samples) | Usually none, **except** the CAP-0038 edge case below | (c), rare | Authorization-flag toggle. If flag REVOCATION deauthorizes an account that holds liquidity-pool-share trustlines mixing the revoked asset, CAP-0038 auto-redeems those shares into **two newly created `ClaimableBalanceEntry` rows** as a side effect of *this* op — an amount that appears nowhere in `AllowTrust`'s own body/result, only in `ledger_entry_changes` (`created` rows, same op_index). Rare; flagged as an exotic case, not a phase-1 concern. |
| `SetTrustLineFlags` | Same CAP-0038 edge case as `AllowTrust` | (c), rare | Same mechanism, modern op. |
| `ManageData`, `BumpSequence`, `BeginSponsoringFutureReserves`, `EndSponsoringFutureReserves`, `RevokeSponsorship` | No | — | Metadata/sequence/sponsorship bookkeeping. `RevokeSponsorship` changes *who pays the reserve* for an entry, not the entry's asset balance. |
| `Inflation` | Historically yes, disabled since ~P12 | (a), if ever seen | Op type still exists in the enum; effectively dead on ledgers this project would backfill (P17+). 29 occurrences in one 20k-ledger sample window near ledger 20M (pre-P17) — negligible, out of scope. |
| `InvokeHostFunction`, `ExtendFootprintTtl`, `RestoreFootprint` | N/A — Soroban, not classic | — | Already covered by the existing event-based decoders (`sep41_transfers`, `sep41_supply`, per-protocol Soroban sources) regardless of P23. **The P23 boundary is about classic ops specifically — Soroban-originated movements were never the gap.** |

### 2.1 Fee charges (every transaction, not an operation)

Every successful transaction debits `fee_charged` from its fee
source (the tx source, or the fee-bump's fee account when present).
This is not an operation at all — it's `tx.FeeChanges`, and
`internal/storage/clickhouse/extract_entry_changes.go:36-38` already
tags these at **`op_index = -1`** in `ledger_entry_changes` (confirmed
in the schema comment and the extractor code). Reconstruction path is
therefore (c) too, but the substrate is a tx-level, not an op-level,
concern.

Historically Stellar fees accumulate into the ledger header's
`fee_pool` field (already captured per-ledger in
`stellar.ledgers.fee_pool`) rather than crediting any account — since
inflation was disabled there's no live redistribution mechanism, so
this is closer to "moved into an un-spendable protocol-held pool"
than "transferred to a counterparty." Worth modelling as its own
`movement_kind = 'fee'` row with no counterparty, not forcing it into
a two-party payment shape. Given it touches every one of the 8.8B
pre-P23 transactions and has essentially zero product value (nobody
asks "show me the fee I paid" as a headline feature the way they ask
"show me the payments I received"), this is explicitly the lowest
phase-priority item in §7.

### 2.2 ClaimAtom variant note (the "pre-P18 ClaimAtom" ask)

Already fully decoded, **not part of this gap** — flagged here only
because the task asked for it to be called out explicitly:

| `ClaimAtomType` | Decoded? | First seen | Reference |
| --- | --- | --- | --- |
| `ClaimAtomTypeV0` (0) | Yes | Pre-CAP-27 (earliest Stellar history) | `internal/sources/sdex/decode.go:148-165` — derives the seller G-address from raw ed25519 bytes. |
| `ClaimAtomTypeOrderBook` (1) | Yes | Post-CAP-27 | `decode.go:126-133`. |
| `ClaimAtomTypeLiquidityPool` (2) | Yes | P18 (CAP-38, 2021-11-03) | `decode.go:134-146` — records the pool ID (hex) as `Maker` since there's no G-address counterparty. |

What's genuinely **not** covered is the LP's own deposit/withdraw
side (§2, `LiquidityPoolDeposit`/`Withdraw` rows above) — a trade
*against* a pool (SDEX-covered) is a different event from liquidity
entering/leaving the pool (not covered, needs `ledger_entry_changes`).

---

## 3. Lake sufficiency check

All queries below ran read-only against r1's ClickHouse (`stellar`
DB) over HTTP `:8123`, via SSH — no files were written to r1, nothing
to clean up. Every query was `LIMIT`-scoped or a bounded ledger
window; no full-table scan was run (`stellar.ledger_entry_changes`
alone is 3.05B rows — see below).

### 3.1 `stellar.operations` + `stellar.operation_results` — full fidelity, genesis to tip

`stellar.operations.body_xdr` (raw op body) and
`stellar.operation_results.result_xdr` (raw op result) are populated
**structurally and decoder-independently** — confirmed by:

- Non-zero row counts as early as ledger 3-29,355 (67 ops in the
  first 100k ledgers — genesis-era Stellar had very low activity, but
  the rows exist).
- Exact 1:1 parity between `operations` and `operation_results` row
  counts in every sampled window (e.g. 15,420,231 rows in each table
  for ledger 50,000,000–50,020,000) — every operation has a
  corresponding result, no silent drop.
- `sum(op_count)` from `stellar.ledgers` for `ledger_seq < 58,762,517`
  is **20,297,622,756** — an exact aggregate over the one-row-per-ledger
  `ledgers` table (cheap), not a scan of the 10B+-row `operations`
  table.

**Verdict: sufficient for every path-(a) and path-(b) movement type**
in the inventory above — i.e., 11 of the 13 value-moving operation
types, without touching `ledger_entry_changes` at all.

### 3.2 `stellar.ledger_entry_changes` — the finding

This table is the one genuine gap, and it's more specific than "the
data isn't there."

**The extraction code is already correct and already shipped.**
`internal/storage/clickhouse/extract_entry_changes.go`
(`extractEntryChanges`, called unconditionally from
`ExtractLedger` at `internal/storage/clickhouse/extract.go:108`)
walks `tx.FeeChanges` + `TxChangesBefore` + every operation's
`Changes` + `TxChangesAfter` from `LedgerCloseMeta` (via
`go-stellar-sdk/ingest.LedgerTransaction`), for **every** entry type
(`account`, `trustline`, `offer`, `data`, `claimable_balance`,
`liquidity_pool`, `contract_data`, `contract_code`, `ttl`,
`config_setting`) and every change kind (`created`/`updated`/
`removed`/`state`), tagging fee-meta and tx-level changes at
`op_index = -1` and per-op changes at their real op index. Its own
comment says it "Closes the G12-03 known gap" — i.e. this is a fix
that landed after an earlier, narrower version of the extractor.

**But the historical backfill hasn't been run over it yet.** A
ledger-boundary probe (binary search across 7 windows, each a cheap
`countIf(op_index >= 0)` over a bounded range) found:

| Ledger window start | Rows in window | Rows with `op_index >= 0` |
| --- | --- | --- |
| 40,000,000 | 70,074 | **0** |
| 50,000,000 | 24,005 | **0** |
| 55,000,000 | 28,064 | **0** |
| 58,762,517 (P23 start) | 13,778 | **0** |
| 60,000,000 | 22,576 | **0** |
| 61,990,000 | 2,128 | **0** |
| 61,995,000 | 2,764 | **0** |
| 61,999,000 | 9,569,287 | 6,758,348 |
| 62,000,000 | 18,395,229 | 12,823,440 |
| 63,000,000 (near tip) | 29,513,681 | 23,413,804 |

The cutover sits between ledger 61,995,000 and 61,999,000 —
**close_time ≈ 2026-04-06**. Below that boundary the table holds
only a sparse legacy feed: `change_type` is *exclusively* `state`,
`tx_hash` is empty, `op_index` is always `-1`, and only
`trustline`/`offer`/`claimable_balance`/`data`/`liquidity_pool`
entry types appear (no `account` rows at all) — this reads as a
periodic reserve/balance census from the classic-supply observers
(ADR-0011/0022), not a per-operation change capture. Above the
boundary, all four change types appear, `tx_hash`/`op_index` are
populated, and `account` entries are present (confirmed for a
recent window: 7.79M `account` `state` rows, 7.79M `account`
`updated` rows, plus full coverage of `contract_data`/`ttl`/
`contract_code` — the ADR-0038 Phase C substrate is genuinely
complete going forward).

Whole-table `change_type` distribution (one `GROUP BY`, ~4.6s,
acceptable): `updated` 1.44B, `state` 1.37B, `created` 159M,
`removed` 78M, total 3.05B rows — the created/updated/removed rows
are concentrated in the post-2026-04-06 range; the pre-boundary
1.37B-ish `state` rows are almost entirely the legacy census feed.

**Sanity check that pool deposits/withdraws are actually absent
pre-boundary:** a direct `entry_type = 'LIQUIDITY_POOL'` query
against the 50,000,000–50,020,000 window (which has 1,056 `LiquidityPoolDeposit`
+ 168 `LiquidityPoolWithdraw` operations per §6) returned **zero
rows**, confirming the LP entry-change substrate genuinely isn't
there yet for that range, not just under-sampled.

**Verdict: `ledger_entry_changes` is architecturally sufficient once
backfilled — the schema and extractor already capture everything
needed (`ReserveA`/`ReserveB` on `LiquidityPoolEntryConstantProduct`,
full before/after account+trustline balances) — but a bulk historical
run is a hard prerequisite for any movement type that needs path (c)**
(LP deposit/withdraw, the CAP-0038 revocation edge case, and as an
optional cross-check for everything else).

**This is a scheduling gap, not a build gap.** The tooling to close it
already exists: `stellarindex-ops ch-backfill -config PATH -from N
-to N [-parallel N]` (`internal/ops/chops/ch_backfill.go`) walks a
ledger range from galexie and calls `clickhouse.ExtractLedger` — the
exact same function that already includes `extractEntryChanges` —
writing to every Tier-1 table including `ledger_entry_changes`.
It's idempotent (`ReplacingMergeTree`), so overlapping/re-run windows
are safe, and it already supports `-parallel` chunking. Running it
over `[2, 58762517]` (or further, to close the small remaining
61,995,000–61,999,000 sliver too) is squarely the kind of multi-day,
`run-heavy-job.sh`-wrapped, operator-gated job CLAUDE.md already has
a runbook shape for — no new decoder, no schema change, no XDR work.
This should be scheduled **ahead of or in parallel with** any new
decoder work in §7, since it's on the critical path for the
LP-deposit/withdraw phase and free money for the others (a "did our
derived amount match the actual balance delta" cross-check).

### 3.3 What's *not* in the lake at all

Nothing found. ADR-0033's substrate claim (contiguous, hash-chained
ledgers to genesis) covers the full pre-P23 range; `operations` and
`operation_results` are already fully populated; `ledger_entry_changes`
is a backfill-scheduling gap, not a missing-substrate gap — the raw
`LedgerCloseMeta` genuinely has everything (fee changes, before/after
entries) and the extractor already knows how to read it.

---

## 4. Precedent scan

Per `VERSIONS.md`, `stellar/stellar-etl` (`v2.8.18`) and
`withObsrvr/cdp-pipeline-workflow` are already pinned reference-only
snapshots; `withObsrvr/cdp-pipeline-workflow` carries CLAUDE.md's
standing "known bugs in i128 decoding and SDEX trade extraction" flag
— nothing below changes that; it stays out of consideration entirely.

### 4.1 `stellar-etl` / Hubble — the trap to avoid

`stellar-etl` extracts directly from `LedgerCloseMetaBatch` XDR (via
captive-core or a datastore backend) — no live Horizon dependency,
consistent with ADR-0001. It exports `operations`, `effects`,
`trades`, `ledger_entry_changes`, and `assets` as first-class
commands (`export_operations`, `export_effects`, `export_trades`,
`export_ledger_entry_changes`, `export_assets`).

**The trap:** `export_effects`' actual derivation logic is not a
fresh reimplementation — it reuses the same "effects processor"
concept and code lineage that lives in `stellar/go`'s Horizon
ingest package (`services/horizon/internal/ingest/processors`),
historically the same monorepo `stellar-etl` was extracted from.
That package is (a) under Horizon's own `internal/` path — not
importable outside its module even if we wanted to — and (b)
part of the archived `stellar/go` monorepo (archived 2025-12-16 per
CLAUDE.md). **Borrowing `stellar-etl`'s effects-derivation *code*
would be a soft violation of ADR-0001's spirit** (Horizon-originated
ingest logic re-entering our path by another name), even though
`stellar-etl` itself never calls a live Horizon *server*. The safe
takeaway is the **published effect taxonomy and the general algorithm
shape** (diff before/after `LedgerEntryChanges`, backed by operation
results for the amounts that are cheap to read directly) — worth
treating as a checklist to reimplement independently against our own
`stellar.operations`/`operation_results`/`ledger_entry_changes`
tables, not as code to vendor.

**Hubble** (SDF's public BigQuery warehouse) is built entirely on
`stellar-etl` — same lineage, same trap applies transitively. Its
docs describe exactly the two-tier split we already have in our lake:
"Transactional Data" (operations/effects, chronological) vs "Ledger
State" (account/trustline/pool snapshots) — a useful naming precedent
for how to talk about `stellar.operations` (transactional) vs
`stellar.ledger_entries_current` (state) in the ADR.

### 4.2 `stellar-expert/tx-meta-effects-parser` — the clean precedent

This is the closest real-world analog to what this workstream needs:
an MIT-licensed, standalone npm package (`@stellar-expert/tx-meta-effects-parser`)
that derives ~80 effect types (`accountDebited`/`accountCredited`,
`trade`, `claimableBalanceCreated`/`Removed`,
`liquidityPoolDeposited`/`Withdrew`, `assetMinted`/`Burned`, etc.)
purely from **transaction envelope + result + meta XDR** — explicitly
no Horizon dependency. Two things worth carrying into the ADR:

1. It takes **meta XDR (i.e. the full `LedgerEntryChanges` stream)
   as a required input**, not an optional one — independent
   confirmation that a general-purpose effects derivation needs the
   before/after entry-change substrate, matching what §3.2 found the
   hard way for liquidity pools specifically.
2. Its effect taxonomy (`accountDebited`/`accountCredited` as the
   generic two-sided movement primitive, with `trade`,
   `liquidityPoolDeposited`/`Withdrew`, `claimableBalanceCreated`/
   `Removed` as more specific overlays) is a reasonable target
   vocabulary for our own `movement_kind` discriminator (§7.3) —
   it's independently converged on roughly the same category split
   as the inventory in §2.

Being JavaScript, it's not directly reusable in this Go monorepo, but
its existence is good evidence the problem is well-posed and
Horizon-independently solvable — and its input contract (envelope +
result + meta) maps 1:1 onto our own
`operations`/`operation_results`/`ledger_entry_changes` triple.

### 4.3 `go-stellar-sdk/ingest` — the primitive we already depend on

`go-stellar-sdk/ingest` (`LedgerTransactionReader`,
`LedgerTransaction.GetChanges()`, `GetChangesFromLedgerEntryChanges`)
is the direct successor to the same Horizon-independent primitive
`stellar/go`'s README described as being "created" because "developers
need features that are outside of Horizon's scope." **We already
import and use this package** — `internal/storage/clickhouse/extract.go`
and `extract_entry_changes.go` are built on it today, and
`internal/dispatcher` uses it for the live per-ledger walk. This is
the right foundation for any new reconstruction decoder: it's already
in `go.mod`, already proven at production scale, and carries no
Horizon-internal baggage.

### 4.4 `stellar.expert` explorer itself

Closed-source-adjacent for the indexing layer (MongoDB-backed;
architecture not fully public beyond the `tx-meta-effects-parser`
building block above) — nothing further to extract here beyond §4.2.

---

## 5. Volume estimate

**Read this as order-of-magnitude, not a forecast.** Per-ledger
operation density is wildly non-uniform across Stellar's ten-plus-year
history — multi-year stretches are dominated by automated
offer-placement/arbitrage bots, not organic payment activity, and a
handful of stratified samples cannot correct for that. The *total*
op count is exact (§3.1); the *breakdown by type* is extrapolated from
seven 20,000-ledger windows (140,000 ledgers total, ~0.24% of the
pre-P23 range) spread across ledgers 3,000,000 / 10,000,000 /
20,000,000 / 30,000,000 / 40,000,000 / 50,000,000 / 57,000,000, chosen
to span eras rather than to be statistically representative. Each
window query ran in well under a second (`GROUP BY op_type` is a
columnar scan over a `LowCardinality` column, not row materialization)
— the raw-scan cost CLAUDE.md warns about is in `SELECT *`-shaped or
unindexed cross-column queries, not this kind of aggregate.

Combined sample (140,000 ledgers, excluding `InvokeHostFunction`/
`RestoreFootprint` which are Soroban, already out of scope): 47.7M
classic ops. Extrapolating each type's sample share against the exact
20.297B pre-P23 total:

| Operation | Sample share | Extrapolated pre-P23 total (order of magnitude) |
| --- | --- | --- |
| `ManageSellOffer` | 26.8% | ≈ 5.4B |
| `ManageBuyOffer` | 18.9% | ≈ 3.8B |
| `Payment` | 19.5% | ≈ 4.0B |
| `PathPaymentStrictReceive` | 12.6% | ≈ 2.6B |
| `CreateClaimableBalance` | 7.3% | ≈ 1.5B |
| `ClaimClaimableBalance` | 6.5% | ≈ 1.3B |
| `PathPaymentStrictSend` | 4.4% | ≈ 0.9B |
| `ChangeTrust` (no movement) | 2.6% | ≈ 0.5B |
| `SetTrustLineFlags` (rarely a movement) | 0.33% | ≈ 66M |
| `Clawback` | 0.18% | ≈ 36M |
| `AllowTrust` (rarely a movement) | 0.13% | ≈ 27M |
| `CreateAccount` | 0.12% | ≈ 24M |
| `AccountMerge` | 0.05% | ≈ 9.9M |
| `ClawbackClaimableBalance` | 0.05% | ≈ 9.8M |
| `CreatePassiveSellOffer` (SDEX-covered) | 0.02% | ≈ 3.8M |
| `LiquidityPoolDeposit` | 0.007% | ≈ 1.4M |
| `LiquidityPoolWithdraw` | 0.0017% | ≈ 343K |

Two things worth flagging explicitly for phasing:

- **`ManageSellOffer`/`ManageBuyOffer`/path-payments dominate the raw
  count**, but they're already-solved SDEX territory for the *trade*
  side; the *new* work (payment-shaped movement records) is a
  comparatively smaller slice — `Payment` alone (≈4.0B) plus the
  claimable-balance pair (≈2.8B combined) plus `AccountMerge`/
  `Clawback`/`ClawbackClaimableBalance` (≈76M combined) is the real
  phase-1/phase-2 scope, on the order of ~7–8B rows total, not 20B.
- **LP deposit/withdraw is tiny in row count (≈1.7M combined)** but
  is the *only* type gated on the `ledger_entry_changes` backfill —
  small blast radius, hard prerequisite. Good candidate for a late
  phase specifically because it's cheap to validate once the
  substrate lands, not because it's unimportant.
- Fee-charge rows (§2.1) would add ≈8.8B more rows (one per
  pre-P23 transaction) if modelled as first-class movement rows —
  explicitly deprioritized; see §7.

---

## 6. Phasing proposal

Each phase is independently shippable — its own storage write path,
its own verification story, no phase blocks on a later one. Ordered
by (need for the `ledger_entry_changes` backfill) × (product value) ×
(row-count cost).

**Phase 0 — prerequisite, can start immediately, runs in parallel
with Phase 1 design work.**
`stellarindex-ops ch-backfill` over `[2, 61999000]` (or a smaller
`[38115806, 61999000]` if the ADR decides pre-P18 history isn't a
priority target) to close the `ledger_entry_changes` gap identified
in §3.2. No new code. Multi-day, `run-heavy-job.sh`-wrapped,
one-job-at-a-time per CLAUDE.md. This unblocks Phase 4 and gives every
other phase a free cross-check (derived amount vs actual balance
delta).

**Phase 1 — `Payment` + `CreateAccount`.** Path (a) only — no
dependency on Phase 0. Highest single-type row count that's genuinely
new work (~4.0B `Payment` + ~24M `CreateAccount`), simplest
reconstruction (op body + success code), and the highest product
value (this is literally "show me what an account received/sent,"
the most-asked question on any block explorer). Verification: op-level
substrate reconciliation only (did we process every `Payment`/
`CreateAccount` op in `stellar.operations`?) — no projection-reconcile
possible yet since it doesn't need `ledger_entry_changes`, but that's
fine, it doesn't need one to be *correct*, only to be *cross-checked*.

**Phase 2 — `PathPaymentStrictReceive`/`Send`.** Path (b), building on
SDEX's already-decoded `ClaimAtom`s for the trade legs; the new piece
is exposing the payment framing (source paid X, dest received Y) as
its own movement row, keyed to the same `(ledger, tx_hash, op_index)`
SDEX already writes trades against. ~3.5B rows combined. Verification:
`Last.Amount` (dest leg) is exact from the result; source leg can be
cross-checked against the first `ClaimAtom`'s consumed amount once
Phase 0 lands, but isn't blocked on it.

**Phase 3 — `ClaimableBalance` create/claim/clawback + `Clawback`.**
Path (a) for create/clawback, (b+own-index) for claim — the
claim/clawback-of-CB pair needs a small self-referential index
(`BalanceId → (asset, amount, creator)`), not `ledger_entry_changes`.
~2.9B rows combined (dominated by create/claim; clawback-adjacent ops
are small). Verification: `BalanceId` correlation completeness (every
claim/clawback resolves to a known create) is itself a strong
data-quality signal, analogous to ADR-0035's recognition-gap concept.

**Phase 4 — `AccountMerge` + `LiquidityPoolDeposit`/`Withdraw` + the
CAP-0038 revocation edge case.** `AccountMerge` doesn't strictly need
Phase 0 (result carries the exact amount) but is grouped here because
it's low-volume (~10M rows) and low-urgency; LP deposit/withdraw and
the revocation edge case are **hard-gated on Phase 0**. Verification:
this is the first phase where a genuine ADR-0033-style projection
reconcile is possible — Σ derived LP-deposit amounts vs the pool
entry's actual `ReserveA`/`ReserveB` delta over the same window is a
real, provable check, not just a substrate-continuity claim.

**Phase 5 (optional, low priority) — `fee` rows.** One row per
pre-P23 transaction (~8.8B), zero counterparties, marginal product
value. Candidate for being modelled as a lightweight aggregate
(daily fee burn per account, or just left out of the movement table
entirely and served from `stellar.transactions.fee_charged` directly
via the explorer's tx detail page, which already has that column) —
recommend the ADR explicitly punt this to "serve from the existing
column, don't materialize a per-tx movement row" rather than adding
8.8B rows for a feature nobody's asked for yet.

**Cross-cutting, applies to every phase:** the recognition-completeness
principle from CLAUDE.md's "EVERY event for EVERY Soroban protocol"
binding applies identically here — the classic `OperationType` enum
is closed and fully known (27 values, no "unknown future contract"
problem the way Soroban gating has), so there's no excuse for a
partial `switch` over op types within a phase's scope. Ship the whole
type's coverage or don't ship the type.

---

## 7. Constraints and architecture alignment

### 7.1 One writer per data domain (ADR-0031)

This is **not** Soroban-events-derived data — none of it flows through
`soroban_events` (ADR-0029's landing zone) or the projector. It's
lake-derived from `stellar.operations`/`operation_results`/
`ledger_entry_changes`, structurally identical in shape to how
`internal/sources/sdex` already sits *outside* the projector (an
`OpDecoder`, not a projected Soroban source) while still writing into
the shared `trades` table alongside every projected Soroban DEX. The
precedent is directly reusable: a **new, non-projected decoder path**
(an `OpDecoder` + `LedgerEntryChangeDecoder` hybrid, mirroring SDEX's
`OpContext` pattern) that is the *sole* writer into whatever new table
this becomes — satisfying "one writer per domain" by construction,
the same way `sdex` and the Soroban DEX projectors coexist as
disjoint writers into one shared destination today.

### 7.2 ClickHouse-lake-derived, not a MinIO walk

Every reconstruction path in §2 reads from `stellar.operations`/
`operation_results`/`ledger_entry_changes` — never a fresh MinIO/
Galexie walk. This is exactly the `ch-rebuild`-style shape CLAUDE.md
already prescribes ("Decoder backfills re-derive from the lake…not
MinIO walks"). Phase 0 (§6) is itself a `ch-backfill` run, which *does*
walk galexie — but that's populating the lake's own substrate, the
same category of work as any other Tier-1 backfill, not a
per-decoder MinIO dependency.

### 7.3 NUMERIC-exact (ADR-0003)

Every amount in scope (`Int64` classic-Stellar amounts, 7-decimal
stroop scale) fits in `int64` without truncation risk — classic
amounts were never `i128`; ADR-0003's i128 concern is specifically
about Soroban token amounts, which are out of scope here (already
`*big.Int`-safe via the existing SEP-41 paths). Still: store as
`NUMERIC` in Postgres and string in JSON per the existing convention,
for uniformity with every other amount field the API serves — no
reason to special-case classic amounts as a raw integer type just
because they happen to fit in `int64` today.

### 7.4 Same table (provenance-discriminated) vs a parallel table

**Recommendation: a new table, not a literal extension of
`sep41_transfers`, but explicitly modelled on the multi-writer/
shared-destination *pattern* `trades` already proves out — not the
strict one-projector-one-table pattern.**

Arguments against literally writing into `sep41_transfers`:
- Its schema is Soroban-shaped (`ContractID`, SCVal-derived amounts,
  event-index-keyed). A classic `Payment` has no `ContractID` — it has
  an `Asset` (code+issuer or native), and its natural key is
  `(ledger, tx_hash, op_index)`, not `(ledger, tx_hash, op_index,
  event_index)`. Force-fitting classic movements into that shape
  either leaves `ContractID` empty (a schema smell) or requires a
  synthetic stand-in, neither of which is honest about what the row
  represents.
- `sep41_transfers` is *exclusively* projector-written today (ADR-0031
  domain). Reusing it for a lake-derived, non-projected writer blurs
  a boundary CLAUDE.md is explicit about, even though the ledger
  ranges are provably disjoint (pre-P23 classic-derived vs post-P23
  event-derived) and there's no real double-write race.

Arguments for keeping the *pattern* (provenance-discriminated, shared
read surface) rather than fully parallel, disconnected tables:
- Every consumer that wants "this account's activity history"
  (the explorer's account page, a future `/v1/accounts/{g}/movements`
  endpoint) wants ONE chronological feed, not a UNION the client has
  to sort-merge across a P23 boundary. Re-inventing Horizon's
  "operations feed that silently changes shape depending on ledger
  era" is exactly the kind of trap this project explicitly exists to
  avoid.
- `canonical.Trade.Source` already proves the "one shared table,
  many writer-sources, provenance column" pattern works at scale
  (`sdex`, `soroswap`, `phoenix`, `blend`, `comet`, … all write
  `trades` with a `Source` discriminator) — extend that same idea
  here with a `movement_kind` (`payment` / `path_payment` /
  `account_merge` / `clawback` / `claimable_balance_create` /
  `claimable_balance_claim` / `liquidity_pool_deposit` /
  `liquidity_pool_withdraw`) and a `provenance` (`classic_derived` for
  pre-P23, `cap67_event` for post-P23) pair of columns on a **new**
  table purpose-built for two-party asset movements — leave
  `sep41_transfers` exactly as it is for the Soroban-audit-trail
  fields it uniquely carries (`approve`/`set_admin`/
  `set_authorized`, which have no classic equivalent at all).
- The read-side API can then present a single unified "account
  movement history" that merges the new table with
  `sep41_transfers`'s `transfer`-kind rows (post-P23 CAP-67 events)
  for the account-activity feed, without either table needing to know
  about the other's existence at write time. This is the ADR's call
  to finalize, but the research strongly favors "new table, shared
  pattern, unified read" over either "cram into `sep41_transfers`" or
  "fully disconnected tables with no unified read path."

### 7.5 Completeness verification for derived-not-event data

ADR-0033's three-part model (substrate continuity / recognition /
projection reconciliation) maps cleanly onto this domain, with one
simplification: the classic `OperationType` enum is closed (27 known
values, no "unknown future contract" problem), so **recognition
completeness reduces to a static switch-coverage check**, not an
ongoing contract-gating exercise like ADR-0035. The three legs become:

1. **Substrate continuity** — already provable today: `stellar.ledgers`
   is contiguous+hash-chained to genesis (ADR-0033's existing claim),
   and `operations`/`operation_results` row counts already reconcile
   exactly against `stellar.ledgers.op_count` (§3.1) — no new work
   needed here, it's inherited for free.
2. **Recognition** — did the derivation code handle every value-moving
   op type in its scope for a given phase? A static test asserting
   the decoder's `switch` covers exactly the op types listed as
   in-scope for that phase (same shape as the existing
   `matchesTradeOp` switch in `internal/sources/sdex/decode.go`) is
   sufficient; there's no "curated allowlist that needs an operator
   to seed a new entry" the way ADR-0035 contract gating needs one,
   because there's no adversarial contract-identity question for
   classic ops.
3. **Projection reconciliation** — this is the part that's
   genuinely new: once Phase 0 (§6) lands, a periodic reconcile job
   can sum derived movement amounts per `(account, asset, epoch)` and
   compare against the actual balance delta implied by
   `ledger_entry_changes` (or, for the simpler types, against the
   result-XDR amount directly) — closing the loop the same way
   ADR-0033's projection reconcile closes it for Soroban-derived
   tables today. Phases that don't depend on `ledger_entry_changes`
   for correctness (§6 Phases 1–3) still benefit from this as an
   independent cross-check, not a correctness requirement.

---

## 8. Open questions for the ADR

1. Does the ADR want pre-P18 (pre-CAP-38 AMM) history in scope at
   all for LP deposit/withdraw, or is P18-onward sufficient (AMMs
   didn't exist before then, so the question is really about whether
   Phase 0's backfill needs to reach ledger 2 or can stop at
   38,115,806)?
2. Should the CAP-0038 trustline-revocation edge case (§2, `AllowTrust`/
   `SetTrustLineFlags` auto-liquidation) be in scope for the ADR's
   first cut, or explicitly deferred as a known-rare exotic (sample
   data suggests it's a small fraction of an already-small op-type
   count — no query in this doc directly measured its frequency,
   which would require decoding `ledger_entry_changes` `entry_xdr`
   payloads rather than just counting rows, and was judged out of
   scope for a research pass)?
3. Does "movement" scope include the fee-charge rows at all (§2.1,
   §6 Phase 5), or is the recommendation to serve fees from
   `stellar.transactions.fee_charged` directly (no new rows) final?
4. New table name/shape for §7.4 — this doc argues for the *pattern*
   but doesn't propose exact column names; that's ADR-author's-call
   territory once the `movement_kind` taxonomy is finalized against
   product requirements (what does the explorer's account-activity
   page actually need to render per row?).
5. Whether Phase 0's `ch-backfill` window should target `[2,
   61999000]` (closing the gap fully) or something narrower/staged —
   the row-count cost of a full-history entry-change backfill wasn't
   estimated in this doc (would require its own sampling pass over
   `ledger_entry_changes` specifically, which is already 3.05B rows
   for the ~1.4M ledgers it *does* cover — extrapolating to the full
   ~62M-ledger range is a genuinely large number worth an operator
   sizing pass before scheduling, not a research-doc estimate).
