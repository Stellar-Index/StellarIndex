---
title: Checklist — read a duplicate-bearing lake table
last_verified: 2026-11-21
status: current
---

# Checklist — read a duplicate-bearing lake table

`stellar.transactions` and `stellar.operations` are
`ReplacingMergeTree(ingested_at)` archives, and their duplicate rows are not
merged away. The same holds for every other ReplacingMergeTree in the
`stellar` database — `ledger_entry_changes`, `contract_events`, the state
tables and the rollups — and the gate watches all of them. Measured on r1 2026-09-07 over ledgers
63,000,000–63,099,999: **every one** of the 33,380,486 distinct
`(ledger_seq, tx_index)` keys in `stellar.transactions` carries more than one
row, and `stellar.operations` runs at exactly **2.0x** — 170,836 rows against
85,418 distinct keys over 100k ledgers.

So a `count()`, `sum()` or `groupArray()` over either table reports a multiple
of the truth, and a join to either multiplies the other side by the duplicate
count of each key it matches. None of that is visible in a total, which is what
makes the class expensive: an inflated board still sums to a plausible number.

Guard: `scripts/ci/lint-lake-dedup.sh` (+ its self-test), in CI's
`repo-gates` job and in `scripts/dev/verify.sh`.

## The rule

- [ ] Is the read **aggregating**? A read is in scope when its own select list
      applies an aggregate whose value changes when a row is repeated —
      `count`, `countIf`, `sum`, `sumIf`, `avg`, `avgIf`, `groupArray`,
      `groupArrayIf`, `topK`, `sumMap`, `avgWeighted`, `quantile*`, `median*`,
      `stddev*`, `varPop`, `varSamp`. If it is not, there is nothing to do:
      a per-entity detail reader legitimately returns what the ledger
      contained, row for row.
- [ ] If it is, **collapse the table on its full identity first** — one of:

  | Collapse | Use it when |
  |----------|-------------|
  | `FINAL` on the read | the scanned range is already bounded (a ledger window or a partition) |
  | Two levels: an inner `SELECT … argMax(col, ingested_at) … GROUP BY <identity>` with no aggregate from the list above, the aggregate in the outer `SELECT` | `FINAL` would force a merge of the whole scanned range |
  | `uniqExact((identity))` as the only aggregate | the answer IS a count — this is `count()`'s exact twin |

  Nothing else in the **same** `SELECT` collapses the rows its aggregate
  reads: a `GROUP BY` over the identity puts every version of a key in its
  group, so `sum()` there still adds each version, and `LIMIT 1 BY` and
  `SELECT DISTINCT` run after aggregation. Keep `LIMIT 1 BY` and `DISTINCT`
  for non-aggregating listings and key sets.

- [ ] The identities are the tables' full `ORDER BY` keys
      (`deploy/clickhouse/tier1_schema.sql`) and **a partial key is not a
      collapse**:

      stellar.transactions  (ledger_seq, tx_index)
      stellar.operations    (ledger_seq, tx_index, op_index)
                         or (ledger_seq, tx_hash,  op_index)
      stellar.ledger_entry_changes  (ledger_seq, tx_hash, op_index, change_index)

- [ ] A join collapses **neither side**. Carry the collapse for each
      duplicate-bearing table the statement reads, the same way the window
      predicate is carried twice because a join condition prunes neither side.
- [ ] Resolve a joined column with `argMax(col, ingested_at)` rather than a bare
      equality. `ingested_at` is the tables' version column, so `argMax` returns
      the row the engine's own merge would have kept.

## Worked examples in the tree

| Site | Shape |
|------|-------|
| `internal/storage/clickhouse/account_sponsors_rollup.go` | `GROUP BY o.ledger_seq, o.tx_index, o.op_index` + `HAVING argMax(t.successful, t.ingested_at) = 1` — the operation identity collapses both sides of the join, and the success flag is resolved rather than compared |
| `internal/storage/clickhouse/explorer_reader.go`, `opTypeStatsQuery` | `count()` over a ledger-bounded window with `FINAL` (audit C2-12) |
| `internal/storage/clickhouse/explorer_reader.go`, `accountOpTypeCountsQuery` | `uniqExact((ledger_seq, tx_index, op_index))` where a plain `count()` would inflate per-type totals |
| `deploy/clickhouse/ops_by_source.sql`, Step 3 | `countDistinct(ledger_seq, tx_index)` — the first verify run flagged 20 of 20 accounts before it did |

**Done when:** `./scripts/ci/lint-lake-dedup.sh` passes, and the query has been
run against a window whose duplicate ratio is known — a correct total is not
evidence, because the inflated forms totalled plausibly too.
