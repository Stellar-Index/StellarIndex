---
title: contract_events_daily MV redesign — uniqExact → uniqCombined(17)
last_verified: 2026-07-10
status: accepted
---

# contract_events_daily MV redesign

**Status: accepted; code-complete. r1 apply is the remaining operator
step (see § Procedure).**

## 1. Incident context

`stellar.contract_events_daily` (AggregatingMergeTree, one row per
`(contract_id, day, event_type, topic_0_sym, t1_xdr, t0_xdr)`, `events
AggregateFunction(uniqExact, UInt32, String, UInt32, UInt32)`) backs the
`/v1/protocols/{name}` fast path (`docs/architecture` BACKLOG #43):
without it, the daily-activity series and event-type breakdown fall
back to a ~15s raw scan of `stellar.contract_events`.

On 2026-07-09, a background merge of that table's `events` column
exceeded the kernel commit budget (`vm.overcommit_memory=2`): `uniqExact`
stores every distinct `(ledger_seq, tx_hash, op_index, event_index)`
tuple it has seen as a literal hash-set entry, so a hot
contract+day+event_type+topic group's state grows **unboundedly** —
tens of millions of events for a busy contract on a busy day serialize
to tens of megabytes, and merging N such states allocates all of them
at once. The merge failed with an allocator exception, the background
executor retried in a loop, and the retry storm starved the live sink +
queries for hours (see `notes/BACKLOG.md` / CHANGELOG `[Unreleased]`).

Two mitigations are live on r1 (`configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`,
commit `adeaef46`):

- `merges_mutations_memory_usage_soft_limit=10G` cluster-wide — an
  oversized merge now aborts cleanly (`MEMORY_LIMIT_EXCEEDED`) instead of
  exhausting the commit budget and retry-looping.
- `ALTER TABLE stellar.contract_events_daily MODIFY SETTING
  max_bytes_to_merge_at_max_space_in_pool = 1` — merges on this specific
  table are parked (any part above ~1 byte is ineligible), which stops
  the crash but also stops ALL compaction.

Consequence: parts accumulate at the live-insert rate (~180/day) with no
merges to consolidate them, converging on ClickHouse's
`TOO_MANY_PARTS` insert-throw ceiling (3,000 parts) — a roughly two-week
fuse from the incident date. This document + the artifacts it describes
close that fuse.

## 2. Where the pieces live

| Piece | Location |
|---|---|
| Canonical DDL (fresh-deployment bootstrap) | `deploy/clickhouse/tier1_schema.sql` — `contract_events_daily` table + `contract_events_daily_mv` |
| r1 rebuild artifact (existing-deployment migration) | `deploy/clickhouse/contract_events_daily_v2.sql` |
| Source table | `stellar.contract_events` (ReplacingMergeTree, same file) — the natural key is `(ledger_seq, tx_hash, op_index, event_index)` |
| Go readers | `internal/storage/clickhouse/protocol_reader.go`: `DailyActivityAvailable`, `ProtocolDailyActivityFast`, `ProtocolEventBreakdownFast` |
| Raw-scan fallback (used when the daily table is absent/unavailable) | same file: `ProtocolDailyActivity`, `ProtocolEventBreakdown` |
| API wiring | `internal/api/v1/protocols.go`: `fastActivity`, `protocolBreakdown`, `protocolSeries` |
| Integration test (applies `tier1_schema.sql` against a real ClickHouse via testcontainers-go) | `test/integration/clickhouse_harness_test.go`, `test/integration/clickhouse_storage_test.go::TestClickHouseProtocolBreakdownT0XDR` |

## 3. Reader survey — what precision do the callers actually need?

Every SQL site that touches the `events` aggregate state, and every
caller reachable from it:

| Reader | Feeds | Precision actually required |
|---|---|---|
| `ProtocolDailyActivityFast` (`protocol_reader.go`) | `GET /v1/protocols/{name}` daily-activity chart (`protocolSeries` in `protocols.go`) | Chart points. Explorer (`web/explorer/src/lib/format.ts::formatCompact`) renders `1.2M` / `845K` / `12.3B` — one decimal place at K/M/B scale. Exactness below that rounding threshold is invisible in the UI. |
| `ProtocolEventBreakdownFast` (`protocol_reader.go`) | `GET /v1/protocols/{name}` `event_breakdown[]` (`protocolBreakdown` in `protocols.go`) | Same compact-formatted display (`ProtocolView.tsx`'s `Kpi`/`Glance` components call the same `formatCompact`). |
| `DailyActivityAvailable` | Probes table existence only | N/A — no count involved. |
| `ProtocolEventBreakdown` / `ProtocolDailyActivity` (raw-scan **fallback**, same file) | Same two API fields, used when the daily table is absent/unbackfilled | These already run plain `count()` over `stellar.contract_events` **without `FINAL` and without any natural-key dedup** — i.e. the product's own accepted fallback path already tolerates possible overcounting from unmerged duplicate inserts. The daily table's `uniqExact` was strictly MORE precise than the path it's allowed to fall back to. |

Two additional facts settle the "does anything need this to be exact"
question:

- **`docs/operations/45b-verify-first-findings.md` §4** (an earlier
  verify-first audit of this exact table) explicitly rules out
  `contract_events_daily` as a completeness/reconcile input: *"A
  ClickHouse materialized view cannot reproduce decoder semantics, so
  `contract_events_daily` ... can seed candidate windows at best, never
  the reconcile oracle itself."* The ADR-0033 completeness verdict
  (`completeness_snapshots`) is computed by the real decoders against the
  lake, never by this table.
- **`/v1/protocols` list-level `events_24h`** (a different, adjacent
  field, easy to confuse with this table) is NOT sourced from
  `contract_events_daily` at all — it's `protocol_events_24h`, a
  separate worker-maintained Postgres rollup over the *served* tier
  (migration `0086_create_protocol_events_24h_rollup.up.sql`). This
  redesign does not touch that path.

**No reader anywhere in the call graph consumes this table's count as an
exact, customer-facing, or correctness-critical number.** Every consumer
is a rounded dashboard display, and the existing fallback path is
already less precise than `uniqExact` was.

## 4. Options compared

All three were built and measured against a real ClickHouse (24.8, the
version pinned in `test/integration/clickhouse_harness_test.go`), not
reasoned about in the abstract.

### (a) `uniqCombined(N)` in place of `uniqExact` — chosen

`uniqCombined` hashes the same natural-key tuple into a HyperLogLog-family
sketch: exact-small-set for low cardinality, degrading gracefully to a
bounded dense representation as cardinality grows. Because it still
hashes the *natural key*, duplicate/retried inserts of the same
`(ledger_seq, tx_hash, op_index, event_index)` still collapse to one —
it inherits the same duplicate-insert-safety `uniqExact` was chosen for,
just with a bounded state.

Measured (single-group state size, `AggregatingMergeTree` part,
uncompressed on-disk bytes — `system.parts.data_uncompressed_bytes`):

| Cardinality | `uniqExactState` | `uniqCombinedState(17)` | `uniqCombinedState(14)` |
|---:|---:|---:|---:|
| 500,000 uniques | 8,000,006 B (~7.6 MB) | 81,996 B (~80 KB) | 10,326 B (~10 KB) |
| 4,000,000 uniques | 72,000,013 B (~68.7 MB, +64,000,007 B over the 500K row) | 163,992 B (~160 KB total across 2 states — **identical 81,996 B per state**, confirming the bound) | 20,652 B total (**identical 10,326 B per state**) |

`uniqExact` grows linearly (~16 bytes/distinct value) without limit.
`uniqCombined` plateaus at a fixed ceiling regardless of cardinality —
this directly eliminates the failure mode (an unboundedly large single
state blowing the merge memory budget).

Measured accuracy (same states, `uniqCombinedMerge(17)`/`(14)` vs.
`uniqExactMerge` ground truth):

| Cardinality | `uniqCombined(17)` estimate | error | `uniqCombined(14)` estimate | error |
|---:|---:|---:|---:|---:|
| 500,000 | 500,691 | 0.138% | 501,565 | 0.313% |
| 4,000,000 | 4,016,122 | 0.403% | 3,991,651 | 0.209% |

Both precisions land inside the "~0.1-0.5%" estimate already carried in
`notes/ROADMAP.md`'s incident row. **Chose precision 17** (ClickHouse's
own default for the bare `uniqCombined(...)` form, the most
battle-tested parameterization) over 14: the state-size difference
(~80KB vs ~10KB max) is immaterial to solving the incident — both are
~100-1000x smaller than the megabyte-plus states that caused the
failure — so there's no reason to trade away 14's slightly better
worst-case accuracy margin for a memory savings that doesn't move the
needle.

Verified separately (see § Procedure): merging two `uniqCombinedState`s
built from an *overlapping* raw-row window (e.g. a retried/duplicated
backfill window) still returns the correct deduplicated count — the
merge is a set union (HyperLogLog register-wise max), not a sum, so it
does not double-count. This is the property that makes a windowed,
retry-safe backfill possible.

### (b) Drop the unique-count column; compute `uniqExact` at query time over raw `contract_events`

Rejected. This is exactly the ~15s-scan problem `contract_events_daily`
was built to eliminate (BACKLOG #43) — reintroducing it at read time
defeats the table's entire purpose. The daily table's whole reason for
existing is that `GET /v1/protocols/{name}` needs to stay fast; an
option that trades the merge-memory bug for the original latency bug
isn't a fix, it's a swap.

### (c) `countState()`/`countMerge()` — plain row counts, no dedup

Rejected. The existing DDL comment (predates this change) already
documents why a `SummingMergeTree`-style plain count was rejected the
first time the table was designed: `stellar.contract_events` is a
`ReplacingMergeTree`, and duplicate inserts from live-sink retries or
`ch-rebuild` re-derives are expected and land as literal duplicate rows
until a merge (or `FINAL`) collapses them. `countState()` sums raw rows
— it has no natural-key awareness — so it would silently overcount by
exactly the duplicate-insert amount, reintroducing the bug class the
table exists to avoid. `uniqCombined` was chosen over `countState`
specifically because it preserves natural-key dedup (proven above)
while still being bounded; `countState` is bounded but not
duplicate-safe. The "exactness isn't needed" finding in § 3 is about
whether the *count* needs to be exact, not about whether duplicate
*rows* are safe to double-count — those are different questions, and
`countState` only helps with the first while creating a real (if
usually small) correctness regression on the second.

## 5. Recommendation

**Option (a): `uniqCombined(17)` in place of `uniqExact`, same table
shape otherwise.** It is the only option that is simultaneously (i)
bounded-memory (fixes the incident), (ii) still duplicate-insert-safe
(preserves the property the original design was chosen for), and (iii)
does not reintroduce the raw-scan latency the table exists to avoid.
The accuracy cost (~0.1-0.5%) is strictly below the explorer's own
display rounding and is not consumed by anything correctness-critical
(§ 3).

## 6. What shipped in this change

- `deploy/clickhouse/tier1_schema.sql` — canonical `contract_events_daily`
  CREATE TABLE + MV updated to `uniqCombined(17)` (what every **fresh**
  deployment gets from day one; the integration test suite applies this
  file against a real ClickHouse container, so it also exercises the new
  shape end-to-end).
- `deploy/clickhouse/contract_events_daily_v2.sql` — the side-by-side
  rebuild artifact for r1's **existing** deployment (`v2`-suffixed table
  + MV so `IF NOT EXISTS` in `tier1_schema.sql` can't silently no-op
  against it).
- `internal/storage/clickhouse/protocol_reader.go` — `ProtocolDailyActivityFast`
  and `ProtocolEventBreakdownFast` now call `uniqCombinedMerge(17)(events)`
  instead of `uniqExactMerge(events)`; doc comments updated.
- This document.

No Go struct/interface changes: `uniqCombinedMerge` returns `UInt64`,
same as `uniqExactMerge`, so `ProtocolDailyPoint.Events` /
`ProtocolEventTypeCount.Count` (`uint64`) are unaffected, and the table
name stays `stellar.contract_events_daily` post-cutover — no reader call
sites, route names, or wire shapes change.

## 7. Procedure — r1 apply runbook

Every command below was executed against a real ClickHouse 24.8
container (the version this repo's integration suite pins) while
writing this doc, including the exact cutover DDL sequence and its
failure mode if done wrong (§ 7.4). This is not a theoretical plan.

Run every step under `/usr/local/sbin/run-heavy-job.sh <name> <cmd>`
per CLAUDE.md's heavy-job discipline — the backfill in particular reads
a partition-pruned slice of a multi-billion-row table.

### 7.1 Create v2 (starts live capture immediately)

```sh
clickhouse-client --queries-file deploy/clickhouse/contract_events_daily_v2.sql
```

This creates `stellar.contract_events_daily_v2` + `..._v2_mv` only (the
file's later sections are commented runbook text, not executable DDL).
From this moment, `contract_events_daily_v2` is receiving every NEW
`contract_events` insert live — the historical backfill below only needs
to cover history up to now.

**Capture the swap ledger** right after this step:

```sql
SELECT max(ledger_seq) AS swap_ledger FROM stellar.contract_events;
```

Record this value — it bounds the historical backfill and seeds the
gap-catchup in § 7.4.

### 7.2 Windowed historical backfill

Loop windows by `ledger_seq` (the source table is
`PARTITION BY intDiv(ledger_seq, 1000000)`, so 1M-ledger windows are
partition-aligned and prune cleanly). Re-running a window, or letting
windows overlap at the edges, is safe — verified: merging two
`uniqCombinedState`s built from overlapping raw rows does not
double-count (§ 4a). This makes the loop trivially resumable after an
interrupted `run-heavy-job` kill.

```sh
for start in $(seq 2 1000000 <swap_ledger>); do
  end=$((start + 1000000))
  /usr/local/sbin/run-heavy-job.sh contract-events-daily-v2-backfill \
    clickhouse-client --query "
      INSERT INTO stellar.contract_events_daily_v2
      SELECT toDate(close_time) AS day, contract_id, event_type,
             topic_0_sym, if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
             if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
             uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index)
      FROM stellar.contract_events
      WHERE ledger_seq >= ${start} AND ledger_seq < ${end}
      GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr"
done
```

### 7.3 Verify before cutover

```sql
SELECT v1.contract_id, v1.day, v1.c AS v1_exact, v2.c AS v2_approx,
       abs(v2.c - v1.c) / v1.c AS rel_err
FROM (SELECT contract_id, day, uniqExactMerge(events) AS c
      FROM stellar.contract_events_daily GROUP BY contract_id, day) v1
JOIN (SELECT contract_id, day, uniqCombinedMerge(17)(events) AS c
      FROM stellar.contract_events_daily_v2 GROUP BY contract_id, day) v2
  USING (contract_id, day)
ORDER BY v1_exact DESC LIMIT 20;
```

Expect `rel_err` well under 1% across the hottest groups (measured
0.1-0.5% in § 4a). Also confirm row-count parity of the (contract_id,
day) key set between v1 and v2 (v2 should be a superset — it has
everything v1 has plus anything landed after v1's last update, since
v1's merges have been parked and it may itself be slightly behind).

### 7.4 Cutover

**Do not** rename the table alone and expect the MV to follow — verified
failure mode: `RENAME TABLE` does not update a `MaterializedView`'s
stored target reference, so a live-sink insert after renaming only the
underlying table (leaving the old MV pointed at the old name) fails
outright:

```
DB::Exception: Target table 'stellar.contract_events_daily_v2' of view
'stellar.contract_events_daily_new_mv' doesn't exists.
```

The MV must be dropped and recreated under the final name, not renamed.
Run as one script (each statement is metadata-only DDL — the whole
sequence takes milliseconds, not the multi-second-plus a data-copying
operation would):

```sql
DROP VIEW stellar.contract_events_daily_v2_mv;
DROP VIEW stellar.contract_events_daily_mv;

RENAME TABLE
  stellar.contract_events_daily TO stellar.contract_events_daily_old,
  stellar.contract_events_daily_v2 TO stellar.contract_events_daily;

CREATE MATERIALIZED VIEW stellar.contract_events_daily_mv
TO stellar.contract_events_daily AS
SELECT
    toDate(close_time) AS day, contract_id, event_type, topic_0_sym,
    if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
    if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
    uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index) AS events
FROM stellar.contract_events
GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;
```

Between the two `DROP VIEW`s and the `CREATE MATERIALIZED VIEW`
succeeding, any `contract_events` row inserted isn't captured by either
MV. Close that gap deterministically (safe to run even if the gap was
actually zero-width — overlapping re-insertion doesn't double-count,
§ 4a):

```sql
INSERT INTO stellar.contract_events_daily
SELECT toDate(close_time) AS day, contract_id, event_type,
       topic_0_sym, if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
       if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
       uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index)
FROM stellar.contract_events
WHERE ledger_seq >= <swap_ledger>
GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;
```

Confirm the API's cached `DailyActivityAvailable` probe re-fires (it's
a `sync.Once` per process — restart `stellarindex-api`, or wait for its
next natural restart, to pick up the canonical table under its new
engine) and that `GET /v1/protocols/{name}` still returns
`event_breakdown` / the daily series.

### 7.5 Drop the old table, confirm merges resume

```sql
DROP TABLE stellar.contract_events_daily_old SYNC;
```

Confirm the new canonical table carries no leftover parked-merge
setting (a fresh `CREATE TABLE` never had one applied — verified: only
`index_granularity` shows in `SHOW CREATE TABLE` post-swap):

```sql
SHOW CREATE TABLE stellar.contract_events_daily;
```

Watch `system.parts` for the table over the next few merge cycles — part
count should trend down as ClickHouse's background scheduler picks up
now-eligible parts (previously blocked by
`max_bytes_to_merge_at_max_space_in_pool = 1`, which no longer applies —
that setting lived on the dropped `_old` table only).

### 7.6 Rollback

If step 7.3's parity check fails, or the cutover needs to be aborted
before § 7.4: nothing has touched the live `contract_events_daily`
table until the `DROP VIEW`/`RENAME` sequence runs. Up through § 7.3,
abort is simply `DROP TABLE stellar.contract_events_daily_v2_mv,
stellar.contract_events_daily_v2` — v1 was never touched and never
stopped serving. After § 7.4 has run but before § 7.5's `DROP TABLE`,
roll back by re-running the drop/rename/create sequence in reverse
(`contract_events_daily` → `_v2`, `_old` → canonical, recreate the
`uniqExact`-based MV) — the `_old` table is untouched data, so this is
lossless as long as it hasn't been dropped yet.

## 8. CI / lint coverage

There is no ClickHouse-specific DDL linter in `scripts/ci/` at the time
of writing (checked: no `sqlfluff`/`clickhouse-local`/dry-run step for
`deploy/clickhouse/*.sql` in `scripts/ci/` or `.github/workflows/`).
`tier1_schema.sql`'s only executable verification is the integration
suite applying it against a real ClickHouse container
(`test/integration/clickhouse_harness_test.go`) — which this change
exercises via `make test-integration` (build tag `integration`,
requires Docker). `contract_events_daily_v2.sql` is not applied by that
harness (it's an r1-only operator artifact, not part of the bootstrap
schema) and so has no automated syntax check; its DDL statements were
validated by hand against a live ClickHouse 24.8 container while
writing this document (§ 4, § 7).
