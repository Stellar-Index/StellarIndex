---
title: Runbook — account-movements-projection
last_verified: 2026-07-16
status: draft
severity: P3
---

# Runbook — materialize `proj_by_address` on `stellar.account_movements` (operator procedure)

## At a glance

| Field | Value |
| ----- | ----- |
| Trigger | BACKLOG #72: `GET /v1/accounts/{g}/movements` times out (> 20 s) for extreme-volume addresses. Operator-initiated, not alert-driven — there's no page for this; run it when a Phase-0-free window opens. |
| Tool | `clickhouse-client` DDL, wrapped in `/usr/local/sbin/run-heavy-job.sh` |
| Typical wall time | Unmeasured against r1's real 6.76B-row table — expect HOURS, not minutes (`ADD PROJECTION` is instant; `MATERIALIZE PROJECTION` rewrites every existing part). Budget a full maintenance window. |
| Impact | None to reads/writes while running (materialize is a background mutation, not a lock) — but it competes for merge/mutation pool + disk I/O with everything else on the box, hence the Phase-0-free-window + `run-heavy-job.sh` preconditions below. |

## Preconditions — do not skip these

1. **Phase-0-free window.** As of this writing, `stellar.account_movements`
   is still growing via the genesis-extension backfill (Phase 0 — see
   notes/ROADMAP.md §0). `MATERIALIZE PROJECTION` on a table under
   concurrent heavy write pressure both fights that backfill for the
   merge/mutation pool AND materializes into a moving target — the
   fragmented-parts state this fix targets is partly a *symptom* of that
   concurrent write pattern (see "Why this exists" below), so materializing
   mid-backfill buys less than materializing once Phase 0 has quiesced.
   Confirm Phase 0 is DONE (or at least not currently writing into
   `account_movements`) before starting.
2. **One heavy job at a time (CLAUDE.md).** Check nothing else is running
   under `run-heavy-job.sh` before starting — `ps aux | grep run-heavy-job`
   or check the systemd scope list (`systemctl list-units 'run-heavy-job-*'`).
3. **This is NOT retrofit automatically.** `EnsureAccountMovementsTable`'s
   `CREATE TABLE IF NOT EXISTS` (and re-running `tier1_schema.sql`) only
   adds `proj_by_address` on a table that doesn't exist yet — same caveat
   as the `idx_cb_balance_id` skip index (CHANGELOG, 2026-07-12). r1's
   table already exists, so this procedure's `ALTER TABLE ... ADD
   PROJECTION` + `MATERIALIZE PROJECTION` is the only path that gets it
   there.

## Why this exists

`GET /v1/accounts/{g}/movements` (`ExplorerReader.AccountMovements`,
`internal/storage/clickhouse/account_movements.go`) reads:

```sql
SELECT <cols> FROM stellar.account_movements
WHERE address = ?
  [AND movement_kind = ?] [AND direction = ?] [AND asset = ?]
  [AND (ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)]
ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC
LIMIT ?
```

The table's `ORDER BY (address, ledger, tx_hash, op_index, leg_index,
direction)` already leads with `address` — so an equality filter on
`address` prunes efficiently *within* any one part. The problem is
`PARTITION BY intDiv(ledger, 1000000)`: partitioning is by **ledger
range**, not by address, so a normal address's handful of movements
sits in a couple of partitions, but a pathological address active
across nearly the whole chain's life (BACKLOG #72's example: an
airdrop-sink address with 264M received payments) has rows in ~140 of
the table's ~473 partitions. `address = ?` cannot prune *across*
partitions — ClickHouse has to open each of those ~140 parts, seek to
the address's range via the primary index, and read at least the
tail granule of that range, and the ROADMAP's measured cost of that
fan-out is ~4M rows read to serve a `LIMIT 5`, well past the API's
timeout.

Two things compound this today, and both are called out explicitly
in notes/ROADMAP.md #72:

- **Part fragmentation from concurrent writes.** Phase 0's
  genesis-extension backfill writes into OLD (low-numbered) partitions
  at the same time live ingest writes into the newest one, so many of
  the ~473 partitions currently carry multiple small, not-yet-merged
  parts rather than one consolidated part each — more parts touched,
  more per-part minimum-read overhead paid. ROADMAP already notes this
  "partly self-heals as parts merge post-Phase-0."
- **Wide row payload.** Every touched part's minimum read unit (one
  granule) carries the full row width, including the `attributes` JSON
  blob and other columns this specific read doesn't need.

A ClickHouse **projection cannot change the table's partitioning** — a
projection is always co-partitioned with its parent table (no
independent `PARTITION BY`), so `proj_by_address` does not, by itself,
reduce the number of partitions a pathological address touches. What
`MATERIALIZE PROJECTION` buys is:

1. A **deterministic, one-time full compaction** of every existing
   part into `proj_by_address`'s own physical structure — instead of
   waiting on background merges that are (today) contending with
   Phase 0's concurrent writes and may not converge on any fixed
   schedule.
2. A **narrower column list** (`proj_by_address` selects the 13
   columns `AccountMovements()` actually reads/filters on — address,
   ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
   movement_kind, provenance, asset, counterparty, amount, attributes —
   and drops `ingested_at`, the ReplacingMergeTree version column the
   reader never selects), so each touched partition's minimum-granule
   read moves fewer bytes.

Both are real, but neither eliminates the ~140-partition fan-out
itself. **If this projection alone doesn't bring the pathological
case under the API's timeout, the next lever is partitioning — not
covered by this runbook** (a `PARTITION BY` change needs a full table
rebuild via `CREATE ... AS SELECT` into a new table, a materially
bigger and riskier operation than a projection; file a follow-up
BACKLOG item rather than improvising it live). The verification step
below is what actually tells you whether this was enough.

## One alternative considered — and why projection wins

A separate, address-sorted `account_movements_by_address` table (a
second copy, written by a second INSERT from `InsertAccountMovements`)
would give the same query shape without the partition-fanout
question. Rejected: it doubles storage AND doubles the write path (an
extra insert per `InsertAccountMovements` call, an extra table to keep
in sync in both DDL sites, an extra thing that can silently drift out
of sync under a partial-write failure). A projection is
auto-maintained by ClickHouse from the SAME inserts the base table
already receives — no application-code write-path change at all.

## Procedure

Run on r1, over SSH (`ssh root@136.243.90.96`). All DDL wrapped in
`run-heavy-job.sh` per CLAUDE.md's heavy-one-shot-job rule
(MemoryMax=20G, batch CPU/IO weight, one job at a time).

```sh
# 0. Confirm nothing else heavy is running, and confirm Phase 0 has
#    stopped writing into account_movements (check the
#    classic-movements-backfill cursor / systemd unit state first).

# 1. Add the projection definition (fast — metadata only, no data
#    movement yet).
/usr/local/sbin/run-heavy-job.sh account-movements-add-projection \
  clickhouse-client --port 9300 --query "
    ALTER TABLE stellar.account_movements
    ADD PROJECTION proj_by_address
    (
        SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
               movement_kind, provenance, asset, counterparty, amount, attributes
        ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)
    )"

# 2. Materialize it — THE heavy step. Rewrites every existing part in
#    the background as a mutation; does not block reads or writes.
/usr/local/sbin/run-heavy-job.sh account-movements-materialize-projection \
  clickhouse-client --port 9300 --query "
    ALTER TABLE stellar.account_movements
    MATERIALIZE PROJECTION proj_by_address"

# 3. Poll mutation progress (repeat until is_done=1 for both the ADD
#    and MATERIALIZE mutations).
clickhouse-client --port 9300 --query "
  SELECT mutation_id, command, parts_to_do, is_done, latest_fail_reason
  FROM system.mutations
  WHERE table = 'account_movements' AND database = 'stellar'
  ORDER BY create_time DESC LIMIT 5
  FORMAT PrettyCompact"
```

`ADD PROJECTION` alone is metadata-only and returns immediately — it's
`MATERIALIZE PROJECTION` that does the actual rewrite (an
`ALTER ... MATERIALIZE` mutation), which is why step 2 is the one that
needs the heavy-job wrapper and the Phase-0-free window. Do not skip
straight to step 2's query without step 1 — `MATERIALIZE PROJECTION`
on a projection that hasn't been `ADD`ed errors immediately (cheap
failure, but keep the order).

## Disk implications — read before starting

A materialized projection is a **second, independently-stored copy**
of the projected columns, sorted differently — it is not a free index.
Budget **roughly a doubling of the projected columns' footprint** on
top of the base table's own size for the duration of (and after)
materialization; `proj_by_address` is narrower than the full row
(drops `ingested_at`) so the actual delta is somewhat under 2x the
full table, but treat 2x as the planning number.

This lands on the **ZFS data pool, not root** — `account_movements`
is served by the same ClickHouse data directory as every other lake
table, which per ADR-0002/ADR-0016 lives on the pool, not the 49 GB
root partition that has its own history of filling
(`node-root-disk-filling-fast.md`). Still: confirm headroom on the
pool itself before starting (`zpool list`, `df -h` on the CH data
mount) — a multi-hundred-GB table growing toward ~2x mid-materialize
is exactly the kind of heavy job `run-heavy-job.sh`'s MemoryMax +
batch I/O weight exists to contain, but it does not create disk space
that isn't there. If the pool is tight, free space first (this is not
the runbook for that — see `node-root-disk-filling-fast.md` /
`db-disk-full.md` for pool-pressure procedures) rather than starting
materialization into a near-full pool.

## Verification

```sh
# 1. Confirm the projection's parts exist and are landing (grows over
#    the course of materialization; compare against system.parts for
#    the base table to gauge progress).
clickhouse-client --port 9300 --query "
  SELECT count() AS projection_parts, sum(rows) AS projection_rows,
         sum(bytes_on_disk) AS projection_bytes
  FROM system.projection_parts
  WHERE table = 'account_movements' AND database = 'stellar'
    AND name = 'proj_by_address' AND active
  FORMAT PrettyCompact"

# 2. Confirm the query planner picks the projection for the reader's
#    exact shape — look for "Projections: proj_by_address" (or
#    "Choosed projection" depending on CH version) in the plan.
clickhouse-client --port 9300 --query "
  EXPLAIN indexes = 1
  SELECT ledger, tx_hash, op_index, leg_index, direction, movement_kind,
         provenance, asset, counterparty, amount, attributes
  FROM stellar.account_movements
  WHERE address = 'GC6ZWKVY...'
  ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC
  LIMIT 5
  FORMAT PrettyCompact"

# 3. The number that actually matters: timed LIMIT-5 read on the SAME
#    pathological address BACKLOG #72 measured (~4M rows / >20s
#    before this fix). Compare read_rows before/after.
clickhouse-client --port 9300 --query "
  SELECT ledger, tx_hash, op_index, leg_index, direction, movement_kind,
         provenance, asset, counterparty, amount, attributes
  FROM stellar.account_movements
  WHERE address = 'GC6ZWKVY...'
  ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC
  LIMIT 5
  FORMAT PrettyCompact" --time
# or: SET send_logs_level='trace'; ... check the "read_rows" summary
# line, or pull it from system.query_log's read_rows/query_duration_ms
# for this query_id after the fact.
```

`read_rows` in the query summary (or `system.query_log`) dropping from
the ~4M-row baseline toward roughly `parts_touched * granule_size` is
the signal this worked. If `EXPLAIN` doesn't show the projection being
selected at all, check `optimize_use_projections` (default `1`/on —
confirm it hasn't been disabled in `configs/ansible/` or a session
override) before assuming the DDL is wrong.

**If read_rows drops but the query is still too slow for the API's
budget**, that confirms the "one alternative considered" section's
caveat: the partition fan-out itself, not per-partition cost, is the
remaining bottleneck, and a `PARTITION BY` change is the next
structural step — file it as a new BACKLOG item rather than
extending this one.

## Rollback

```sh
/usr/local/sbin/run-heavy-job.sh account-movements-drop-projection \
  clickhouse-client --port 9300 --query "
    ALTER TABLE stellar.account_movements DROP PROJECTION proj_by_address"
```

`DROP PROJECTION` is a metadata-only op (unlike `ADD` + `MATERIALIZE`,
it does not need `run-heavy-job.sh` for correctness, but wrap it
anyway for consistency/audit trail) — it removes the projection's
stored parts on the next merge cycle, no data loss to the base table.
Safe to run at any point, including mid-materialize, if the mutation
needs to be aborted (check `system.mutations` for a `KILL MUTATION`
if it's already running and DROP alone doesn't stop it fast enough).

## Related

- `notes/ROADMAP.md` #72 — the backlog item this runbook closes
  (gitignored, so not a clickable link here — same convention as
  other narrative docs that cite it, e.g. `docs/architecture/
  supply-pipeline.md`) — the source of the row/part counts cited
  above.
- `internal/storage/clickhouse/account_movements.go` —
  `accountMovementsDDL` (`EnsureAccountMovementsTable`) and the
  `AccountMovements()` reader this projection targets.
- `deploy/clickhouse/tier1_schema.sql` — the second DDL site; both
  must carry the same `PROJECTION proj_by_address` definition.
- [api-latency.md](api-latency.md) — the general API-slowness runbook;
  an extreme-address `/v1/accounts/{g}/movements` timeout is a
  narrower, address-specific instance of that symptom.
- [node-root-disk-filling-fast.md](node-root-disk-filling-fast.md) /
  [db-disk-full.md](db-disk-full.md) — pool/disk-pressure procedures,
  relevant if headroom is tight before materializing.
- CHANGELOG 2026-07-12 `idx_cb_balance_id` entry — the precedent for
  "DDL codified in both sites, retrofit needs a manual `ALTER` on an
  already-existing table."

## Changelog

- 2026-07-16 — initial draft (BACKLOG #72). DDL landed in both sites;
  this procedure has NOT yet been run against r1 — `MATERIALIZE
  PROJECTION` is deliberately deferred to a Phase-0-free window per
  CLAUDE.md's heavy-job discipline. Update `status:` to `ratified` and
  fill in real timing/row-count numbers once it has been executed and
  verified once.
