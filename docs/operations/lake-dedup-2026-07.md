---
title: Lake deduplication — operator runbook
last_verified: 2026-07-25
status: point-in-time audit
---

# Lake deduplication — operator runbook (2026-07)

Companion driver: `deploy/clickhouse/lake-dedup-driver.sh`. Read the
driver's header comment for the full evidence trail; this file is the
execution procedure.

## What happened

The raw lake was ingested twice: a **partial** June-2026 backfill
(transactions reached ~96% of history, operations ~48%, LEC ~1.4%) and
the **full** 2026-07-16/17 re-backfill. The copies are value-identical
— verified by sampling: every checked ledger's duplicate rows agree on
every column except `ingested_at`, and 0 of 1M ledgers in bucket 42M
have disagreeing copies.

ReplacingMergeTree was expected to absorb this, and never did:
**RMT dedups only when parts merge**, historical partitions sit at 5–9
active parts with no write pressure, and the background scheduler has
no reason to merge them. "Eventual" deduplication on write-cold
partitions means **never**. (Same finding family as the audit's
trap 10 — un-FINAL'd reads over-count; this is the storage-side twin.)

## Why run it

1. **~3 TiB of disk** (ClickHouse-reported; ZFS-realized may be
   ~2 TiB — the two accountings don't reconcile perfectly, measure at
   execution). Breakdown in the driver header.
2. **Reader correctness**: any read without `FINAL`/`LIMIT 1 BY`/
   `uniqExact` currently over-counts by ~1.3–2× on four tables. After
   dedup those readers become near-correct on historical partitions
   (tip partitions still need dedup-aware reads — nothing about query
   discipline changes).
3. It makes the full-history completeness audit trivially re-checkable
   (declared-vs-actual with no dup noise), which strengthens the
   galexie-trim gate.

## Execution

Run per table, biggest reclaim first, one table at a time:

```sh
# 1. Plan first — lists dup-candidate partitions, touches nothing:
DRY_RUN=1 /usr/local/bin/lake-dedup-driver transactions

# 2. Execute (background unit; survives SSH):
systemd-run --unit=lake-dedup-transactions --nice=15 \
  /usr/local/bin/lake-dedup-driver transactions

# 3. Watch:
tail -f /var/log/lake-dedup-transactions.log

# 4. Repeat for: operations, operation_results, contract_events, ledgers
```

- **Graceful stop:** `touch /tmp/lake-dedup.stop` — finishes the
  in-flight partition and exits. Delete the file before resuming.
  Re-running is safe and skips already-clean partitions automatically
  (single-ingest-month partitions are never touched).
- **`ledger_entry_changes` is deliberately excluded** — ~1.4% dup on a
  6.17 TiB table is a 6 TiB rewrite for ~90 GiB. Revisit only under
  real pressure.
- **Scheduling:** overnight preferred (it's hours of sustained I/O),
  but partitions are write-cold so ingestion is structurally
  unaffected; the `--nice` and per-partition granularity keep serving
  interference bounded.
- **Scratch guard:** the driver aborts if free space < 3× the next
  partition's size. At ~20–40 GB per partition this passes even at
  94% pool.

## Verification

Per partition, the driver logs `rows_before / rows_after /
dup_removed`. On completion per table:

```sql
SELECT count(), uniqExact(ledger_seq, tx_index) FROM stellar.transactions
-- the two numbers should now be equal (± tip-partition writes)
```

and re-run the identity-based completeness audit
(`/usr/local/bin/lake-completeness-audit-v2`) — its per-bucket deltas
should collapse to the genuine ones only.

## Interactions

- **Do not run concurrently with** the pre-trim parity check, the
  completeness audit's `uniqExact` scans, or the galexie trim — not
  for safety (all are read-only or disjoint) but for I/O contention.
  Sequence: verification jobs → dedup → trim.
- The `OPTIMIZE` merges are the same operation the engine performs on
  its own; crash mid-merge leaves the old parts active (atomic swap on
  completion). The worst case of an interrupted run is "nothing
  changed for that partition".
