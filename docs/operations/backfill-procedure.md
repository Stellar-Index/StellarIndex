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
- Refuses sources with `BackfillSafe=false`. Soroban sources need a per-WASM-hash
  audit ([`wasm-audits/README.md`](wasm-audits/README.md)) before the flag flips;
  SDEX and off-chain are unconditionally safe.
- After each chunk's inserts it refreshes every CAGG built on them: the twelve
  `timescale.TradesCAGGs` (`prices_1m` ... `prices_1mo`, `twap_1h`, `twap_1d`,
  `dex_volume_by_pair_1d`, `source_volume_1h`, `pools_per_source_1h`) over the
  chunk's trade timestamps, and the seven `oracle_prices_*` rungs
  (`timescale.OracleCAGGs`) over its `oracle_updates` range. The refresh policies
  only roll forward, so a historical range is never materialised without this.
  Disable with `-refresh-caggs=false` only to debug a refresh failure.
  `prices_1m` is refreshed first and forced over every window the twaps read.
  While `prices_1m`'s retention policy (migration 0156) is armed the twap refresh
  is refused and the chunk fails: disarm it as the migration states, then `-resume`.
- Each refresh CALL runs under its own `statement_timeout`: 5 min per hour of
  window, floor 10 min, ceiling 4 h. A wedged refresh fails its chunk (view,
  window and bound logged); `-resume` re-walks it.

## Prerequisites

- [ ] Config validates: `stellarindex-ops -config /etc/stellarindex.toml dry-run`.
- [ ] Every source to replay is `BackfillSafe=true`.
- [ ] The Galexie bucket reaches the range. r1's `galexie-archive` is TRIMMED to a
  hot floor (ADR-0027; `stellarindex_archive_hot_floor`, role default 2 = none;
  r1 boundary 49,984,000, `inventory/r1.example.yml`): below it no objects exist.
  Check: `ssh r1 'grep ARCHIVE_HOT_FLOOR /etc/default/galexie-archive-fill'`.
  r2 reads `aws-public-blockchain` (any range); r3 pulls from Vultr Object Storage.
  Below the floor use a region that holds the range, or rehydrate first
  ([lcm-cache-tiering.md](lcm-cache-tiering.md)).
- [ ] Disk and DB IO headroom for the CAGG materialisation.
- [ ] Running beside live ingest is fine (shared hypertable, primary-key dedupe);
  expect a brief CPU spike.

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

Prints range, sources and bucket. The bucket is `galexie-archive` below the live
seam and `galexie-live` above; override with `-bucket` if the range straddles.

### 3. Run, under the heavy-job wrapper on r1

```sh
/usr/local/sbin/run-heavy-job.sh backfill-50000000-50100000 \
  /usr/local/bin/stellarindex-ops backfill -write \
    -config /etc/stellarindex.toml -from 50000000 -to 50100000
```

The wrapper is mandatory for every ops one-shot on r1
([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot jobs): systemd
scope, `MemoryMax=20G`, `MemorySwapMax=0`, batch CPU/IO weights, per-job singleton
lock, disk watchdog, ClickHouse `ops_batch` identity. Use the SAME job name every
attempt; a run that finds the lock held is refused with exit 75 (`fuser -v` on
the lock file names the holder). Append `2>&1 | tee backfill-50000000-50100000.log`
to the inner command to keep a log.

Throughput: ~50-150 ledgers/s per source (Galexie fetch + decode); ~100k ledgers
takes ~10-30 min.

### 4. Resume after a crash

Re-run step 3 with `-resume`. It reads the cursor and skips processed ledgers; the
cursor upserts every ~256 ledgers, and replays are idempotent (primary-key dedupe).
Never edit the cursor table by hand.

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
`SQLSTATE 22023: refresh window too small` (hence the wider last three).
`PadRefreshWindow` (`internal/storage/timescale/diagnostics.go`) applies the same
padding per chunk; per-grain minimums are the `MinWindow` values in `TradesCAGGs`
and `OracleCAGGs`. The policies cover only `prices_1m`'s 5-minute `start_offset` up
to `prices_1mo`'s 3 months (migration 0002); older buckets are never materialised
on their own.

## Failure modes

- **`backfill: source "<X>" is BackfillSafe=false; per-WASM-hash audit required
  before historical replay`**: audit per `wasm-audits/README.md`, flip
  `BackfillSafe: true` in `internal/sources/external/registry.go`, re-run.
- **Cursor collision**: the cursor key is `(from, to, sources)`; identical runs
  share progress, and any change of range (even one ledger) or source set gives a
  fresh cursor.
- **`ledgerstream: 404 fetching FC<...>.xdr.zst`**: the bucket lacks the range.
  Check `ssh r1 "ls /var/lib/galexie/galexie-archive/<partition>/" | head`; if it
  is genuinely missing see [archival-node-bringup.md](archival-node-bringup.md)
  §"Disaster recovery".

## One-year retention catch-up (F-1265)

Target is historical retention >= 1 year (ideally from inception). Run once before
public flip and whenever the data window shrinks below 1 year (e.g. restore from a
recent snapshot). ~6.3M ledgers at 5 s/ledger; budget 6-12 h wall-clock on one r1
at `-parallel 4`.

1. **Resolve the start ledger**: first ledger of the earliest in-scope archive
   partition, rounded DOWN to a multiple of 64:
   `ssh r1 'ls -1 /var/lib/galexie/galexie-archive/2025-05-* | head -1'`.
2. **Check the archive has no gaps** (catch-up reads only the immutable archive
   bucket; `-from` / `-to` are ledgers, not dates):
   ```sh
   stellarindex-ops verify-archive -config /etc/stellarindex.toml \
     -from <year-ago-ledger> -to <tip-ledger> -bucket galexie-archive
   ```
3. **Size it**: ~50-200 GB of trade rows plus ~10-20 GB of CAGG data (compressed
   ~5x) across the audited Soroban set (each DEX source ~50-500 trades/day).
4. **Run in 1-week chunks** (~120k ledgers); a whole-year run makes a crash a
   12 h resume and holds the cursor row throughout. Stop if a chunk fails.
   ```sh
   for week_from in $(seq -w 50000000 120000 56000000); do
     week_to=$((week_from + 120000))
     stellarindex-ops backfill -write -config /etc/stellarindex.toml \
       -from "$week_from" -to "$week_to" -resume -parallel 4 2>&1 | tee "backfill-${week_from}.log"
   done
   ```
5. **CAGGs refresh automatically** per chunk (all seven price grains). Only on a
   binary lacking `-refresh-caggs` run the nine calls from step 6 above after the
   chunk, with `<T_LO>` / `<T_HI>` = THIS chunk's timestamps, `prices_1m` first,
   windows widened for coarse rungs (`prices_1mo` needs two calendar months).
   A binary from 2026-05-13 to 2026-09-04 refreshed only `prices_1h` and coarser:
   see the repair section.

   > **`NULL, NULL` is the WHOLE view**, back to 2015: order-of-10M `prices_1m`
   > rows per 30 days of history in one uninterruptible `CALL`, nine times.
   > Acceptable only on a small or freshly-seeded deployment. Otherwise walk
   > `[T_LO, T_HI]` in weekly or monthly slices under a heavy-job scope, off the
   > 14:00-22:00 UTC peak ([cagg-broad-recompute.md](cagg-broad-recompute.md)).

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
   Every 2xx is an `Envelope` (`{data, as_of, flags, ...}`): bars are
   `.data.intervals` and chart points `.data.points`. `.data | length` on
   `/v1/ohlc` counts the six `OHLCSeriesResponse` keys and prints `6` regardless.
   (b) is mandatory: `timeframe=1y` defaults to `granularity=1d` (ADR-0020), reads
   `prices_1d`, and passed through the whole 1m/15m hole.

### Repairing a range backfilled after 2026-08-22

A binary older than the per-chunk `TradesCAGGs` / `OracleCAGGs` refresh left
`twap_1h`, `twap_1d`, `dex_volume_by_pair_1d`, `source_volume_1h`,
`pools_per_source_1h` and every `oracle_prices_*` rung stale. For the `trades`
rollups: `stellarindex-ops trades-cagg-refresh -config PATH -from <ledger> -to <ledger>`
refreshes all twelve in the safe order. For the oracle rungs run migration 0040's
`refresh_continuous_aggregate` calls over the range.

A range backfilled before 2026-09-04 also lacks `prices_1m` / `prices_15m` buckets
and the `twap_1h` / `twap_1d` bars over them. `trades` rows are intact (migration
0031 removed their retention), so this is a pure re-derive with no archive read.
Migration 0147 already re-materialised both minute grains over all history before
2026-08-22: run `SELECT min(bucket) FROM prices_1m;` first, and skip a range 0147
already swept.

```sql
-- [T_LO, T_HI] = the backfilled range's timestamps. prices_1m FIRST.
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_15m', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>', '<T_HI>');
```

Walk it in weekly or monthly slices (r1 accrues ~390k `prices_1m` rows/day, so a
30-day slice is order-of-10M buckets), under a heavy-job scope, off the
14:00-22:00 UTC peak; abort/monitor steps are in
[cagg-broad-recompute.md](cagg-broad-recompute.md). `55P03` just means the policy
job is running: retry.

> **Don't run slices while a backfill is running.** Timescale rejects the losing
> concurrent refresh with `55P03`, and the backfill's retry budget is five attempts
> over ~3.0 s (`RefreshContinuousAggregate`). A manual month-long `prices_1m` slice
> outlives it, so the backfill chunk fails without checkpointing and must be
> re-walked under `-resume`. Finish or stop the backfill first, or avoid its ranges.

Confirm from the served side:

```sh
curl -s '<host>/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from=<T_LO>&to=<T_HI>&limit=1000' | jq '.data.intervals | length'
```

### Retiring a shard's cursor row

`ingestion_cursors` keeps one permanent row per `(source, sub_source)`, so
finished or abandoned one-shot shards pile up (4,703 of 4,815 rows were idle over a
week in September 2026) and dominate `list-cursors`, `/diagnostics` and
`/v1/diagnostics/cursors`.

1. **Confirm the work is over.** One-shot rows older than 7 days publish as
   `state: abandoned`:
   ```sh
   curl -s 'https://api.stellarindex.io/v1/diagnostics/cursors?status=abandoned' | jq '.data | length'
   stellarindex-ops resume-stalled -config /etc/stellarindex.toml -dry-run
   ```
   `resume-stalled` prints each cursor's remaining range and skips ranges sibling
   coverage closed. A shard with real remaining work is resumed, not reaped.
2. **Reap.** Previews by default:
   ```sh
   stellarindex-ops reap-cursors -config /etc/stellarindex.toml
   stellarindex-ops reap-cursors -config /etc/stellarindex.toml -write
   ```
   `-older-than` (default `168h`, floor `24h`) sets the cutoff; `-source` narrows
   to one job. `ledgerstream` and `projector` are never reaped: an old row there
   is [cursor-stuck](runbooks/cursor-stuck.md). Reaping deletes the resume record,
   never data.

## When NOT to use this

- Live tail: the indexer's job.
- Re-deriving the prices CAGGs from existing trades: call
  `refresh_continuous_aggregate(...)` directly; backfill re-decodes XDR and is far
  heavier.
- A source with `BackfillSafe=false`: audit first (Soroban contracts upgrade in
  place; AGENTS.md).

## Cross-references

- [`internal/ops/ingest/reap_cursors.go`](../../internal/ops/ingest/reap_cursors.go): `reap-cursors`.
- [`migrations/0002_create_price_aggregates.up.sql`](../../migrations/0002_create_price_aggregates.up.sql): CAGG definitions.
