---
title: OPS PLAN — historical intra_ledger_seq backfill (zombie-offer root-cause repair)
last_verified: 2026-08-01
status: planned — do NOT execute before launch
severity: P3 (data-quality debt; served reads are already guarded)
---

# OPS PLAN — historical `intra_ledger_seq` backfill

**Root cause being repaired:** every `stellar.ledger_entry_changes` row
written before the 13-column writer deployed carries `intra_ledger_seq = 0`
(the column's `DEFAULT 0`, added by
`deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` Step 0).
`stellar.ledger_entries_current`'s ReplacingMergeTree version is
`(ledger_seq << 32) | intra_ledger_seq`, so every same-ledger change to one
key TIES across that history and FINAL keeps an ARBITRARY survivor. Founding
case (2026-07-31): XLM/USDC offers 845025288 / 845025425 / 845025699 /
845028065, consumed 2021-11-10 at ledger 38224736+, survived as `updated` —
phantom "live" offers serving a crossed book (best bid 0.4327 vs best ask
0.1722; ~46k phantom bids total). The order book now quarantines
`intra_ledger_seq == 0` loads and re-verifies them via
`ExplorerReader.OfferRemovedAt` probes (`internal/api/v1/sdex_orderbook.go`,
`internal/storage/clickhouse/sdex_offer_book_reader.go`) — a per-restart tax
this plan removes. Second symptom, same substrate: `ledger_entries_current`
holds ZERO offer rows below ledger ~38M (not even dead offers' trailing
`removed` states — pointing at the unfinished `[2, ~38.1M]` genesis tranche
of the Phase-0 entry-changes backfill, to be confirmed in scoping).

## Scope

- `stellar.ledger_entry_changes` (append log, RMT(`ingested_at`),
  `PARTITION BY intDiv(ledger_seq, 1000000)`): all rows from the historical
  backfill (Phase 0 genesis + the [62M, 63.05M] seam) and pre-fix live
  ingest carry `intra_ledger_seq = 0` (plus `account_id/asset/balance`
  defaults on the oldest rows). Snapshot/seed rows stamped `MaxUint32` are
  correct — leave them.
- `stellar.ledger_entries_current` (unpartitioned, RMT(version)): the
  version-tied survivors derived from those rows.
- **Scoping queries (run first, cheap per-partition):** find affected
  ranges and the exact upper boundary of the intra=0 era — per partition,
  count `(ledger_seq, key_xdr)` groups with `count() > 1 AND
  max(intra_ledger_seq) = 0`; and confirm whether `[2, ~38.1M]` has ANY
  offer-entry rows at all (gap vs tie — different fix sizes, same tool).

## Re-extraction approach

`intra_ledger_seq` is a per-LEDGER position across the three-phase
ledger-wide walk (all fee changes → all apply-phase meta → all P23
post-apply refunds — `internal/storage/clickhouse/extract_entry_changes.go`,
mirroring `dispatcher.walkLedgerEntryChanges`, `EntryWalkVersion = 2`). It
CANNOT be reconstructed from existing columns (`change_index` is
per-transaction and phases interleave across txs), so the repair is a
re-derive from tx meta per ledger: re-run **`stellarindex-ops ch-backfill
-from N -to M -bucket galexie-archive`** over the affected windows — the
same writer the entry-walk-renumbering runbook designates for append-log
repair. Re-derived rows share the append log's ORDER BY identity
`(ledger_seq, tx_hash, op_index, change_index)`, so RMT(`ingested_at`)
replaces the legacy rows (and repopulates `account_id/asset/balance` as a
bonus). `-bucket galexie-archive` is mandatory: the default live bucket is
retention-trimmed to ~the most recent 1M ledgers.

## Tied-key repair for `ledger_entries_current`

No DELETE needed — this is the BENIGN direction of the version guard
(contrast: docs/operations/runbooks/entry-walk-renumbering.md, where
corrections carry LOWER positions and stale rows must be removed). Every
re-derived insert flows through the MV; the ledger-final change now carries
`(L << 32) | intra` with `intra > 0`, which strictly outranks every legacy
tie at `(L << 32) | 0` — the corrected removal/final-state deterministically
displaces the arbitrary survivor at merge/FINAL. Keys whose final change is
genuinely position 0 tie on identical content.

## Verification

1. Scoping query → **0** multi-change `(ledger, key)` groups with
   `max(intra_ledger_seq) = 0` across repaired partitions.
2. Count parity per window: rows-per-ledger unchanged by the re-derive
   (only `intra_ledger_seq`/owner columns change, never cardinality).
3. `stellarindex_sdex_orderbook_crossed_pairs` → 0 and
   `stellarindex_sdex_orderbook_pending_offers` → 0 after an API restart
   with the quarantine disabled on a canary.
4. Spot checks: the founding four offer IDs absent from `/v1/sdex/orderbook`;
   sample served offers against Horizon's `/offers` (external verification
   oracle only — ADR-0001 bars Horizon from ingest, not from checking;
   precedent: `scripts/ops/reconcile-supply-vs-horizon.sh`).
5. Offer rows (including dead offers' trailing states) appear below ledger
   ~38M once the genesis tranche closes.

## Sequencing

Post-launch heavy job — never before. Windowed (≤1M ledgers,
partition-aligned; `ch-backfill` has no resume flag) under
`/usr/local/sbin/run-heavy-job.sh`, one heavy job at a time, off-peak, with
the CH-log root-fill caution (2026-06-11) in force. Priority order:
`[~38M, 63.05M]` (confirmed intra=0 + offer-active era) → the `[2, ~38.1M]`
tranche → verification pass → remove the order book's load-time quarantine
+ `VerifyPending` path (keep the crossed-pairs gauge as the permanent
tripwire).

## Rollback

None needed beyond re-running: the walk is deterministic (byte-identical
re-derives), the append log's RMT(`ingested_at`) makes overlapping/repeated
windows idempotent-corrective, and the projection's version is monotonic in
the corrective direction. The one hazard is a future walk-ORDER change —
that is an `EntryWalkVersion` bump and follows its own runbook, not this
plan.

## Payoff

Deterministic current-state truth for the whole offer (and every other
entry-type) history: the order book loads the FINAL scan directly — no
per-restart quarantine/re-verification tax, no probabilistic trust
discipline — and `account_state` / asset-holder / SAC-seed reads over the
historical range stop depending on arbitrary tie survivors.
