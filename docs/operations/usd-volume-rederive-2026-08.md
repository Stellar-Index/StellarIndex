---
title: Operator runbook — re-derive and re-stamp trades.usd_volume, rebuild the CAGGs
last_verified: 2026-10-05
status: reference
---

# Re-derive and re-stamp `trades.usd_volume`, rebuild the CAGGs

Use when `verify-usd-volume` reports violations or a valuation fix changes stored `usd_volume`.

| class | tool |
|---|---|
| ESTIMATED tier (tier-3b FX/XLM bridge) | `ch-rebuild` (Step 2), sharing `tradeUSDVolume` with the insert path |
| EXACT tier (`pegged_leg / 10^decimals`) | `usd-volume-restamp -tier exact` (Step 5) |
| tier-4 XLM anchor (base leg is XLM) | `usd-volume-restamp -tier xlm-base` (Step 6) |

`usd-volume-restamp` also has `xlm-quote` and `cex-fx` (see `-h`).

## Invariants

- Rewritten rows carry the run's `derive_generation` (`now.Unix()`); writes are guarded by `derive_generation <= gen`, so a gen-0 replay cannot undo a correction and re-runs are idempotent.
- Deploy the insert-path fix FIRST; record `SELECT max(ledger) FROM trades;` as `L_HI`, and the left edge via `SELECT min(ledger), min(ts) FROM trades WHERE ts >= '<first dirty day>';`.
- One heavy job at a time, ONE job name for every window and attempt
  (exit 75 = another is alive).
- Source the env first or the run fails 28P01: `set -a; . /etc/default/stellarindex; set +a`.
- Never inline `$$` SQL over ssh; `scp` + `-f file.sql`.
- Hand DML into compressed chunks: one transaction per window with `SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0`.

## Step 2 — ESTIMATED-tier rows via `ch-rebuild`

```sh
/usr/local/sbin/run-heavy-job.sh ch-rebuild-sdex \
  /usr/local/bin/stellarindex-ops ch-rebuild \
    -config /etc/stellarindex.toml -ch-addr 127.0.0.1:9300 \
    -from <W_LO> -to <W_HI> -sdex -sources sdex -write
```

- If `reDeriveNullVolumeGuard` fires, STOP: the host config's peg lists are missing.
- Windows of at most 50,000 ledgers (SDEX read OOMs above), oldest first.
  `-write` needs `-sources <name>` (BackfillSafe gate; see
  [history-completeness-plan.md](history-completeness-plan.md) §2.2).
- Decompress first (compressed upserts crawl, ~100x slower). `SELECT alter_job(<job_id>, scheduled => false)`,
  `decompress_chunk(...)` under the heavy wrapper, re-enable after the last window.
- Refresh `prices_1m` over each window before the next.

## Step 3 — CAGG rebuild over `[T_LO, T_HI]`

Neither tool refreshes anything. Run as psql on r1 under a heavy-job scope, padding 2x the bucket, AFTER the whole span is restamped. Order
matters: `prices_1m` first, `twap_*` last (they read `prices_1m`).

```sql
CALL refresh_continuous_aggregate('prices_1m',  '<T_LO>', '<T_HI>');
-- same call, in order, for: prices_15m prices_1h prices_4h prices_1d
-- prices_1w prices_1mo dex_volume_by_pair_1d source_volume_1h
-- pools_per_source_1h twap_1h twap_1d
```

Confirm `asset_volume_24h` re-summed. `55P03` = policy job holds it: retry. Served volume keeps old numbers until these run.

## Step 4 — acceptance

1. `stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -days 30`: 0 violations (blind to tier 3/4).
2. XLM-base: `usd_volume ~ base_amount/1e7 x XLM/USD(ts)` within 1%; 24h `native`-base sum matches within FX noise.
3. A dust pair's `/v1/price` returns `price-withheld`.
4. Re-running a corrected window gives byte-identical row md5s.

## Step 5 — EXACT-tier rows (`usd-volume-restamp`)

It rewrites only rows where stored `IS DISTINCT FROM` `round(leg / 10^d, 8)` (a re-run reports 0); NULLs stay unless `-fill-null`. It walks `-from..-to` (inclusive UTC days, never today) in `-slice` windows (default 1h), one transaction each. Before-images go to `usd_volume_restamp_log` (migration 0175); its header has the undo statement. Dry run by default.

```sh
stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day <LAST_DAY> -days <N>
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml -from <D0> -to <D1>   # dry run
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -from <D0> -to <D1> -write
```

- 2-3 weeks per job. No `-fill-null` on the first pass (unpriced rows feed the coverage alerts); fill as a second pass.
- Windows older than 7 days: use `-chunks` (below). `-chunk-batch`, `-report`, `-sample`, `-batch`, `-min-rel-delta`, `-max-generation` are refused with `-tier exact`.

## Step 6 — tier-4 XLM-base rows (`-tier xlm-base`)

Population: DEX trades with an XLM (`native` or SAC) BASE leg and a non-USD quote. The anchor runs at the ROW's `ts`, never falls through to the quote side or blanks a stored value, and the tool refuses a window above the live `ledgerstream` cursor (`-allow-live-overlap`). Also takes `-max-generation`, `-batch`, `-report`, `-min-rel-delta`.

```sh
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier xlm-base -from <D> -to <D> -report -fill-null     # read-only decision input
/usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base -from <D0> -to <D1> -write
# coverage fill: same with -fill-null; then Step 3 and verify
```

Resume = rerun from the printed stop day.

- Filling NULLs moves historical OHLC extremes on thin XLM pairs (CAGG dust floor `usd_volume >= 0.01`).

## Chunk mode (`-chunks`, both tiers)

In-place DML on compressed chunks crawls (~1,574 rows/min). The UPDATE must carry a `ts` range chunk exclusion can read: `applyXLMBaseRestampBatch` binds `t.ts >= $2 AND t.ts <= $3`, binds `tx_hash` as `bpchar` and pins `SET LOCAL plan_cache_mode = force_custom_plan`. Keep all three.

```sh
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier exact -chunks -from <D0> -to <D1>                # dry run: chunk plan
HEAVY_JOB_STOP_TIMEOUT=2h /usr/local/sbin/run-heavy-job.sh usd-volume-restamp \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier xlm-base -chunks -from <D0> -to <D1> -fill-null -write
```

xlm-base: repair and fill in one pass (a second re-decompresses every chunk). On `-write` the driver (`internal/ops/chops/usd_volume_restamp_chunks.go`):

1. Takes advisory lock `hashtext('usd-volume-restamp:trades')`, refuses an already-unscheduled policy (unless `-resume-paused-policy`), pauses it (mandatory: else it re-compresses the open chunk mid-run) and waits up to 10 min.
2. Per chunk, oldest first: skip no-op chunks (the resume mechanism), `decompress_chunk`, restamp in `-chunk-batch` UPDATEs (default 20,000, max 10,922), `compress_chunk`.
3. On every exit re-enables the policy and releases the lock.

Guards:

- Free space must EXCEED 2 x the largest chunk's uncompressed size; run ON r1 (`-min-free-bytes N` overrides).
- A window within `compress_after` of now is refused (`-allow-live-adjacent`); a future `-generation` is refused. Carry `-generation N` across restarts (the `RESUME:` line does).
- `-batch` is refused with `-chunks`; the chunk-only flags are refused without it.

Cost: Postgres runs it at serving priority with heavy locks; a decompressed chunk is ~15x larger (largest 160 GB, ~36 GB/h): budget days.

### Stopping and SIGKILL

SIGTERM rolls back the batch in flight, re-compresses and re-enables the policy. `HEAVY_JOB_STOP_TIMEOUT` sets `TimeoutStopSec` (default `5min`, minimum 90 s; a bare integer is SECONDS: write `2h`); r1 has it only after `--tags heavy-job-wrapper`. See [ops-job-stalled](runbooks/ops-job-stalled.md) § "Stopping a wrapped job".

After SIGKILL the chunk stays DECOMPRESSED (not corrupt), policy paused. By hand: `SELECT compress_chunk('_timescaledb_internal._hyper_1_<id>_chunk');` then `SELECT alter_job(<job_id>, scheduled => true);`.

After any exit without the tool's summary, list uncompressed `trades` chunks older than 7 days and the `policy_compression` job's `scheduled` flag. Compress by hand or let the policy; then rerun with `-resume-paused-policy`, or set `scheduled` true and run the `RESUME:` command.

Acceptance is the printed `acceptance:` line (`-day` is the LAST day). Then Step 3.
