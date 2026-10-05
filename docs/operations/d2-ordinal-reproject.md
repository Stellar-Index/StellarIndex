---
title: D2 — in-CH intra_ledger_seq reproject
last_verified: 2026-07-23
status: RETIRED — the formula is the EntryWalkVersion-1 order; do not run (see the notice below)
---

# D2 — in-CH `intra_ledger_seq` reproject

> **RETIRED — do not run `scripts/ops/d2-ordinal-reproject.sh`; it refuses.**
> It ranked each ledger's rows by `(tx_index, change_index)`, the per-transaction
> walk of `dispatcher.EntryWalkVersion` 1. The writer now emits the ledger-wide
> three-phase walk (every tx's fee changes, then every tx's apply phase, then
> every post-apply refund), and no SQL over the lake reproduces it: fee, before,
> after and refund changes all carry `op_index = -1`.
> `TestNoOpsScriptRanksLedgerEntryChangesInSQL` fails any script that ranks
> `ledger_entry_changes` in SQL.

## Queued repair: partitions 39–53

Partitions 39–53 (ledgers 39M–54M, ~76.55 billion rows) were reprojected by
this script before the version-2 walk shipped, so they carry version-1
positions (INV-1313). Re-derive them through the Go walk with
`scripts/ops/ordinal-rederive-chunks.sh` (`ch-backfill`), setting
`START`/`BAND_END` to the range, under `run-heavy-job.sh`, following
[entry-walk-renumbering.md](runbooks/entry-walk-renumbering.md) §1. ch-backfill
writes idempotent RMT rows that supersede by `ingested_at`: no partition swap,
safe beside live ingest. Without `ingestion.live_seam_ledger` configured,
`ch-backfill` reads the live bucket, which does not hold historic ranges; pass
the archive bucket with `-bucket` (the script does not).

## Why the ordinal matters

`ledger_entries_current`'s ReplacingMergeTree must keep the LAST intra-ledger
change to a key (audit C2-4c). Without ordinals a key changed more than once in
a ledger can serve its `state` pre-image as current, and 90.78% of
(ledger, key) pairs have >1 change (every modification emits `state` +
`updated`).

## Facts the D2 run established (still true of the lake)

- **Census rows.** Partitions 39–53 hold legacy rows with no transaction
  (`op_index = -1, tx_hash = '', change_type = 'state', intra_ledger_seq = 0`),
  ~9 million in the range (7,566 of 65,001,834 rows in ledgers
  45,000,000–45,010,000). Any rewrite that inner-joins `transactions` and then
  replaces a partition deletes them permanently. They are removed only by the
  deliberate cleanup `DELETE WHERE op_index=-1 AND tx_hash='' AND change_type='state'`.
- **Read the source `FINAL`.** `ledger_entry_changes` is a ReplacingMergeTree; a
  re-ingested range leaves exact duplicates in unmerged parts (partition 44 held
  11,181,201). A raw read double-counts them, so row counts and per-ledger
  windows go wrong; counts used for verification must be `FINAL` counts too.
  `OPTIMIZE` on these ~300 GiB partitions is blocked by
  `max_bytes_to_merge_at_max_space_in_pool`.
- **Do not validate below ~63,550,000** against stored ordinals: ingest before
  that wrote zeros, so a comparison looks like a total formula failure.
- **Memory.** One window over a whole partition (~6.8B rows) exceeds the 12 GiB
  query-memory cap; ledger sub-range chunks are exact.

`max_partition_size_to_drop` was raised by hand for D2's `REPLACE PARTITION`
and left raised; planned big drops now use the force flag, never a raised
limit: [clickhouse-destructive-ddl.md](clickhouse-destructive-ddl.md).

## Then

D3 (`deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`, windowed,
drop MVs before RENAME) → D4 (`projector-replay`, `derive_generation` guarded) →
cleanup (census DELETE + tx_hash ZSTD) → Phase E prove.
