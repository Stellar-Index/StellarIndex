---
title: D2 — in-CH intra_ledger_seq reproject
last_verified: 2026-07-23
status: in progress — partitions 39–43, 45 done; 44, 46–53 running (FINAL-dedup fix)
---

# D2 — in-CH `intra_ledger_seq` reproject

Restores the per-ledger ordinal on the full-fidelity-but-un-ordinaled rows in
`stellar.ledger_entry_changes`, so `ledger_entries_current`'s ReplacingMergeTree
dedup picks the LAST intra-ledger change to a key rather than an arbitrary one
(audit C2-4c). Without it, a key changed more than once in a ledger can serve its
`state` pre-image as current — and 90.78% of (ledger, key) pairs have >1 change,
because every entry modification emits `state` + `updated`.

## Scope (measured 2026-07-21)

| | |
|---|---|
| Partitions needing it | **39–53** (ledgers 39M–54M) |
| Rows | **76.55 billion** |
| Partition size | ~310 GiB / ~6.8B rows each (partition 45 measured) |
| Already correct | partition 63 tail — live ingest writes ordinals from ledger **~63,550,000** (the 2026-07-19 v0.17.0 deploy) |
| Pool free at planning | 1,804 GiB — staging one partition at a time peaks ~620 GiB |

Cannot be scoped down: sampling 50k ledgers found 90.78% of (ledger, key) pairs
have multiple changes, and **all** 50,001 ledgers had at least one.

## The formula — VALIDATED, do not change without re-validating

```sql
intra_ledger_seq =
  row_number() OVER (PARTITION BY ledger_seq ORDER BY tx_index, change_index) - 1
```

`tx_index` comes from `stellar.transactions` joined on `(ledger_seq, tx_hash)`.

**Why this is right** (`internal/storage/clickhouse/extract_entry_changes.go`):
`entryChangeSeq` is a per-LEDGER counter incremented on every emitted change, in
the canonical walk order — per tx in tx order: fee changes → TxChangesBefore →
per-op changes (op order) → TxChangesAfter. `change_index` is a per-TX counter
incremented in that same walk, so within a tx it fully encodes the position, and
`tx_index` orders the txs. **`op_index` must NOT appear in the ORDER BY** —
op_index is -1 for fee/before/after changes, so sorting by it interleaves
tx-level and per-op changes wrongly (the update→remove bug).

**Empirical proof** — run against ledgers ABOVE ~63,550,000, where live ingest
wrote the ordinals, and compare computed vs stored:

| range | rows | exact match | mismatch |
|---|---|---|---|
| 63,555,000–63,555,050 | 230,814 | 230,814 | **0** |
| 63,570,000–63,570,060 | 305,500 | 305,500 | **0** |

⚠️ **Do not validate below ~63,550,000.** Those ledgers are themselves
un-ordinaled (all zeros), so a comparison there returns ~0 matches and looks like
a catastrophic formula failure when it is merely comparing against absent data.

## CRITICAL: census rows must be preserved

Partitions 39–53 contain legacy census rows that have **no transaction**:

```
op_index = -1, tx_hash = '', change_type = 'state', intra_ledger_seq = 0
```

Measured in ledgers 45,000,000–45,010,000: **7,566 of 65,001,834 rows** (~0.012%),
entry types trustline / offer / claimable_balance / data. Extrapolated across the
D2 range that is roughly **9 million rows**.

An `INNER JOIN` to `transactions` drops every one of them, and because D2 finishes
with `REPLACE PARTITION`, they would be **permanently deleted from the lake**.
They are removed deliberately later, by the cleanup phase
(`DELETE WHERE op_index=-1 AND tx_hash='' AND change_type='state'`) — D2 must not
pre-empt that.

⟹ The reproject is a **UNION ALL of two selects**, never a single inner join:

```sql
INSERT INTO stellar.lec_stage_<P>
-- (1) real rows: recomputed ordinal
SELECT <all columns>,
       row_number() OVER (PARTITION BY lec.ledger_seq
                          ORDER BY t.tx_index, lec.change_index) - 1 AS intra_ledger_seq
FROM stellar.ledger_entry_changes lec FINAL   -- FINAL: dedup re-ingested rows (see below)
INNER JOIN stellar.transactions t
  ON t.ledger_seq = lec.ledger_seq AND t.tx_hash = lec.tx_hash
WHERE lec.ledger_seq BETWEEN <lo> AND <hi> AND lec.tx_hash != ''
UNION ALL
-- (2) census rows: preserved verbatim, excluded from the window
SELECT <all columns>, intra_ledger_seq
FROM stellar.ledger_entry_changes FINAL         -- FINAL here too
WHERE ledger_seq BETWEEN <lo> AND <hi> AND tx_hash = '';
```

No `MaxUint32` seed rows exist in this range (measured 0), so they need no special
handling here — but assert that rather than assume it.

## CRITICAL: dedup the source with `FINAL`

Both source reads — real rows and census — MUST read `stellar.ledger_entry_changes
FINAL` (the transactions join subquery already dedups the tx side with
`argMax(tx_index, ingested_at)`). `ledger_entry_changes` is a
`ReplacingMergeTree`, so a **re-ingested ledger range leaves exact-duplicate rows**
in unmerged parts until a background merge collapses them. Every `FINAL`-less query
already returns the deduped set, but a raw physical read does not.

If the source is read raw when duplicates are present:

- `row_number() OVER (PARTITION BY ledger_seq ...)` counts the duplicated rows, so
  ordinals run `0..2N-1` instead of `0..N-1`;
- the staging `ReplacingMergeTree` then collapses the duplicate keys back down, so
  the stage row count comes out **short** of a raw source count and the contiguity
  check finds gaps — the verification gate aborts (correctly refusing to replace),
  but on a partition that is actually fine once deduped.

This happened on **partition 44** (2026-07-22): ledgers 44,115,806–44,117,805 were
re-ingested, leaving **11,181,201 duplicate rows** (an exact 2× for the affected
ledgers). The run aborted with `stage rows=6,326,108,758 (src 6,337,289,959) …
bad_ledgers=2000` — the 11.18M gap is precisely the dup count. Reading the source
`FINAL` fixed it: deduped source == stage == 6,326,108,758, `bad_ledgers=0`.

Two consequences for the verification gate:

- the source counts (`SRC_TOTAL`, `SRC_CENSUS`) must ALSO be `FINAL` counts, so the
  gate compares stage against the *deduped* source, not the raw doubled count;
- because the stage is built from `FINAL` source and then `REPLACE PARTITION`d in,
  the physical duplicates are **removed** from the lake as a side effect — the
  on-disk partition ends up matching what `FINAL` queries always returned. This is
  correct (RMT would have collapsed them on the next merge anyway) and needs no
  separate `OPTIMIZE` (which is blocked here regardless: the ~300 GiB partitions
  exceed `max_bytes_to_merge_at_max_space_in_pool`).

`FINAL` on a partition with no duplicates is a no-op on the result, so this is safe
for every partition, not just re-ingested ones.

## Chunking is mandatory

A single window over a whole partition (~6.8B rows) exceeds ClickHouse's 12 GiB
query-memory cap — the same wall the substrate scan hit. The window partitions per
`ledger_seq`, so chunking by ledger sub-range is **exact** (no cross-chunk
dependency). A 25k-ledger chunk in partition 45 produced ~310M rows; size chunks
to a few tens of millions of rows.

## Per-partition procedure

1. Create `stellar.lec_stage_<P>` with identical schema/engine
   (`ReplacingMergeTree(ingested_at)`, same ORDER BY).
2. INSERT the UNION ALL above, chunked by ledger sub-range, until the partition
   is covered.
3. **Verify before replacing** — all must hold:
   - `count(stage) == count(source FINAL)` (no rows lost, census included; the
     source count is a `FINAL`/deduped count — see the `FINAL` section above)
   - per-ledger ordinals over non-census rows are contiguous `0..N-1`:
     `GROUP BY ledger_seq HAVING max(intra_ledger_seq)+1 != count() OR uniqExact(intra_ledger_seq) != count()` returns 0 rows
   - census row count in stage == census row count in source
4. `ALTER TABLE stellar.ledger_entry_changes REPLACE PARTITION <P> FROM stellar.lec_stage_<P>`
5. `DROP TABLE stellar.lec_stage_<P>`; record P done (resumable state file).
6. Disk-guard between partitions; run under `run-heavy-job.sh` so the data-pool
   watchdog can stop it if the pool runs low.

Idempotent: recomputing an already-correct partition yields identical ordinals, so
a re-run is safe.

## Then

D3 (`deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`, windowed, drop
MVs before RENAME) → D4 (`projector-replay`, INV-3 guarded) → cleanup (census
DELETE + tx_hash ZSTD) → Phase E prove.
