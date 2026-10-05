---
title: OPS PLAN — historical intra_ledger_seq backfill
last_verified: 2026-10-05
status: queued (INV-2567, child of INV-2534)
severity: P3 (data-quality debt; served reads are already guarded)
---

# OPS PLAN — historical `intra_ledger_seq` backfill

## Scope

- Per partition (`stellar.ledger_entry_changes`, `PARTITION BY intDiv(ledger_seq, 1000000)`),
  count `(ledger_seq, key_xdr)` groups with `count() > 1 AND
  max(intra_ledger_seq) = 0`. Snapshot/seed rows stamped `MaxUint32` are
  correct — leave them.
- INV-2567 measured the seq>0 drop-off at ~38.11M.

## Re-extraction

`intra_ledger_seq` cannot be reconstructed from existing columns
(`change_index` is per-transaction), so re-derive from tx meta per ledger:
**`stellarindex-ops ch-backfill -write -from N -to M -bucket galexie-archive`**.
`-bucket galexie-archive` is mandatory: the default live bucket is
retention-trimmed to ~the most recent 1M ledgers.

## Tied-key repair

No DELETE needed — this is the BENIGN direction of the version guard.
Re-derived inserts carry `(L << 32) | intra` with `intra > 0`, which strictly
outranks every legacy tie at `(L << 32) | 0`.

## Verification

1. Scoping query → **0** tied `(ledger, key)` groups across repaired partitions.
2. Row-count parity per window (only `intra_ledger_seq`/owner columns change).
3. `stellarindex_sdex_orderbook_crossed_pairs` and
   `stellarindex_sdex_orderbook_pending_offers` → 0.
4. The founding offer ids (845025288 / 845025425 / 845025699 / 845028065)
   are absent from `/v1/sdex/orderbook`.

## Sequencing

Priority order: `[~38M, 63.05M]` first. Windows of ≤1M ledgers, aligned to
partitions, under `/usr/local/sbin/run-heavy-job.sh`. Then remove the order
book's load-time quarantine.

## Rollback

None needed beyond re-running: the walk is deterministic and the append log is idempotent-corrective.
