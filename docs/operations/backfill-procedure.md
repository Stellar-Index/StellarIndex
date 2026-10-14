---
title: Backfill procedure — replaying a historical ledger range
last_verified: 2026-10-05
status: operator runbook
---

# Backfill procedure

Runbook for `stellarindex-ops backfill` (implementation:
[`internal/ops/ingest/backfill.go`](../../internal/ops/ingest/backfill.go)).
It replays a bounded ledger range from the Galexie bucket through the live
dispatcher -> decoder -> sink path. Use for: a newly enabled source, a gap in
`trades`, a late-joining region, or a source whose `BackfillSafe` was flipped
in [`internal/sources/external/registry.go`](../../internal/sources/external/registry.go).

Projected sources (soroswap, phoenix, aquarius, ...) are refused here:
re-derive with `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>`.
Non-projected sources use `backfill` / `ch-rebuild`.

## Behaviour

- Writes one `trades` row per decoded event.
- Tracks its own cursor (`source="backfill"`, `sub_source="<from>-<to>:<sources>"`);
  the indexer's cursor is untouched. Exits at `-to`; never tails live.
- Refuses sources with `BackfillSafe=false`; Soroban sources need a per-WASM-hash audit ([`wasm-audits/README.md`](wasm-audits/README.md)) first. SDEX and off-chain are always safe.
- After each chunk's inserts it refreshes every CAGG built on them: the twelve
  `timescale.TradesCAGGs` over the chunk's trade timestamps and the seven
  `oracle_prices_*` rungs (`timescale.OracleCAGGs`) over its `oracle_updates`
  range; refresh policies only roll forward, so history is never materialised
  without this. Disable with `-refresh-caggs=false` only to debug a refresh failure.
  `prices_1m` is refreshed first. While `prices_1m`'s retention policy (migration
  0156) is armed the twap refresh is refused and the chunk fails: disarm it as the
  migration states, then `-resume`.
- Each refresh CALL runs under its own `statement_timeout` (5 min per hour of
  window, floor 10 min, ceiling 4 h). A wedged refresh fails its chunk; `-resume` re-walks it.

## Prerequisites

- [ ] Config validates: `stellarindex-ops -config /etc/stellarindex.toml dry-run`.
- [ ] Every source to replay is `BackfillSafe=true`.
- [ ] The Galexie bucket reaches the range. r1's `galexie-archive` is TRIMMED to a
  hot floor (ADR-0027; `stellarindex_archive_hot_floor`, r1 boundary 49,984,000):
  below it no objects exist. Check:
  `ssh r1 'grep ARCHIVE_HOT_FLOOR /etc/default/galexie-archive-fill'`.
  r2 reads `aws-public-blockchain` (any range); r3 pulls from Vultr Object Storage.
  Below the floor use a region that holds the range, or rehydrate first
  ([lcm-cache-tiering.md](lcm-cache-tiering.md)).
- [ ] Disk and DB IO headroom for the CAGG materialisation.
- [ ] Running beside live ingest is fine (primary-key dedupe); see
  [Running alongside live ingest](#running-alongside-live-ingest).

## Steps

### 1. Pick the range

```sh
psql stellarindex -c "SELECT min(ledger), max(ledger), count(*) FROM trades
  WHERE source = 'soroswap' AND ts BETWEEN '2026-04-15' AND '2026-04-20';"
```

`-from` / `-to` are inclusive ledger sequences; Galexie buckets are 64-ledger
granular, so the run aligns to `floor(from / 64) * 64`.

### 2. Dry-run

```sh
stellarindex-ops backfill -config /etc/stellarindex.toml -from 50000000 -to 50100000 -dry-run
```

Prints range, sources and bucket (`galexie-archive` below the live seam,
`galexie-live` above; override with `-bucket` if the range straddles).

### 3. Run, under the heavy-job wrapper on r1

```sh
/usr/local/sbin/run-heavy-job.sh backfill-50000000-50100000 \
  /usr/local/bin/stellarindex-ops backfill -write \
    -config /etc/stellarindex.toml -from 50000000 -to 50100000
```

The wrapper is mandatory for every ops one-shot on r1
([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot jobs). Use the
SAME job name every attempt; a held lock is refused with exit 75 (`fuser -v` on the
lock file names the holder). Append `2>&1 | tee <name>.log` to the inner command to
keep a log. Throughput ~50-150 ledgers/s per source; ~100k ledgers takes ~10-30 min.

### 4. Resume after a crash

Re-run step 3 with `-resume`. It reads the cursor and skips processed ledgers; the
cursor upserts every ~256 ledgers, and replays are idempotent. Never edit the cursor
table by hand.

### 5. Narrow the source set (optional)

Default is `cfg.Ingestion.EnabledSources`; override with `-source sdex,band`.

### 6. Verify

```sh
psql stellarindex -c "SELECT source, count(*) FROM trades
  WHERE ledger BETWEEN 50000000 AND 50100000 GROUP BY source ORDER BY 1;"
psql stellarindex -c "SELECT bucket, base_asset, quote_asset, vwap, trade_count FROM prices_1m
  WHERE bucket BETWEEN '2026-04-15' AND '2026-04-15 01:00' AND base_asset = 'native'
  ORDER BY bucket LIMIT 5;"
```

If CAGGs are empty, refresh by hand: all seven price views, `prices_1m` first, then
`twap_*` LAST (they build on `prices_1m`):

```sql
CALL refresh_continuous_aggregate('prices_1m',  '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_15m', '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1h',  '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_4h',  '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1d',  '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1w',  '2026-04-01'::timestamptz, '2026-04-22'::timestamptz);
CALL refresh_continuous_aggregate('prices_1mo', '2026-02-01'::timestamptz, '2026-05-01'::timestamptz);
CALL refresh_continuous_aggregate('twap_1h',    '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('twap_1d',    '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
```

Each window must span at least 2 buckets of its own grain or the call fails with
`SQLSTATE 22023: refresh window too small` (hence the wider last three; per-grain
minimums are the `MinWindow` values in `TradesCAGGs` / `OracleCAGGs`). Policies
cover only `prices_1m`'s 5-minute `start_offset` up to `prices_1mo`'s 3 months, so
older buckets are never materialised on their own.

## Failure modes

- **`backfill: source "<X>" is BackfillSafe=false; per-WASM-hash audit required
  before historical replay`**: audit per `wasm-audits/README.md`, flip
  `BackfillSafe: true` in `internal/sources/external/registry.go`, re-run.
- **Cursor collision**: the cursor key is `(from, to, sources)`; identical runs
  share progress, and any change of range or source set gives a fresh cursor.
- **`ledgerstream: 404 fetching FC<...>.xdr.zst`**: the bucket lacks the range.
  Check `ssh r1 "ls /var/lib/galexie/galexie-archive/<partition>/" | head`; if it
  is genuinely missing see [archival-node-bringup.md](archival-node-bringup.md)
  §"Disaster recovery".

## One-year retention catch-up (F-1265)

Target is historical retention >= 1 year. Run once before public flip and whenever
the data window shrinks below 1 year. ~6.3M ledgers; budget 6-12 h on one r1 at `-parallel 4`.

1. **Resolve the start ledger**: first ledger of the earliest in-scope archive
   partition, rounded DOWN to a multiple of 64:
   `ssh r1 'ls -1 /var/lib/galexie/galexie-archive/2025-05-* | head -1'`.
2. **Check the archive has no gaps** (`-from` / `-to` are ledgers, not dates):
   ```sh
   stellarindex-ops verify-archive -config /etc/stellarindex.toml \
     -from <year-ago-ledger> -to <tip-ledger> -bucket galexie-archive
   ```
3. **Size it**: ~50-200 GB of trade rows plus ~10-20 GB of CAGG data.
4. **Run in 1-week chunks** (~120k ledgers), so a crash is not a 12 h resume. Stop if a chunk fails.
   ```sh
   for week_from in $(seq -w 50000000 120000 56000000); do
     week_to=$((week_from + 120000))
     stellarindex-ops backfill -write -config /etc/stellarindex.toml \
       -from "$week_from" -to "$week_to" -resume -parallel 4 2>&1 | tee "backfill-${week_from}.log"
   done
   ```
5. **CAGGs refresh automatically** per chunk. Only on a binary lacking
   `-refresh-caggs` run the nine calls from step 6 after the chunk, with
   `<T_LO>` / `<T_HI>` = THIS chunk's timestamps, `prices_1m` first, windows
   widened for coarse rungs (`prices_1mo` needs two calendar months). A binary
   that refreshed only `prices_1h` and coarser needs the repair section below.

   > **`NULL, NULL` is the WHOLE view**: order-of-10M `prices_1m` rows per 30 days
   > in one uninterruptible `CALL`, nine times. Only for a small or freshly-seeded
   > deployment. Otherwise walk `[T_LO, T_HI]` in weekly or monthly slices under a
   > heavy-job scope, off the 14:00-22:00 UTC peak ([cagg-broad-recompute.md](cagg-broad-recompute.md)).

6. **Verify at 1m grain, not only the default**:
   ```sh
   # (a) coarse coverage
   curl -s '<host>/v1/chart?asset=native&quote=fiat:USD&timeframe=1y' \
     | jq '{n: (.data.points|length), truncated: .data.truncated, first: .data.points[0].t}'
   # (b) the grain this procedure materialises; limit defaults to 100 and takes the
   # EARLIEST buckets, so pass limit=1000 and keep the window under ~1000 minutes
   curl -s '<host>/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from=<T_LO>&to=<T_HI>&limit=1000' \
     | jq '.data.intervals | length'
   ```
   Bars are `.data.intervals`, chart points `.data.points` (`.data | length` on
   `/v1/ohlc` prints `6` regardless). (b) is mandatory: `timeframe=1y` defaults to
   `granularity=1d` (ADR-0020), reads `prices_1d`, and passes through a 1m/15m hole.

### Repairing a range whose CAGGs were left stale

A binary older than the per-chunk refresh left `twap_1h`, `twap_1d`, `dex_volume_by_pair_1d`, `source_volume_1h`, `pools_per_source_1h` and every `oracle_prices_*` rung stale. For the `trades` rollups: `stellarindex-ops trades-cagg-refresh -config PATH -from <ledger> -to <ledger> -write` refreshes all twelve in order; for the oracle rungs run migration 0040's `refresh_continuous_aggregate` calls over the range.

An older range may also lack `prices_1m` / `prices_15m` buckets and the `twap` bars over them (`trades` rows are intact, so no archive read). Migration 0147 already re-materialised both grains over older history: run `SELECT min(bucket) FROM prices_1m;` first and skip what it covered.

```sql
-- [T_LO, T_HI] = the backfilled range's timestamps. prices_1m FIRST.
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_15m', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>', '<T_HI>');
```

Walk it in weekly or monthly slices under a heavy-job scope, off the 14:00-22:00 UTC
peak; abort/monitor steps are in [cagg-broad-recompute.md](cagg-broad-recompute.md).
`55P03` just means the policy job is running: retry.

> **Don't run slices while a backfill is running.** Timescale rejects the losing
> concurrent refresh with `55P03`, and the backfill's retry budget is five attempts
> over ~3.0 s, so its chunk fails without checkpointing and must be re-walked under
> `-resume`. Finish or stop the backfill first, or avoid its ranges.

Confirm with the `/v1/ohlc` call from step 6(b).

### Retiring a shard's cursor row

Finished one-shot shards leave permanent `ingestion_cursors` rows that bury `list-cursors` and `/v1/diagnostics/cursors`.

1. **Confirm the work is over.** One-shot rows older than 7 days publish as `state: abandoned`:
   ```sh
   curl -s 'https://api.stellarindex.io/v1/diagnostics/cursors?status=abandoned' | jq '.data | length'
   stellarindex-ops resume-stalled -config /etc/stellarindex.toml -dry-run
   ```
   A shard with real remaining work is resumed, not reaped.
2. **Reap** (previews by default):
   ```sh
   stellarindex-ops reap-cursors -config /etc/stellarindex.toml
   stellarindex-ops reap-cursors -config /etc/stellarindex.toml -write
   ```
   `-older-than` (default `168h`, floor `24h`), `-source` narrows. `ledgerstream` and `projector` are never reaped: an old row there is [cursor-stuck](runbooks/ledger-ingest.md#stellarindex_ingestion_cursor_stuck). Reaping deletes the resume record, never data.

## When NOT to use this

Live tail (the indexer's job); re-deriving the prices CAGGs from existing trades (call `refresh_continuous_aggregate(...)` directly); a `BackfillSafe=false` source (audit first).

## Running alongside live ingest

A 12-way parallel `soroban-events` fill walk plus a 12-chunk verify-archive bootstrap exhausted Postgres connections and froze live trade ingest for 7 h: back-pressure blocks the fill producer, and a live indexer sharing that Postgres blocks too. The live indexer must have priority.

### Posture

The live indexer has Postgres to itself; write-heavy walks run in maintenance
windows where the live cursor may lag.

- **Live indexer** always running at full priority; its cursor advances every ledger.
- **Fill walks (`stellarindex-ops backfill`)**: `-parallel 4` or lower while live
  ingest writes. `-parallel 12` only when live ingest is paused or on a fresh box.
- **verify-archive**: `verify-archive-tier-a.timer` fires daily at 03:23 UTC under
  `run-heavy-job.sh`. An ad-hoc run should `systemctl stop stellarindex-indexer`
  first OR use `-workers 4` or lower.
- **Galexie + ledgerstream-fill** read MinIO, not Postgres; they do not contribute.

### Alerts that page on recurrence

| Signal | Alert | Severity |
| --- | --- | --- |
| Live cursor stalls | `stellarindex_ingestion_source_insert_stale` | ticket |
| Live indexer keeps inserting duplicates only | `stellarindex_ingestion_duplicate_flood` | ticket |
| Aggregator output stops | `stellarindex_aggregator_silent` | **page** |
| Per-asset staleness > 120 s | `stellarindex_api_price_stale` | ticket |
| Postgres connections > 80 % of `max_connections` | `stellarindex_timescale_connections_saturated` | ticket |

`page` → Discord #stellarindex-pages, `ticket` → #stellarindex-alerts; rules in
`configs/prometheus/rules.r1/`. Duplicate-flood runbook:
[ingestion-duplicate-flood](runbooks/ingestion-sink.md#stellarindex_ingestion_duplicate_flood).
If any fires while a fill walk or verify-archive runs, assume back-pressure and
stop the heavy walker first.

### Operator commands

#### Stop a running fill walk

```sh
# On r1. The fill is a manual invocation (`stellarindex-ops backfill -source
# soroban-events`, normally under run-heavy-job.sh), NOT a systemd unit.
ps -eo pid,args | grep '[s]tellarindex-ops backfill'
# Graceful: drains in-flight rows then exits. Kill by EXPLICIT PID; a
# `pkill -f 'backfill'` self-matches your own ssh shell.
kill -INT <pid>
```

SIGINT lets the in-flight batch finish, preserving cursor coherence. SIGKILL
loses that batch's rows; use it only if the process has not exited within 60 s.

#### Stop a running verify-archive

```sh
systemctl stop verify-archive-tier-a.service
```

The timer stays armed; the next scheduled fire still happens.

#### Resume the live indexer after a freeze

The symptom is "cursor not advancing", not "process not running":

```sh
systemctl status stellarindex-indexer
curl -sS http://localhost:3000/v1/diagnostics/cursors \
  | jq '.data[] | select(.source=="ledgerstream") | .lag_seconds'   # under 30 s in steady state
```

If lag stays high after the heavy walker is stopped:

```sh
systemctl restart stellarindex-indexer
journalctl -u stellarindex-indexer -f
```

### Unshipped hardening

The cheapest next step is reserving N Postgres connections for the live indexer via per-binary pool sizes; per-sink prioritisation in AsyncSink and a fill-walk replica are costlier.
