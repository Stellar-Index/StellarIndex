---
title: Lake deduplication — operator runbook
last_verified: 2026-10-05
status: runbook
---

# Lake deduplication

Driver: `deploy/clickhouse/lake-dedup-driver.sh` (its header holds the evidence trail).

## Why

The raw lake was ingested twice (partial June 2026 backfill, full 2026-07-16/17
re-backfill); copies are value-identical except `ingested_at`. ReplacingMergeTree
dedups only on merge, and write-cold partitions (5-9 active parts) never merge.
Un-`FINAL`'d reads over-count 1.3-2x on four tables; dedup frees ~3 TiB
(ClickHouse-reported; ZFS-realized ~2 TiB, measure at execution). Tip partitions
still need dedup-aware reads.

## Run

One table at a time, biggest reclaim first:

```sh
DRY_RUN=1 /usr/local/bin/lake-dedup-driver transactions     # plan; touches nothing
systemd-run --unit=lake-dedup-transactions --nice=15 /usr/local/bin/lake-dedup-driver transactions
tail -f /var/log/lake-dedup-transactions.log
# repeat for: operations, operation_results, contract_events, ledgers
```

- Stop gracefully: `touch /tmp/lake-dedup.stop` (finishes the in-flight partition); delete the file before resuming.
- Re-running is safe: the driver skips partitions where `count() - uniqExact(<ORDER BY key>)` is 0.
- `ledger_entry_changes` is excluded on purpose (~1.4% dup on 6.17 TiB = 6 TiB rewrite for ~90 GiB).
- Scratch guard: aborts if free space < 3x the next partition (20-40 GB each).
- Never run beside the pre-trim parity check, the completeness audit's `uniqExact` scans or the
  galexie trim (I/O contention). Order: verification, dedup, trim. A crash mid-merge leaves the old parts active.

## Verify

The driver logs `rows_before / rows_after / dup_removed` per partition. Per table:

```sql
SELECT count(), uniqExact(ledger_seq, tx_index) FROM stellar.transactions
-- equal, give or take tip-partition writes
```

Then re-run `/usr/local/bin/lake-completeness-audit-v2`; per-bucket deltas should shrink to the genuine ones.
