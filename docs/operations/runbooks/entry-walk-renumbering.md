---
title: Runbook — entry-walk renumbering (intra_ledger_seq walk-version bump)
last_verified: 2026-10-06
status: draft
severity: P2
---

# Runbook — entry-walk renumbering

## At a glance

Operator-run repair, three steps.

Deploy-time procedure, not a paging condition. Run once per `dispatcher.EntryWalkVersion` bump (current value 3: each `LedgerEntryChanges` block is walked in ledger-key order, `internal/entrywalk.Canonical`).

- Skipped: `ledger_entries_current` rows from the old walk keep a position from a numbering that no longer exists, and a corrective reproject is silently discarded by its RMT version. Balance observations stamp `walk_version` (migration 0199), so a re-derive under the bumped binary lands there; only a renumbering shipped WITHOUT a bump strands them.
- Done wrong: worse than skipping. The seed writes at `MaxUint32`, unbeatable within its `walk_version`; seeding from an un-repaired source makes a stale balance permanently unfixable. Follow the steps in order.

## Why replays lose

`intra_ledger_seq` is a position inside one ledger's walk. Two guards compare it across binary versions: Postgres `account_observations` and siblings (`... WHERE (<t>.walk_version, <t>.intra_ledger_seq) <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)`, migrations 0111, 0199; the trap applies when both rows carry the same `walk_version`), and ClickHouse `ledger_entries_current`'s RMT version `(ledger_seq << 32) | intra_ledger_seq`. A legacy row at position 6 outranks its correction at position 3 (`6 <= 3` is false), and re-running recomputes 3 every time.

| Table | Engine | Partitioning |
| --- | --- | --- |
| `stellar.ledger_entry_changes` (append log) | `ReplacingMergeTree(ingested_at)` | `PARTITION BY intDiv(ledger_seq, 1000000)` |
| `stellar.ledger_entries_current` (projection) | `ReplacingMergeTree(version)`, `version = (ledger_seq << 32) \| intra_ledger_seq` | none, `ORDER BY (entry_type, key_xdr)` |

- The append log needs no drop: a later replay wins on `ingested_at`.
- The projection is unpartitioned (`DROP PARTITION` does not reach it), and a reproject alone does NOT repair it: the corrected row has a lower version and loses under `FINAL`. Its stale rows must be deleted, not out-written.

**Do step 2 (seed) only after step 1 has verified. Seeding a stale balance at `MaxUint32` is strictly worse than the original corruption.**

## 1. Repair the append log (`ledger_entry_changes`)

```
stellarindex-ops ch-backfill -write -config /etc/stellarindex.toml -from {N} -to {M}
```

`ch-backfill` writes the Tier-1 `stellar.*` tables (not `ch-rebuild`). Windowed, under `run-heavy-job.sh`, off-peak; idempotent-corrective, overlapping windows are safe.

### Lake scale on r1 (partitions of `ledger_entry_changes`, "LEC")

Use `ch-backfill -write -changes-only` (keeps `Ledger` and `Changes`; `stellar.ledgers` stays the commit marker; skips the ~1 TiB rewrite of txs/ops/results/events for partitions 58-64). `-to` is inclusive; the wrapper passes `-to hi-1` (`scripts/ops/ordinal-rederive-chunks.sh`). An extract error skips a ledger and fails the run, so a non-zero chunk exit is a real gap. One partition per run; reinstall the wrapper from the repo and check its sha256 against the repo copy first:

```
START=<lo> BAND_END=<hi+1> BUCKET=<archive bucket> EXTRA_FLAGS=-changes-only \
  run-heavy-job.sh ord-<item>-p<N> /usr/local/sbin/ordinal-rederive-chunks.sh
```

Dry-run 1k ledgers first (`stellarindex-ops ch-backfill` without `-write`).

Re-extract EVERY P23+ partition written before `f4bebbfda` (positions drifted in `c4ab63e45..f4bebbfda`). Every gate read of `ledger_entry_changes` uses `FINAL`; a non-FINAL read, or one before the delete, proves nothing. Table (a), restored rows per 2k slice over `[lo, hi]`; save before (2a) and after (6a), after must be >= before on every slice:

```sql
SELECT intDiv(ledger_seq, 2000) AS slice, countIf(change_type = 'restored') AS restored
FROM stellar.ledger_entry_changes FINAL
WHERE ledger_seq BETWEEN lo AND hi
GROUP BY slice
ORDER BY slice WITH FILL FROM intDiv(lo, 2000) TO intDiv(hi, 2000) + 1
```

Pass query (zero rows = PASS). `restored` exists only from P23 (ledger 58,762,517; P20 starts at 50,457,424), so the baseline starts there:

```sql
SELECT b.slice, b.restore_ops, ifNull(r.restored, 0) AS restored
FROM (SELECT intDiv(ledger_seq, 2000) AS slice, count() AS restore_ops
      FROM stellar.operations
      WHERE ledger_seq BETWEEN lo AND hi AND ledger_seq >= 58762517 AND op_type = 'OperationTypeRestoreFootprint'
      GROUP BY slice) AS b
LEFT JOIN (SELECT intDiv(ledger_seq, 2000) AS slice, countIf(change_type = 'restored') AS restored
           FROM stellar.ledger_entry_changes FINAL
           WHERE ledger_seq BETWEEN lo AND hi
           GROUP BY slice) AS r USING (slice)
WHERE ifNull(r.restored, 0) = 0
ORDER BY b.slice
```

The baseline is a lower bound (auto-restore inside `InvokeHostFunction` has no `RestoreFootprint` op), so also require the ledger floor `>= 58,762,517`:

```sql
SELECT minIf(ledger_seq, change_type = 'restored') FROM stellar.ledger_entry_changes FINAL
WHERE ledger_seq BETWEEN lo AND hi
```

Views fed by every re-extract:

| Target | Key / version | Action |
| --- | --- | --- |
| `ledger_entries_current` | RMT(`L<<32 \| seq`), `(entry_type, key_xdr)` | windowed delete + reproject |
| `ttl_live_until` | RMT(`version`), `key_hash` | windowed delete on the version range + reproject |
| `contract_instance_changes` | RMT(`ingested_at`), `(contract_hash, ledger_seq, tx_hash, change_index)` | delete the old rows (orphans) |
| `contract_instance_changes_old` | RMT(`ingested_at`), `(contract_hash, ledger_seq, change_index)` | same delete |

For the two `contract_instance_changes*` deletes exclude the 07-03 rows (`tx_hash = ''` and `ingested_at` on 07-03); `_old` has no `tx_hash`, so exclude by `ingested_at` date alone.

**Per-partition cycle** (one per day, disk permitting):

1. `zfs list -Hp -o avail data` at least 2.0 TiB, else wait for the 01:45 UTC snapshot rotation; start after it.
2. Re-extract with the wrapper. 2a: before it starts, run table (a) on `FINAL` and save it.
3. Gates before any delete, `T` = re-extract start: log shows `ALL CHUNKS DONE`, every chunk exit 0, no `extract ledger` errors; `SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN lo AND hi AND ingested_at >= toDateTime(T)` > 0; `uniqExact(ledger_seq)` in `stellar.ledgers` with `ingested_at >= T` equals `hi-lo+1`; per 100k window no ledger with rows but none newer than `T`; orphan census per 100k window on `ledger_entry_changes FINAL` for rows with `ingested_at < T` (minus 07-03 `tx_hash = ''`): expect 0 on a pre-restored partition and ~45 per 1k ledgers (all `op_index = -1`) on P23+; on a 1k-ledger sample each orphan must have a new twin at a different `change_index` and a `restored` row in its tx. Any unmatched orphan is a STOP.
4. STOP for operator approval. Dry count with the delete's predicate, compare to the expected old-row count (`IN PARTITION <N>` is `_partition_id = '<N>'` here); proceed only on a match and an explicit yes:
   `SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN lo AND hi AND ingested_at < toDateTime(T) AND NOT (tx_hash = '' AND toDate(ingested_at) = '2026-07-03')`
5. With no other heavy job running, delete, then run the two view deletes and reclaim:
   `SET lightweight_deletes_sync=2; DELETE FROM stellar.ledger_entry_changes IN PARTITION <N> WHERE ledger_seq BETWEEN lo AND hi AND ingested_at < toDateTime(T) AND NOT (tx_hash = '' AND toDate(ingested_at) = '2026-07-03');`
   `OPTIMIZE TABLE stellar.ledger_entry_changes PARTITION <N> FINAL`
6. Re-run the orphan census: must be 0. 6a: run the restored gate (table (a), pass query, floor; all `FINAL`) and compare to the 2a snapshot. A failing slice or floor below 58,762,517 is a STOP.
7. With no other heavy job running, repair the projection (below) and verify.

**Projection at lake scale**: Option A below per 100k window ascending, reading `ledger_entry_changes FINAL` and reducing with `argMax` over `(ledger_seq, intra_ledger_seq)`:

```sql
INSERT INTO stellar.ledger_entries_current
    (entry_type, key_xdr, account_id, asset, balance, change_type,
     ledger_seq, close_time, entry_xdr, intra_ledger_seq)
SELECT entry_type, key_xdr, argMax(account_id, v), argMax(asset, v),
       argMax(balance, v), argMax(change_type, v), argMax(ledger_seq, v),
       argMax(close_time, v), argMax(entry_xdr, v), argMax(intra_ledger_seq, v)
FROM (SELECT *, (ledger_seq, intra_ledger_seq) AS v
      FROM stellar.ledger_entry_changes FINAL
      WHERE ledger_seq >= {a} AND ledger_seq < {b})
GROUP BY entry_type, key_xdr
SETTINGS max_bytes_before_external_group_by = 8e9
```

`ttl_live_until` gets the same delete (`version >= a<<32 AND version < b<<32`) and an `argMax` reproject over its own view expressions. Option B (side-by-side) is not used at lake scale.

Pause these timers for the repair (stop `X.timer` after confirming `X.service` is inactive); restart once verified: holders-rollup, cohort-rollup, creators-rollup, sponsors-rollup, census-rollup, issuer-flags, ch-supply, supply-snapshot, supply-verify-rollup, verify-network-state, verify-served-values, compute-completeness, compute-completeness-sdex, compute-completeness-sep41, verify-lake. Leave ch-live-catchup running.

## 2. Rebuild the current-state projection

Needed only for the accounts seed (3b) or `account_state` / asset-holder reads over the range.

**Option A, targeted delete + reproject:**

```sql
ALTER TABLE stellar.ledger_entries_current
  DELETE WHERE ledger_seq BETWEEN {N} AND {M};
```

Watch `system.mutations` to completion BEFORE the insert, or the delete eats rows the reproject just wrote. Then windowed:

```sql
INSERT INTO stellar.ledger_entries_current
    (entry_type, key_xdr, account_id, asset, balance, change_type,
     ledger_seq, close_time, entry_xdr, intra_ledger_seq)
SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
       ledger_seq, close_time, entry_xdr, intra_ledger_seq
FROM stellar.ledger_entry_changes
WHERE ledger_seq >= {window_start} AND ledger_seq < {window_end};
```

**Option B, full side-by-side rebuild** (whole history): follow `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` end to end (build `_v3` + MV, windowed reproject, verify, cutover).

## 3. Reconstruct the FINAL state and seed it

One row per key per ledger, stamped `timescale.SeedIntraLedgerSeq` (`4294967295` = `math.MaxUint32`); the `<=` guard always admits it, so the repair is idempotent-corrective. Never stamp the sentinel on a change-by-change replay: it is sound only for exactly one row per `(key, ledger)`, otherwise the C2-6 out-of-order last-writer-wins bug reopens.

**3a. SAC balances, full-history variant** (reduces `stellar.ledger_entry_changes` directly, never reads the projection; correct once step 1 verifies). Without `-full-history` it reads the un-repaired projection; do not use that here:

```
stellarindex-ops supply seed-sac-balances -config /etc/stellarindex.toml -full-history -write
```

**3b. Account balances** (reads `ledger_entries_current FINAL`, no full-history variant; run only after step 2 verified, else a stale balance is stamped permanently):

```
stellarindex-ops supply seed-observations -config /etc/stellarindex.toml -write
```

## Verification

Verify against the append log, never the projection (comparing an observation to its own source proves nothing). Reference reduction, same as `StreamSACBalanceSeedsFullHistory` (`internal/storage/clickhouse/sac_balance_seed.go`); the `argMax` tuple must match exactly:

```sql
SELECT key_xdr,
       argMax(entry_xdr,   (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_entry_xdr,
       argMax(change_type, (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_change_type,
       argMax(ledger_seq,  (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win_ledger_seq
FROM stellar.ledger_entry_changes
WHERE entry_type = 'account'
  AND ledger_seq BETWEEN {N} AND {M}
GROUP BY key_xdr;
```

1. Step 1: for a key with fee-phase and apply-phase changes, `intra_ledger_seq` in `ledger_entry_changes` is dense, phase-ordered and non-zero (0 = not yet covered by `ch-backfill`).
2. Step 2: `ledger_entries_current FINAL` agrees with the reduction for the same keys; disagreement means the mutation did not complete or the reproject ran first.
3. Step 3: `account_observations.balance_stroops` for the `(account, ledger)` equals the reduction's winner, not `ledger_entries_current`.
4. `SELECT count(*) FROM account_observations WHERE intra_ledger_seq = 4294967295` equals the number of `(key, ledger)` pairs seeded, not more.
5. `stellarindex-ops compute-completeness -ch -write` for the range; a residual mismatch means step 1 did not fully replay.
6. `stellarindex-ops compare-entry-changes -config /etc/stellarindex.toml -from N -to M` must print `"differences":0` (extracts our export and the cold-tier AWS export; a non-zero count names the first differing positions).
7. CAP-0038 revocation legs (`account_movements.leg_index`) are numbered in balance-id order from walk version 3; re-derive classic movements for the range before trusting per-leg counts.

## Do NOT

- Widen the guard to `<` or drop it: reopens C2-6, a continuous bug.
- Re-run the re-derive "harder": it is deterministic; bump `EntryWalkVersion` with the renumbering.
- Run `supply seed-observations` before the projection rebuild verified, or `supply seed-sac-balances` without `-full-history`.
- Treat a reproject as a projection repair, or verify against `ledger_entries_current`.
- Collapse `derive_generation` and `intra_ledger_seq` into one column.

## Residual risk

The seeds write CURRENT state only (one row per key at its latest change's ledger); no tool emits a per-ledger historical final state, so a historical observation repair is not executable end to end. After a binary rollback across a bump, rows stamped with the newer version cannot be displaced by an older-version replay: `DELETE` them for the range or `UPDATE` to `walk_version = 0` first.

## Related

[Migration 0120](../../../migrations/0120_intra_ledger_seq_walk_version.up.sql), [supply-divergence](supply.md#stellarindex_supply_divergence_high), [projector-lag](projector.md#stellarindex_projector_lag_high).
