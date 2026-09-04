---
title: Backfill procedure — replaying a historical ledger range
last_verified: 2026-09-04
status: operator runbook
---

# Backfill procedure

Operator runbook for `stellarindex-ops backfill`. Use when:

- A new source is enabled; need to populate historical trades.
- A gap was discovered in the trades hypertable.
- A region is brought up later than its peers and needs to catch up.
- A source's WASM audit was completed retroactively (`BackfillSafe=true`
  was flipped in `internal/sources/external/registry.go`); historical
  rows can now be ingested.

The CLI lives at `cmd/stellarindex-ops/backfill.go`. It replays a
bounded ledger range through the same dispatcher → decoder →
sink path the live indexer uses, so output matches what the
indexer would have produced.

## What it does (and doesn't)

**Does:**
- Replays the requested range against the configured (or
  flag-overridden) source set.
- Writes one trade row per decoded event into the trades
  hypertable.
- **Force-refreshes all seven price CAGGs (`prices_1m` /
  `prices_15m` / `prices_1h` / `prices_4h` / `prices_1d` /
  `prices_1w` / `prices_1mo`) over each chunk's timestamp range as
  soon as the chunk's trade-insert loop completes** — this is
  mandatory for historical inserts, see "Why" below. Disable with
  `-refresh-caggs=false` only when debugging a specific CAGG-refresh
  failure. The order is fixed, with `prices_1m` first, because
  `prices_1m` is the one view another aggregate is defined over — see
  the `twap_1h` / `twap_1d` entry under **Doesn't** for what that does
  and does not get you.
- Maintains its own cursor row (`source="backfill"`) so a crash
  doesn't pollute the indexer's resume position.

### Why auto-refresh matters (the May 2026 SDEX incident)

The first SDEX historical backfill (~80M trades, ledgers
6,307,178 → 50,457,423) ran May 6-11 2026 and **completed every
cursor range** — yet a week later the trades hypertable
`MIN(ledger)` for `sdex` was 61,191,617. Every backfilled trade
was lost.

Root cause: the 90-day retention policy on raw `trades`
(migration 0001) runs daily and drops chunks whose `range_end >
90d ago`. Historical inserts carry `ts` values from 2017-2024 —
those chunks are *immediately* eligible for drop. The CAGG policy
refresher runs every 30 min but only rolls forward; it doesn't
auto-backfill historical buckets when old trades are inserted. So:

  T0   : backfill inserts ~80M trades with ts = 2017-2024
  T+24h: daily retention drops every backfilled chunk
  T+30m: CAGG refresher's next tick finds no data to roll up
  ...    : 80M trades and ~5d of wall-clock work, gone

The fix landed 2026-05-13 (`feat(ops): auto-refresh CAGGs in
backfill`): the backfill tool now calls
`refresh_continuous_aggregate` over each chunk's actual ts range
immediately after the insert loop, **before** the next retention
cycle. Aggregates persist.

> **Update (migration 0031, 2026-05-14):** the 90-day retention
> policy on raw `trades` described above was REMOVED. Raw trades are
> now retained indefinitely, so the aging-out failure mode no longer
> occurs. The CAGG auto-refresh remains correct and is kept (it still
> ensures historical buckets materialize promptly), but the
> "trades age out 90 days later" outcome no longer happens.

> **Update (2026-09-04):** the refresh set now covers **all seven**
> price CAGGs. The same migration 0031 also removed the 30-day
> retention on `prices_1m` and `prices_15m`, but the refresh set kept
> skipping those two for another four months — first on the retired
> retention, then on a cost argument that assumed nothing read the
> minute grains over a historical window. Three surfaces do:
> `/v1/ohlc?interval=1m|5m|15m|30m` (the 5m and 30m bars re-bucket
> `prices_1m`), `/v1/chart?granularity=1m|15m` at every timeframe
> including the unbounded `all`, and `/v1/history/since-inception` at
> both grains — that last one takes `granularity` verbatim, applies no
> time bound at all and returns oldest bucket first, so a hole in
> historical materialisation is the FIRST thing it serves. (Plain
> `/v1/history` is a different handler and is not affected: it reads
> raw trades, no CAGG.)
> A range backfilled before this has **no bars at 1m or 15m** unless a
> later full re-materialisation covered it — the trades are in the
> hypertable, but nothing materialised those buckets and the policies
> only roll forward. Migration 0147 (2026-08-22) recreated all seven
> price views `WITH NO DATA` and its operator block re-materialised
> `prices_1m` and `prices_15m` over all history, so in practice only
> ranges backfilled AFTER that date are exposed. Run
> `SELECT min(bucket) FROM prices_1m;` before scheduling the repair —
> an hours-long heavy-job slice is not worth running on a range that
> 0147 already swept. See "Repairing a range backfilled after
> 2026-08-22" below.

**Doesn't:**
- Tail live ledgers — exits at `-to`.
- **Refresh `twap_1h` or `twap_1d`.** Those two are hierarchical over
  `prices_1m` and are outside the tool's allow-list, so a range whose
  `prices_1m` buckets the tool just materialised still has **no TWAP
  bars** over that span until an operator refreshes them by hand.
  Refreshing `prices_1m` first only means that hand step reads current
  input; it does not perform it. See "Repairing a range backfilled
  after 2026-08-22" for the calls.
- Pollute the indexer's `ingestion_cursors` cursor.
- Run unaudited Soroban sources. Each on-chain Soroban decoder
  is gated by `BackfillSafe` in
  `internal/sources/external/registry.go`; the backfill CLI
  refuses to run an unsafe source against a historical range.

## Prerequisites

- [ ] **Operator config validates.**
      `stellarindex-ops -config /etc/stellarindex.toml dry-run`
      (or whatever your config-validate path is).
- [ ] **All sources you'll replay have `BackfillSafe=true`** in
      `internal/sources/external/registry.go`. Soroban sources
      need a per-WASM-hash audit (`docs/operations/wasm-audits/`
      directory) before this flag flips. SDEX + off-chain are
      `BackfillSafe=true` unconditionally.
- [ ] **Galexie archive bucket reaches the requested range.**
      r1's `galexie-archive` mirror is TRIMMED to a hot floor
      (ADR-0027; `stellarindex_archive_hot_floor` in the region's
      inventory — 49,984,000 on r1 today), so it does **not** reach
      ledger 2 and a request below the floor finds no objects. Check
      the floor before choosing `-from`:
      `ssh r1 'grep ARCHIVE_HOT_FLOOR /etc/default/galexie-archive-fill'`.
      r2 reads via `aws-public-blockchain` so any range is reachable;
      r3 pulls from Vultr Object Storage. Below the floor, source from
      the region that still holds the range — or rehydrate it first
      (see [lcm-cache-tiering.md](lcm-cache-tiering.md)).
- [ ] **Disk + DB headroom.** A ~24h backfill produces tens of
      thousands of trade rows for popular pairs; budget IO for
      the CAGG materialisation that follows insert.
- [ ] **Coordinate with the live indexer.** A backfill running
      simultaneously with live ingest is fine (they share the
      same trades hypertable + dedupe by primary key) but you
      will see a brief CPU spike. The indexer continues
      tip-tail uninterrupted.

## Step-by-step

### 1. Pick the ledger range

```sh
# Find the gap. Easiest: query the trades hypertable for
# distinct ledgers in the range you suspect missing.
psql stellarindex -c "
  SELECT min(ledger), max(ledger), count(*)
  FROM trades
  WHERE source = 'soroswap'
    AND ts BETWEEN '2026-04-15' AND '2026-04-20';
"

# Cross-reference with what was on-chain — Galexie bucket
# typically has every ledger in the range:
ssh r1 "ls /var/lib/galexie/galexie-archive/ | head -3"
```

Decide `-from` (inclusive) and `-to` (inclusive) ledger
sequences. Galexie buckets at 64-ledger granularity, so the
backfill aligns to `floor(from / 64) * 64` internally.

### 2. Dry-run first

```sh
stellarindex-ops backfill \
  -config /etc/stellarindex.toml \
  -from 50000000 \
  -to   50100000 \
  -dry-run
```

Expected output:

```
backfill dry-run:
  range:   [50000000, 50100000] (100001 ledgers)
  sources: [soroswap aquarius phoenix sdex binance]
  bucket:  galexie-archive
```

The bucket is `galexie-archive` (historical) when the range is
below the live seam; `galexie-live` when it's above. The CLI
picks automatically — you can override with `-bucket` if the
range straddles.

### 3. Run — under the heavy-job wrapper on r1

```sh
/usr/local/sbin/run-heavy-job.sh backfill-50000000-50100000 \
  /usr/local/bin/stellarindex-ops backfill \
    -config /etc/stellarindex.toml \
    -from 50000000 \
    -to   50100000
```

> ⚠️ **The wrapper is mandatory for every ops one-shot on r1**
> ([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot
> jobs). It puts the job in a systemd scope with `MemoryMax=20G`,
> `MemorySwapMax=0`, batch-class CPU/IO weights, a per-job singleton
> lock and the disk watchdog, and imports the low-priority ClickHouse
> `ops_batch` identity. A raw run has none of that: on 2026-07-05 an
> unwindowed re-derive ballooned, swapped galexie's captive core into
> an `invalid local state` wedge and froze the lake for 11 hours. Use
> a UNIQUE job name per attempt — a name whose lock is still held is
> **skipped**, silently and with exit 0.

Stream the output to a log so a stuck run is diagnosable:

```sh
/usr/local/sbin/run-heavy-job.sh backfill-50000000-50100000 \
  /usr/local/bin/stellarindex-ops backfill ... 2>&1 \
  | tee backfill-50000000-50100000.log
```

Throughput in steady state: ~50-150 ledgers/second per source,
limited by Galexie XDR fetch + decode. A ~100k-ledger range
replays in ~10-30 minutes.

### 4. Resume after a crash

```sh
stellarindex-ops backfill \
  -config /etc/stellarindex.toml \
  -from 50000000 \
  -to   50100000 \
  -resume
```

`-resume` reads the prior cursor (keyed on `source="backfill"`,
`sub_source` = `"<from>-<to>:<sources>"`) and skips ledgers
already processed. The cursor row gets upserted every ~256
ledgers during the run, so crash-and-restart loses at most
that many ledgers of progress (each replayable cleanly thanks
to the trades-hypertable primary-key dedupe).

### 5. Narrow the source set (optional)

```sh
stellarindex-ops backfill \
  -config /etc/stellarindex.toml \
  -from 50000000 -to 50100000 \
  -source soroswap,phoenix
```

By default the run uses `cfg.Ingestion.EnabledSources`. Override
with `-source <csv>` for a subset — useful when only one source
is missing data.

### 6. Verify

```sh
# Trade count for the range:
psql stellarindex -c "
  SELECT source, count(*)
  FROM trades
  WHERE ledger BETWEEN 50000000 AND 50100000
  GROUP BY source
  ORDER BY 1;
"

# Spot-check the most-recent rows:
psql stellarindex -c "
  SELECT source, ledger, base_asset, quote_asset, ts
  FROM trades
  WHERE ledger BETWEEN 50000000 AND 50100000
  ORDER BY ledger DESC, ts DESC
  LIMIT 5;
"

# CAGG materialisation should auto-trigger; verify by querying:
psql stellarindex -c "
  SELECT bucket, base_asset, quote_asset, vwap, trade_count
  FROM prices_1m
  WHERE bucket BETWEEN '2026-04-15' AND '2026-04-15 01:00'
    AND base_asset = 'native'
  ORDER BY bucket
  LIMIT 5;
"
```

If the CAGGs look empty for the backfilled range, refresh them by
hand — **all seven, `prices_1m` first**, over the range's timestamps:

```sql
CALL refresh_continuous_aggregate('prices_1m',
       '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_15m',  '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1h',   '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_4h',   '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1d',   '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('prices_1w',   '2026-04-01'::timestamptz, '2026-04-22'::timestamptz);
CALL refresh_continuous_aggregate('prices_1mo',  '2026-02-01'::timestamptz, '2026-05-01'::timestamptz);
-- twap_* are hierarchical over prices_1m — run them LAST.
CALL refresh_continuous_aggregate('twap_1h',     '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
CALL refresh_continuous_aggregate('twap_1d',     '2026-04-15'::timestamptz, '2026-04-21'::timestamptz);
```

Each window must span at least **2 buckets** of its own grain or the
call is rejected with `SQLSTATE 22023: refresh window too small` —
which is why the last three widen. `PadRefreshWindow`
(`internal/storage/timescale/diagnostics.go`) is the same arithmetic
the tool applies per chunk; the per-grain minimums are the
`MinWindow` values beside each entry of `CAGGsLiveForever`.

Do **not** expect the refresh policies to cover a historical range.
They only roll forward: `prices_1m`'s `start_offset` is 5 minutes and
the widest of the seven is `prices_1mo` at 3 months (migration 0002),
so a bucket older than that window is never materialised on their own
cadence, however long you wait.

## Failure modes

### `BackfillSafe=false` for source X

The CLI exits with:

```
backfill: source "<X>" is BackfillSafe=false; per-WASM-hash
audit required before historical replay
```

Run the WASM audit per `docs/operations/wasm-audits/README.md`,
then flip `BackfillSafe: true` in
`internal/sources/external/registry.go`. Re-run backfill.

### Cursor collision

If two operators try to replay overlapping ranges with the same
source set, both will write to the same `(source="backfill",
sub_source=...)` cursor row. The cursor key includes
`(from, to, sources)` so non-identical ranges don't collide;
identical ones share progress. To force a fresh run, change
the source set or the range by even one ledger.

### Galexie archive missing the range

Surfaces as:

```
ledgerstream: 404 fetching FC<...>.xdr.zst
```

Confirm the range is within the bucket's coverage:

```sh
ssh r1 "ls /var/lib/galexie/galexie-archive/<partition>/" | head
```

If the archive genuinely doesn't cover the range, cross-anchor
recovery is in `docs/operations/archival-node-bringup.md`
§"Disaster recovery".

## One-year retention catch-up (F-1265)

The service targets historical retention ≥ 1 year (ideally since
inception). Pre-launch R1 only has the
prices_1m data the indexer has filled since first deploy
(~7 days at audit time 2026-05-12); `/v1/chart?timeframe=1y`
truncates accordingly. This section walks through running the
catch-up backfill so the retention target is met on launch day.

### When to run it

Once before public-flip, and any time the operator's data window
shrinks back below 1 year (e.g. after a disaster-recovery
restore that started from a more recent snapshot).

### Scope

The catch-up runs ~1 year of pubnet ledgers — at typical Stellar
cadence of 5 s/ledger that's ~6.3M ledgers. Pacing depends on
the dispatcher's parallelism + the source set; budget 6–12 hours
wall-clock on a single R1 box at `-parallel 4`.

### Plan

1. **Resolve the target window.** The audit's data point: prod
   should anchor at "1 year ago today". Compute the
   corresponding ledger sequence via the Galexie archive's
   manifest:

   ```sh
   ssh r1 'ls -1 /var/lib/galexie/galexie-archive/2025-05-* | head -1'
   # Use the first ledger in the earliest archive bucket
   # within scope. Round DOWN to a multiple of 64.
   ```

2. **Sanity-check the upstream archive.** Catch-up reads only
   from the immutable archive bucket; the live bucket isn't
   in scope. Confirm no gaps:

   ```sh
   stellarindex-ops verify-archive \
     -from <year-ago> -to <today> \
     -bucket galexie-archive
   ```

3. **Estimate the row count.** Each Soroban DEX source emits
   roughly 50–500 trades per day at recent volume; aggregator
   prices_1m row count is bounded by (pairs × minutes). A
   1-year backfill across the audited Soroban set produces
   roughly 50–200 GB of trade rows + ~10–20 GB of CAGG
   materialisation (compressed: ~5×).

4. **Run in 1-week chunks.** Don't try the whole year as a
   single `-from`/`-to`: a crash mid-run is a 12-hour resume,
   and the run holds the source-cursor row for its duration.

   ```sh
   # Adapt to your range; each chunk is ~120k ledgers.
   for week_from in $(seq -w 50000000 120000 56000000); do
     week_to=$((week_from + 120000))
     stellarindex-ops backfill \
       -config /etc/stellarindex.toml \
       -from "$week_from" -to "$week_to" \
       -resume \
       -parallel 4 2>&1 | tee "backfill-${week_from}.log"
     # Stop if the chunk failed — don't paper over.
   done
   ```

5. **(Automatic since 2026-05-13; all seven grains since
   2026-09-04.)** Backfill auto-refreshes the price CAGGs at the end
   of each chunk — no manual step needed. If you're running on an
   older binary that lacks `-refresh-caggs`, append this after the
   trade-insert loop (`prices_1m` first — `twap_1h` / `twap_1d` are
   materialised from it).

   `<T_LO>` / `<T_HI>` are THIS chunk's timestamps — the wall-clock
   bounds of the ledger range the chunk just replayed, not the whole
   run's. Nine full-history calls is the shape to avoid:

   > **`NULL, NULL` would be the WHOLE view, not this run's range.**
   > Passing it re-materialises every bucket the aggregate has ever
   > covered, back to 2015. At r1's density that is order-of-10M
   > `prices_1m` rows per 30 days of history, in one uninterruptible
   > `CALL` — nine of them, `prices_1m` and both TWAP views included.
   > It is defensible only on a small or freshly-seeded deployment. On
   > anything else use the bounded form below, and walk a wide
   > `[T_LO, T_HI]` in weekly or monthly slices, under a heavy-job
   > scope and off the peak 14:00–22:00 UTC ingest window — the
   > sizing, the abort/monitor steps and the reason are in "Repairing
   > a range backfilled after 2026-08-22" below and in
   > [cagg-broad-recompute.md](cagg-broad-recompute.md).

   ```sh
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_15m', '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_1h',  '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_4h',  '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_1d',  '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_1w',  '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('prices_1mo', '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   psql stellarindex -c "CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>'::timestamptz, '<T_HI>'::timestamptz);"
   ```

   Each window must span at least two of that view's buckets or
   Timescale rejects the call with SQLSTATE 22023 — `prices_1mo`
   needs two calendar months, so widen `[T_LO, T_HI]` for the coarse
   rungs rather than narrowing them all to the chunk. This is the
   padding `PadRefreshWindow` applies for you on a current binary.

   A binary between 2026-05-13 and 2026-09-04 refreshed only
   `prices_1h` and coarser, so a range backfilled by one has no bars
   at 1m or 15m — see the repair section below.

6. **Verify — at 1m grain, not only at the default.** Check both:

   ```sh
   # (a) coarse coverage: non-truncated point set, earliest bucket
   #     where you expect it.
   curl -s '<host>/v1/chart?asset=native&quote=fiat:USD&timeframe=1y' \
     | jq '{n: (.data.points|length), truncated: .data.truncated, first: .data.points[0].t}'

   # (b) the fine grain THIS procedure materialises. `limit` DEFAULTS
   #     TO 100 and the query takes the EARLIEST buckets, so pass
   #     limit=1000 (the maximum) and keep the window under ~1000
   #     minutes; a whole day at 1m grain is 1440 buckets and would
   #     print the cap, hiding any hole that starts after it. Expect
   #     one bar per traded minute up to the cap, and never 0 for a
   #     pair that has trades in the window.
   curl -s '<host>/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from=<T_LO>&to=<T_HI>&limit=1000' \
     | jq '.data.intervals | length'
   ```

   Both filters reach INTO `.data`, and that is the whole of their
   correctness. Every 2xx on this API is an `Envelope` — `{data,
   as_of, flags, …}` — so `.data` is the payload OBJECT, never the
   array. `.data | length` on `/v1/ohlc` counts the six keys of
   `OHLCSeriesResponse` (`base`, `quote`, `interval`, `from`, `to`,
   `intervals`) and prints `6` whether `intervals` holds a thousand
   bars or none, which is exactly the failure below; the bars are
   `.data.intervals`. On `/v1/chart` the payload is `ChartSeries`, so
   `.data[0]` is `jq: error: Cannot index object with number` (exit
   5) and `.truncated` reads the envelope, where no such key exists —
   the series is `.data.points` and its flag `.data.truncated`.

   (b) is not optional padding. `?timeframe=1y` defaults to
   `granularity=1d` (ADR-0020's table), so it reads `prices_1d` and
   says nothing about `prices_1m` — it passed every day of the four
   months the 1m/15m hole was open. A check that cannot see the
   failure this step exists to prevent is not a check.

### Repairing a range backfilled after 2026-08-22

Every range backfilled by a pre-2026-09-04 binary is missing its
`prices_1m` and `prices_15m` buckets, and therefore its `twap_1h` /
`twap_1d` bars over the same span. The `trades` rows are intact
(migration 0031 removed their retention), so this is a pure re-derive
— no re-decode, no archive read:

```sql
-- [T_LO, T_HI] = the backfilled range's timestamps. prices_1m FIRST.
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_15m', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>', '<T_HI>');
```

Walk it in weekly or monthly slices rather than one call — the
minute grain is the row-heavy one (r1 accrues roughly 390k
`prices_1m` rows/day, so a 30-day slice materialises order-of-10M
buckets). Run it under a heavy-job scope and off the peak
14:00–22:00 UTC ingest window; the full-sweep procedure and its
abort/monitor steps are in
[cagg-broad-recompute.md](cagg-broad-recompute.md). A `55P03`
concurrent-refresh error just means the policy job is running —
retry.

> **Don't run these slices while a backfill is running.** Timescale
> serialises refreshes of one view by REJECTING the loser with
> `55P03` rather than blocking it, and the backfill's own retry
> budget is five attempts over ~3.0 s
> (`RefreshContinuousAggregate`). A manual `prices_1m` slice over a
> month outlives that budget many times over, so the backfill worker
> that collides with it exhausts all five attempts and fails its
> chunk — the cursor does not checkpoint, and that chunk has to be
> re-walked under `-resume`. Recoverable, but it costs the chunk.
> Finish or stop the backfill first, or scope the slices to ranges
> the run is not touching.

Confirm the repair from the served side, not the table — `.data` is
the `Envelope` payload object, so the bars are `.data.intervals` and
`.data | length` would print its key count (a constant `6`) instead:

```sh
curl -s '<host>/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from=<T_LO>&to=<T_HI>&limit=1000' | jq '.data.intervals | length'
```

### Failure & resumption

Each chunk runs with `-resume` so a crash mid-chunk re-anchors at
the last persisted cursor. Don't manually edit the cursor table —
that path is `stellarindex-ops backfill -resume` only.

### Retiring a shard's cursor row

`ingestion_cursors` has one permanent row per `(source, sub_source)`
and no retention, so every shard of every one-shot run leaves a row
behind when it finishes — or when it is abandoned. They accumulate:
an SDEX backfill abandoned in May 2026 left 91 rows, and by
September 2026 the table held 4,815 rows of which 4,703 had not been
written to in over a week. `list-cursors`, the `/diagnostics` page
and the public `/v1/diagnostics/cursors` endpoint all read that
table, so the dead rows become the bulk of what every consumer sees.

Reaping is a two-step procedure, and the first step is the decision:

1. **Confirm the work is over.** One-shot job rows past the 7-day
   boundary are published with `state: abandoned` (a live namespace
   never is — see step 2):

   ```sh
   curl -s 'https://api.stellarindex.io/v1/diagnostics/cursors?status=abandoned' | jq '.data | length'
   ```

   Then check what those shards still owe before deleting their
   resume points — `resume-stalled` prints the remaining range per
   cursor and skips the ones sibling coverage already closed:

   ```sh
   stellarindex-ops resume-stalled -config /etc/stellarindex/config.toml -dry-run
   ```

   A shard with real remaining work should be resumed, not reaped.

2. **Reap.** Previews by default; `-write` applies:

   ```sh
   stellarindex-ops reap-cursors -config /etc/stellarindex/config.toml
   stellarindex-ops reap-cursors -config /etc/stellarindex/config.toml -write
   ```

   `-older-than` (default `168h`, floor `24h`) sets the cutoff and
   `-source` narrows to one job's shards. The live namespaces —
   `ledgerstream` and `projector` — are never reaped at any age: an
   old row there means ingest is stuck, which is
   [cursor-stuck](runbooks/cursor-stuck.md), not garbage. Reaping
   deletes a RECORD, never data; the ledgers a shard walked stay in
   the lake and the served tier. What it deletes is the resume
   point.

## When NOT to use this

- **Live tail.** That's the indexer's job; backfill exits at
  `-to`.
- **Re-deriving the prices CAGG from existing trades.** Run
  `CALL refresh_continuous_aggregate(...)` directly; backfill
  re-decodes from XDR which is much heavier.
- **Source whose `BackfillSafe=false`.** Audit first (see
  `wasm-audits/README.md`). Skipping the audit risks
  silently-bad historical data per AGENTS.md "Soroban DeFi
  contracts upgrade in place".

## Cross-references

- [`internal/ops/ingest/backfill.go`](../../internal/ops/ingest/backfill.go) — implementation.
- [`internal/ops/ingest/reap_cursors.go`](../../internal/ops/ingest/reap_cursors.go) — `reap-cursors`, the cursor-row retirement path above.
- [`docs/operations/wasm-audits/README.md`](wasm-audits/README.md) — flip `BackfillSafe` once a Soroban source's WASM history is audited.
- [`internal/sources/external/registry.go`](../../internal/sources/external/registry.go) — `BackfillSafe` flag per source.
- [`migrations/0002_create_price_aggregates.up.sql`](../../migrations/0002_create_price_aggregates.up.sql) — CAGG definitions that materialise on inserted trades.
- [`docs/operations/archival-node-bringup.md`](archival-node-bringup.md) §"Disaster recovery" — when the Galexie archive itself is missing a range.
