---
title: SEP-41 completeness diagnosis — why sep41_supply / sep41_transfers sit at complete=false
last_verified: 2026-07-25
status: living
---

# SEP-41 completeness diagnosis

`sep41_supply` and `sep41_transfers` showed `complete=false` with
`lake_complete=true` on `/v1/coverage`. Durable findings, from the code at
`main`. Generic runbook: [`runbooks/completeness-incomplete.md`](runbooks/completeness-incomplete.md).

## 1. Verdict math

`compute-completeness` (`internal/ops/chops/compute_completeness.go`) sets
`lake_complete = substrate ∧ recognition` and `complete = lake_complete ∧ projOK`
(`combineWatermark`). So `lake_complete=true ∧ complete=false` means
`projOK == false`, nothing else. `projOK` goes false at:

| # | Site | Condition | `detail` text |
| - | ---- | --------- | ------------- |
| P1 | `projectionClaim` | this run's reconcile found `delta != 0` | `projection: <table>: N mismatched ledger(s), Σ|Δ|=…, first: ledger=… expected=… served=…` |
| P2 | `projectionClaim` | incremental run, no prior verdict | `…no prior verdict exists to carry…` |
| P3 | `projectionClaim` | incremental run, prior projection FAILING | `…the prior verdict's projection was FAILING — refusing to upgrade without evidence (re-run without -from)` |
| P4 | `projectionClaim` | incremental run, prior clean verdict not contiguous | `…leaving [a,b] verified by nobody…` |
| P5 | `detectFloorLoss` | served rows below the durable floor (migration 0116) | `projection: <table> now begins at ledger N but was previously verified from M…` |

### The latch (P3)

`scripts/ops/completeness-incremental.sh` passes `-from` = `min(watermark)` over
non-recognition sources. The watermark is the lake axis, so on a clean lake it sits at tip and
the hourly run reconciles about `[tip, tip]` (`runFrom > servedFrom`). A source whose prior
`projection_ok = false` therefore hits P3 every hour. By INV-5 design:

- The hourly timer cannot clear a failing projection verdict. Only a full run (no `-from`) can.
- "Still red" says nothing about data health until a full run has been done.
- Each hourly run overwrites `detail`, erasing the original P1 first-mismatch ledger.

## 2. Ruled out

- **C2-11 >4-topic truncation (migration 0114):** SEP-41 decoders read at most `topic[2]`
  (CAP-67 widest form is 4 topics, indices 0..3, all retained), and the `-ch` verdict reads
  `stellar.contract_events` (topic-complete) vs `sep41_transfers` / `sep41_supply_events`, never
  `soroban_events`. A topic-recovery tool would not move this verdict; do not build it for this.
- **Missing pre-Soroban genesis baseline:** `supply seed-sep41-genesis` writes `sep41_supply_rollup`
  (genesis_* columns); the reconcile counts rows in the two event tables
  (`internal/ops/chops/reconciliation_catalogue.go`). Disjoint. Its real symptom is
  `supply.ErrNegativeTotalMissingBaseline` / refresher outcome `missing_baseline`; seed it anyway (step 0).

## 3. If the full run fails with P1: first-mismatch ledger discriminates

| First mismatch | Likely cause | Query |
| -------------- | ------------ | ----- |
| >= ~63,420,000, contiguous | Truncate-boundary hole: the 2026-07-11 `ch-rebuild -sep41 -write` re-derived 50.0M-63.42M after a TRUNCATE; rows in `(63.42M, tip_at_truncate]` were never re-derived | Q4 |
| scattered, `served > expected` | Served rows for contracts outside the watched set: reconcile targets have `whereFilter: ""` while the expected side is prefiltered to `[supply] watched_sep41_contracts` | Q3 |
| scattered, `expected > served` | Failed-tx events counted as expected: `dispatcher.go` and `census.go` skip `!tx.Result.Successful()`, `clickhouse/extract.go` has no success gate. Matters only if other sources are also red | Q5 |
| at a source's `MIN(ledger)` with a P5 line | Durable-floor loss | Q6 |

Watched set: 39 contracts in `configs/ansible/roles/archival-node/defaults/main.yml` (last changed
2026-07-10; the 2026-07-11 re-derive covered all of them). The old `watched_sep41_contracts = []` premise is stale.

## 4. Queries (operator-verify on r1)

**Q1, run first** (Postgres, `STELLARINDEX_POSTGRES_DSN`):

```sql
SELECT source, complete, lake_complete,
       substrate_ok, recognition_ok, projection_ok,
       genesis, watermark, tip, first_problem, updated_at,
       detail
  FROM completeness_snapshots
 WHERE source IN ('sep41_supply', 'sep41_transfers')
 ORDER BY source;
```

Read `detail` against section 1: P3 text means the latch is engaged (go to step 2); `mismatched ledger(s)` is
P1 (take the ledger to section 3); `now begins at ledger` is P5, i.e. served-tier data loss, escalate;
`no prior verdict exists` is P2. `substrate_ok` and `recognition_ok` must be `true`; if not, re-triage.

**Q2** latch confirmation (only the two sep41 rows false, all `updated_at` within the hour):

```sql
SELECT source, projection_ok, tip, updated_at
  FROM completeness_snapshots
 ORDER BY updated_at DESC;
```

**Q3** served rows outside the watched set (expect zero rows; substitute the C-strkeys):

```sql
SELECT 'sep41_transfers' AS tbl, contract_id, count(*) AS rows,
       min(ledger) AS min_ledger, max(ledger) AS max_ledger
  FROM sep41_transfers
 WHERE contract_id <> ALL (ARRAY[ '<C1>', '<C2>', ... ]::text[])
 GROUP BY 1, 2
UNION ALL
SELECT 'sep41_supply_events', contract_id, count(*),
       min(ledger), max(ledger)
  FROM sep41_supply_events
 WHERE contract_id <> ALL (ARRAY[ '<C1>', '<C2>', ... ]::text[])
 GROUP BY 1, 2
 ORDER BY 3 DESC;
```

**Q4** truncate-boundary cliff (repeat for `sep41_supply_events`); zero buckets from ~63.4M until live capture restarted are the signature:

```sql
SELECT (ledger / 100000) * 100000 AS bucket, count(*) AS rows
  FROM sep41_transfers
 WHERE ledger BETWEEN 63000000 AND 64500000
 GROUP BY 1
 ORDER BY 1;
```

Cross-check against the lake (ClickHouse, `clickhouse-client --port 9300`); lake non-zero where Postgres is zero is a real served-tier gap:

```sql
SELECT intDiv(ledger_seq, 100000) * 100000 AS bucket, count() AS events
  FROM stellar.contract_events
 WHERE ledger_seq BETWEEN 63000000 AND 64500000
   AND contract_id IN ( '<C1>', '<C2>', ... )
   AND topic_0_sym IN ('transfer','approve','set_admin','set_authorized')
 GROUP BY bucket
 ORDER BY bucket;
```

**Q5** failed-tx inflation (ClickHouse; expect 0; non-zero is the expected-side over-count):

```sql
SELECT count() AS failed_tx_sep41_events
  FROM stellar.contract_events AS e
 INNER JOIN (
       SELECT ledger_seq, tx_hash
         FROM stellar.transactions
        WHERE successful = 0
          AND ledger_seq BETWEEN 50457424 AND <tip>
 ) AS f USING (ledger_seq, tx_hash)
 WHERE e.ledger_seq BETWEEN 50457424 AND <tip>
   AND e.contract_id IN ( '<C1>', '<C2>', ... )
   AND e.topic_0_sym IN ('transfer','approve','set_admin','set_authorized','mint','burn','clawback');
```

**Q6** durable floors. No rows means no floor was ever recorded (only a clean reconcile records one), so P5 is not the cause:

```sql
SELECT source, target_table, target_filter,
       projection_verified_from, first_recorded_at, updated_at
  FROM completeness_target_floors
 WHERE source IN ('sep41_supply', 'sep41_transfers');

SELECT 'sep41_transfers' AS tbl, min(ledger), max(ledger), count(*) FROM sep41_transfers
UNION ALL
SELECT 'sep41_supply_events', min(ledger), max(ledger), count(*) FROM sep41_supply_events;
```

**Q7** genesis baseline (`genesis_baseline_ledger IS NULL` means never seeded):

```sql
SELECT contract_id, genesis_baseline_ledger, genesis_seeded_at,
       genesis_mint_total, genesis_burn_total, genesis_clawback_total
  FROM sep41_supply_rollup
 ORDER BY (genesis_baseline_ledger IS NULL) DESC, contract_id;
```

## 5. Remedy sequence

**Step 0, genesis seed** (idempotent, independent of the verdict; dry run is the default). Verify with Q7: every
watched contract has `genesis_baseline_ledger` (50457424 by default).

```sh
stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -dry-run
stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -write
```

**Step 1, capture Q1's two `detail` strings** before anything overwrites them. Optionally
`systemctl stop stellarindex-completeness.timer`.

**Step 2, full source-scoped re-verify** (the diagnostic and the only cure for the latch):

```sh
nice -n 15 ionice -c2 -n7 stellarindex-ops compute-completeness \
    -config /etc/stellarindex.toml -ch \
    -source sep41_supply \
    -skip-substrate -skip-recognition

nice -n 15 ionice -c2 -n7 stellarindex-ops compute-completeness \
    -config /etc/stellarindex.toml -ch \
    -source sep41_transfers \
    -skip-substrate -skip-recognition
```

- No `-from`, mandatory; with it you get P3 again.
- `-source` scopes the write to one snapshot row; a typo fails closed (`validateSourceFilter`).
- `-skip-substrate -skip-recognition` sets those axes `true` on trust (carried, not re-proven) and avoids the heaviest scan; drop both for a from-scratch certification.
- `nice`/`ionice` mirror `completeness-incremental.sh`; the full-history sep41 reconcile was one leg of the 2026-07-08 OOM series (fixed by 250k windowing in `clickhouse/completeness.go`).

Re-run Q1. `complete=true` means the latch was the whole story: restart the timer. `complete=false` with P1 means a real gap: use section 3.

**Step 3, repair only after step 2 names a range; never speculatively.**

- Truncate-boundary hole (Q4): additive re-derive of exactly the missing range, no TRUNCATE. SEP-41 is a
  projected source (invariant 7), so replay through the projector; `ch-rebuild -sep41` would be a second writer
  and `ch-rebuild -write` refuses a range the live projector is inside:

  ```sh
  stellarindex-ops projector-replay -config /etc/stellarindex.toml -source sep41_transfers -from <hole_start> -write
  stellarindex-ops projector-replay -config /etc/stellarindex.toml -source sep41_supply -from <hole_start> -write
  ```

  Check the exact source names and range handling in `stellarindex-ops help` and
  [ingest-pipeline.md](../architecture/ingest-pipeline.md#the-replay-decision-rule) first; the supply
  fold checkpoint in `sep41_supply_rollup` must be reset by the write so the aggregator re-folds (the KALE 2x bug).
  See [`sep41-mint-recovery.md`](sep41-mint-recovery.md).
- Un-watched surplus (Q3): code defect. Give the two `reconTarget`s in
  `internal/ops/chops/reconciliation_catalogue.go` a `whereFilter` scoping served rows to the watched set
  (as `trades` is split by `source`). Do not delete rows.
- Failed-tx inflation (Q5): code defect in `internal/storage/clickhouse/extract.go` and/or the reconcile streamer
  (`internal/storage/clickhouse/completeness.go`); it affects every event source, so confirm blast radius first.
- Floor loss (Q6 plus P5): served rows were deleted. Escalate; do not re-record the floor (`recordFloors` refuses so the evidence survives).

**Step 4:** `systemctl start stellarindex-completeness.timer`; the next hourly run carries the clean verdict forward. Confirm with Q1 and `/v1/coverage`.

## 6. Not established

- Which of P1-P5 originally tripped (only the overwritten `detail` knew); Q1 plus step 2 settle it.
- Whether a truncate-boundary hole exists (a hypothesis until Q4 returns).
- Whether failed-tx events appear in operation events under protocol-23 meta; Q5 measures it.
- Anything about served supply values; completeness is a row-count claim.
