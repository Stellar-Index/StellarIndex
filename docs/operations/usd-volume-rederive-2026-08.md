---
title: Operator runbook — re-derive and re-stamp trades.usd_volume, rebuild the CAGGs
last_verified: 2026-10-05
status: reference
---

# Re-derive and re-stamp `trades.usd_volume`, rebuild the CAGGs

Use this when `verify-usd-volume` reports violations, or a valuation fix
lands that changes how stored `usd_volume` is computed. Two tools, two
classes of row:

| class | tool | why |
|---|---|---|
| ESTIMATED tier (tier-3b FX/XLM bridge; needs the resolver waterfall) | `ch-rebuild` (Step 2) | one valuation implementation, `tradeUSDVolume`, shared with the insert path |
| EXACT tier (USD-pegged leg; a SQL identity `pegged_leg / 10^decimals`) | `usd-volume-restamp -tier exact` (Step 5) | |
| tier-4 XLM anchor (base leg is XLM) | `usd-volume-restamp -tier xlm-base` (Step 6) | needs `prices_1m` at the row's `ts`, so not spellable in SQL |

`usd-volume-restamp` has two more tiers (`xlm-quote`, `cex-fx`); see
`stellarindex-ops usd-volume-restamp -h`.

Invariants:

- Never hand-write SQL for a heterogeneous class; use the Go path.
- Every rewritten row carries the run's `derive_generation` (`now.Unix()`),
  and the upsert/UPDATE is guarded by `derive_generation <= gen`. A live
  gen-0 replay can never claw a correction back, and re-running a window is
  idempotent.
- Fix the insert path and deploy it FIRST (indexer + api + aggregator as
  needed). Until it is live the dirty span has no right edge. Record
  `SELECT max(ledger) FROM trades;` at deploy time as `L_HI`; pin the left
  edge with `SELECT min(ledger), min(ts) FROM trades WHERE ts >= '<first dirty day>';`.
- One heavy job at a time, ONE job name for every window and attempt of a
  run (`run-heavy-job.sh` locks per name and host-wide; exit 75 = the
  previous attempt or another heavy job is still alive).
- Source the env file first or the run fails 28P01 (the TOML's
  `postgres_dsn` carries a placeholder password):
  `set -a; . /etc/default/stellarindex; set +a`.
- Never inline `$$` SQL over ssh; ship SQL by `scp` + `-f file.sql`.
- Any hand SQL DML into compressed chunks: one transaction per window,
  `SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0`
  (never session-wide `SET`, which rides the pooled connection past
  COMMIT), predicate bound to the window's `ts` range.

## Step 2 — re-derive ESTIMATED-tier rows via `ch-rebuild`

```sh
/usr/local/sbin/run-heavy-job.sh ch-rebuild-sdex \
  /usr/local/bin/stellarindex-ops ch-rebuild \
    -config /etc/stellarindex.toml -ch-addr 127.0.0.1:9300 \
    -from <W_LO> -to <W_HI> -sdex -sources sdex -write
```

- `ch-rebuild` stamps `derive_generation` and installs the FX/peg
  resolvers itself. If `reDeriveNullVolumeGuard` fires, STOP: the host
  config's peg lists are missing.
- Windows of at most 50,000 ledgers (the SDEX read OOMs the 10 GiB client
  pin above that). Oldest to newest, one window at a time.
- `-sdex` is needed (SDEX is most of the volume). Under `-write`, scope
  with `-sources <name>` as the F050 BackfillSafe gate requires; see
  [history-completeness-plan.md](history-completeness-plan.md) §2.2.
- Decompress first: upserting into a compressed trades chunk crawls
  (~620 rows/s measured; ~100x faster after decompress). Pause the
  compression policy (`SELECT alter_job(<job_id>, scheduled => false)`),
  `decompress_chunk(...)` the overlapping chunks under the heavy wrapper,
  and re-enable (`scheduled => true`) after the last window.
- After each window, refresh `prices_1m` over that window's range before
  the next: it breaks the tier-3b circularity (the bridge reads `prices_1m`,
  a CAGG over `trades`) for later windows.

## Step 3 — CAGG rebuild over the span

`ch-rebuild` and `usd-volume-restamp` refresh nothing. The Go allow-list (`allowedCAGGViews`) is built from all 12
trades views plus the oracle and supply CAGGs, but this refresh runs as psql
on r1, under a heavy-job scope, over `[T_LO, T_HI]`
(pad at least 2x the bucket). The ORDER matters: `twap_1h`/`twap_1d` read
`prices_1m`, so refresh `prices_1m` first and the two `twap_*` last.

```sql
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_15m', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1h',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_4h',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1d',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1w',  '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('prices_1mo', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('dex_volume_by_pair_1d', '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('source_volume_1h',      '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('pools_per_source_1h',   '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1h',    '<T_LO>', '<T_HI>');
CALL refresh_continuous_aggregate('twap_1d',    '<T_LO>', '<T_HI>');
```

Then force the `asset_volume_24h` rollup (it re-sums `prices_1m.volume_usd`
and self-heals on its cadence; verify it did). `55P03` on a refresh means
the policy job holds it: retry. Never refresh concurrently with a
backfill. A restamp-tool run prints these 12 refreshes in this order; its
`acceptance:` line reads `trades` directly and cannot see them, so served
volume surfaces keep the pre-restamp numbers until they run. Run them
AFTER the whole span is restamped, never interleaved.

Compression backlog after the writes drains via the policy; if chunks stay
uncompressed for more than a day see the `compression-lag` runbook.

## Step 4 — acceptance

1. `stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -days 30`
   returns 0 violations. It is blind to tier 3/4: necessary, not sufficient.
2. XLM-base identity: for the span, estimated-tier rows with base `native`
   satisfy `usd_volume ~ base_amount/1e7 x XLM/USD(ts)` (expect under 1%
   relative delta except genuine bridge-tier rows).
3. Fleet magnitude: 24h `sum(usd_volume)` for `base_asset='native'` agrees
   with `sum(base_amount/1e7 x xlm_usd)` to within FX noise, and no row is
   more than 10x off its XLM leg.
4. Served surfaces: a dust pair's `/v1/price` returns the `price-withheld`
   problem type; `stellarindex_price_serve_substance_withheld_total` moves.
5. Determinism: re-run one already-corrected window; row md5s are
   byte-identical.

## Step 5 — re-stamp EXACT-tier rows (`usd-volume-restamp`)

Repairs rows whose quote or base leg is USD-pegged but whose stored
`usd_volume` differs from `pegged_leg / 10^decimals`. Use it for any
exact-tier violation `verify-usd-volume` reports.

The tool (`internal/ops/chops/usd_volume_restamp.go`):

- classifies each (source, base, quote) group per UTC day with the same
  `ClassifyUSDVolumeTier` and peg inputs as the insert path and verifier;
- rewrites only rows where the stored value `IS DISTINCT FROM` the
  identity, to the value the insert path writes (`round(leg / 10^d, 8)`),
  stamped with the run's generation under the `derive_generation <= gen`
  guard. Correct rows are untouched, so a re-run reports 0;
- leaves NULL rows alone unless `-fill-null` (a coverage change);
- walks `-from..-to` (inclusive UTC days, never today) oldest to newest in
  `-slice` windows (default 1h), one transaction each, with the `SET LOCAL`
  decompression cap lifted inside it;
- copies each rewritten row's prior `usd_volume` and `derive_generation`
  into `usd_volume_restamp_log` (migration 0175) in the same REPEATABLE
  READ transaction and refuses to commit a window whose before-image and
  UPDATE counts differ. To undo a run, apply the statement in 0175's
  header with the run's generation (the tool prints it);
- is dry-run by default; `-write` applies. Heartbeat is
  `ops_job="usd-volume-restamp"`, so the standing stall alerts apply.

```sh
# 0. size it (read-only)
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day <LAST_DAY> -days <N>
# 1. dry run: per-day candidate counts, sum|delta| before, no writes
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml -from <D0> -to <D1>
# 2. apply under the heavy wrapper, env sourced, same job name every attempt
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -from <D0> -to <D1> -write
# 3. acceptance: verify-usd-volume as in step 0 -> 0 violations
```

- Keep one heavy job to about 2-3 weeks of days; a re-run skips repaired rows.
- Do NOT pass `-fill-null` on the first pass: unpriced exact-tier rows are
  the coverage alerts' population. Fill them as a deliberate second pass
  after the value repair is accepted.
- Tier-3b (quote-side FX bridge) violations are `ch-rebuild`'s, not this
  tool's. The tier-4 XLM anchor is `-tier xlm-base` (Step 6).

### Step 5, chunk mode

In-place UPDATEs into compressed chunks run at roughly 1,574 rows/min
(measured 2026-09-03). For any window older than the compression policy's
7 days use `-chunks`: the same driver as Step 6, which decompresses each
chunk, restamps inside it, and re-compresses it (guards below).

```sh
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier exact -chunks -from <D0> -to <D1>            # dry run: chunk plan only
HEAVY_JOB_STOP_TIMEOUT=2h /usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier exact -chunks -from <D0> -to <D1> -write
```

`-chunk-batch`, `-report`, `-sample`, `-batch`, `-min-rel-delta` and
`-max-generation` are refused with `-tier exact`; `-slice` is the
per-transaction bound (narrow it, e.g. `-slice 15m`, for a busy span).

## Step 6 — re-derive tier-4 XLM-base rows (`-tier xlm-base`)

Population: on-chain DEX trades whose BASE leg is XLM (`native` or its SAC)
and whose QUOTE leg is not USD-pegged, written before the anchor became the
first route (`fd1860bd`, v0.25.0). The tool
(`internal/storage/timescale/usd_volume_restamp_xlmbase.go`):

- decides the tier in Go with the insert path's own `usdVolumeDecimals`; a
  USD-pegged quote is exact-tier and belongs to Step 5, so the tools cannot
  undo each other;
- rebuilds each row into a `canonical.Trade` and calls the store's own
  `tradeUSDVolumeViaXLMBaseAnchor` with the resolver from
  `InstallUSDVolumeResolution`, time-anchored to the ROW's `ts` (so the
  result is deterministic given `prices_1m`);
- stops at the anchor: when the anchor declines it reports the row
  (`anchor declined, stored NULL` / `stored VALUE`) and never falls through
  to the quote side, and never blanks a stored value;
- refuses a window whose top on-chain ledger is above the live
  `ledgerstream` cursor (`-allow-live-overlap` overrides);
- takes `-max-generation`, `-batch` (default 2000 rows/UPDATE), `-report`
  and `-min-rel-delta` (narrow a first pass to large moves; prefer the full
  pass).

```sh
# 1. REPORT first (read-only, refuses -write): the decision input
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier xlm-base -from <D> -to <D> -report -fill-null
# 2. value repair, oldest to newest, same job name each attempt, env sourced
/usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base -from <D0> -to <D1> -write
# 3. coverage fill as a deliberate second pass (or -fill-null in step 2 with -chunks)
#    same command with -fill-null
# 4. CAGG refresh over the span (Step 3, in Step 3's order)
# 5. acceptance
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day <LAST_DAY> -days <N>
```

Resume = rerun the same command from the day it stopped on (the tool prints
it; there is no cursor).

CAGG caveats for this tier:

- The price CAGGs compute `high/low/first/last_price` with a
  `FILTER (WHERE usd_volume >= 0.01)` dust floor (migrations 0115/0147), so
  filling NULL rows admits trades into OHLC extremes that were excluded:
  historical highs/lows on thin XLM pairs will move. Diff a few before/after.
- `prices_1m.volume_usd` feeds the resolver's tier-3b bridge leg
  (`queryXLMLeg`, `volume_usd >= 0.01`); the XLM anchor reads `native`
  through `queryDB` (floor `vwap * volume`), so the restamp cannot move its
  own inputs. A LATER `ch-rebuild` over the era may price token/token rows
  it could not before: a coverage gain, and the reason the refresh runs
  after the whole span, not interleaved.
- `asset_volume_character` needs no action (trailing-14-day rollup, 15-min
  cadence).

### Chunk mode (`-chunks`, both tiers)

Required for any window older than the compression policy's 7 days. In-place
DML on compressed chunks converges at ~1,574 rows/min; a first attempt over
28.6M rows was stopped with 0 rows committed.

The statement must carry a `ts` range. The batch UPDATE names the hypertable
and used to constrain `ts` only through the join (`t.ts = v.ts`), which
chunk exclusion cannot read: the planner targeted all 260 chunks (258
compressed), decompressing them wholesale (WAL archiving 5-6 to 240-417
segments/min, ~270 GB in an hour). `applyXLMBaseRestampBatch` now binds
`t.ts >= $2 AND t.ts <= $3` to the batch's own earliest and latest row,
binds `tx_hash` as `bpchar` (so `trades_pkey` serves the join), and pins
`SET LOCAL plan_cache_mode = force_custom_plan`. Keep all three.

**The cause was NOT the targeted chunk (measured 2026-09-06).** A second
attempt in `-chunks` mode reached chunk 1 of 91 — the smallest,
`_hyper_1_26385_chunk`, 211,786 rows, 16.2 MB — decompressed it
(`timescaledb_information.chunks` read `is_compressed = false` for the
whole UPDATE), and then one **23-row** UPDATE ran for 60 minutes on CPU
with no wait event and was stopped, again with nothing committed. The
statement, not the chunk, is what does not converge.

What the driver (`internal/ops/chops/usd_volume_restamp_chunks.go`,
`internal/storage/timescale/trades_chunks.go`) does on `-write`:

1. Resolves the `trades` compression policy job and refuses to start if
   there is none; takes the session advisory lock
   `hashtext('usd-volume-restamp:trades')` (one `-write` run per database;
   dropped with the connection, so SIGKILL cannot leave it); refuses if the
   policy is already unscheduled unless `-resume-paused-policy`; prints the
   re-enable statement; pauses the job (`alter_job(id, scheduled => false)`);
   waits up to 10 minutes for an in-flight policy fire to finish; re-lists
   chunks.
2. Per chunk, oldest first: probe read-only and skip chunks with nothing to
   change without decompressing (this is the resume mechanism); check free
   space against that chunk; `decompress_chunk(..., if_compressed => true)`;
   restamp in `-chunk-batch` UPDATE transactions (default 20,000, clamped to
   10,922 by the 65,535-placeholder protocol limit), reading `is_compressed`
   before each batch and STOPPING if something re-compressed the chunk;
   `compress_chunk(..., if_not_compressed => true)` as a deferred call. The
   bracket restores the state it found: an uncompressed chunk stays
   uncompressed.
3. On every exit (success, failure, SIGTERM) re-enables the policy on a
   context that survives cancellation, then releases the lock.

The pause is not optional: the policy selects chunks that are not fully
compressed, so a 12-hourly fire would re-compress the open chunk between
batches and the next batch would crawl with no error.

Guards:

- The dry run prints the chunk plan (count, compressed/uncompressed totals,
  largest chunk, per chunk line) and the pre-flight verdict; it decompresses
  and pauses nothing and takes no lock.
- Pre-flight: free space on the data volume must EXCEED 2 x the largest
  chunk's uncompressed size, re-checked before each decompress. Run it ON
  r1 as a role that can read `data_directory`. `-min-free-bytes N`
  overrides (loudly) after you have checked `zfs list` / `df`; it is then
  not re-measured.
- A window whose `-to` + 1 day is past `now() - compress_after` is refused
  (those chunks are deliberately uncompressed); `-allow-live-adjacent`
  overrides.
- `-generation` in the future is refused (it would lock out every default
  run on a money column). Carry `-generation N` across restarts so the span
  ends at ONE generation; the `RESUME:` line printed on failure does this.
- A failed chunk is re-compressed before a non-zero exit; a failed
  re-compress says `LEFT DECOMPRESSED`, a failed re-enable says
  `LEFT PAUSED`, each with the by-hand command.
- `-batch` is refused with `-chunks` (use `-chunk-batch`); `-chunk-batch`,
  `-min-free-bytes`, `-generation`, `-allow-live-adjacent` and
  `-resume-paused-policy` are refused without it.

Cost on the serving database: the heavy-job wrapper's `IOWeight`/`CPUWeight`
throttle only the ops binary; decompress, UPDATE and compress run in the
Postgres backend at serving priority. `decompress_chunk` takes an
`ExclusiveLock` then an `AccessExclusiveLock` at the end; `compress_chunk`
holds an `ExclusiveLock` throughout. Queries spanning the open chunk wait.
A decompressed chunk is about 15x larger (379 GB vs 25 GB across the 90
chunks of the 2026-01..07 window; largest 160 GB). Budget days, not hours
(measured decompress ~36 GB/h under load); read the first few chunks'
progress lines (seconds and bytes) and extrapolate before the largest one.

Two attempts at once: the advisory lock and the already-unscheduled refusal
exist because a second run would otherwise re-enable the policy at its exit
while the first is mid-chunk. To find a lock holder:

```sql
SELECT a.pid, a.application_name, a.backend_start, a.state, l.classid, l.objid
  FROM pg_locks l JOIN pg_stat_activity a USING (pid)
 WHERE l.locktype = 'advisory';
```

For the #372 xlm-base tier (the one-pass `-fill-null` run):

```sh
#    Value repair AND coverage fill in one pass (-fill-null): each chunk
#    is decompressed once; a second -fill-null pass would decompress all
#    90 again. ONE attempt at a time: the tool refuses a second while
#    the first holds the run lock.
HEAVY_JOB_STOP_TIMEOUT=2h /usr/local/sbin/run-heavy-job.sh usd-volume-restamp /usr/local/bin/stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml -tier xlm-base -chunks -from <D0> -to <D1> -fill-null -write
```

#### Stopping and SIGKILL

SIGTERM cancels the run: the batch in flight rolls back (committed ones
stay), the tool re-compresses the open chunk and re-enables the policy.
`run-heavy-job.sh` renders the scope with `-p TimeoutStopSec=` from
`HEAVY_JOB_STOP_TIMEOUT` (default `5min`, refused below 90 s; a bare integer
is SECONDS, write `2h` never `2`). This job's launch line sets
`HEAVY_JOB_STOP_TIMEOUT=2h` because re-compressing a 160 GB chunk outlives
the default; 2h is a budget, so record the largest chunk's measured
re-compress and tighten it. The bound is host state: r1 has it only after
`--tags heavy-job-wrapper` is applied (`grep -c TimeoutStopSec
/usr/local/sbin/run-heavy-job.sh` reads 1 when applied), and a scope already
running keeps the 90 s it was created with. See
[runbooks/ops-job-stalled.md](runbooks/ops-job-stalled.md) § "Stopping a
wrapped job".

After a SIGKILL the chunk stays DECOMPRESSED (not corrupt), the process
prints no summary, and the policy stays paused. The tool prints the by-hand
repair to stderr before each decompress and re-compress:

```
    SELECT compress_chunk('_timescaledb_internal._hyper_1_<id>_chunk');
    SELECT alter_job(<job_id>, scheduled => true);
```

After ANY exit that did not end in the tool's own summary, check:

```sql
-- chunks the policy would have compressed by now and has not:
SELECT chunk_schema, chunk_name, range_start, range_end
  FROM timescaledb_information.chunks
 WHERE hypertable_name = 'trades' AND NOT is_compressed
   AND range_end < now() - interval '7 days';
-- the policy itself (scheduled must be true):
SELECT job_id, scheduled FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades';
```

Compress a listed chunk by hand or let the re-enabled policy do it (a rerun
of the tool will not: it restores the state it lists). The policy stays
paused until you set `scheduled` back to true and run the `RESUME:` command,
or rerun with `-resume-paused-policy` so the tool owns the paused policy and
re-enables it at exit. `RESUME:` never carries `-resume-paused-policy`.

Acceptance is the `acceptance:` line the tool prints, byte for byte
(`verify-usd-volume ... -day <LAST_DAY> -days <N>`; `-day` is the LAST day,
`-days` counts back from it). Then Step 3.

What a run touches: `trades` rows in the window whose source is in the DEX
registry, base leg an XLM form, quote leg not a declared USD peg,
`derive_generation` at or below the run's, stored value differing from the
anchor's (with `-fill-null`, also NULL rows the anchor can price). It leaves
rows outside the window, exact-tier rows (Step 5), CEX/FX rows, token/token
pairs, pairs with XLM on the quote side, and rows the anchor declines.
