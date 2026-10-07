---
title: Runbook — projector
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — projector alerts

Rule file: `configs/prometheus/rules.r1/projector.yml` (same rules as `deploy/monitoring/rules/projector.yml`), `component: projector`. All alerts are `severity: ticket` (P3) except `stellarindex_projector_i128_overflow`, which is `page` (SEV-1). Implementation: `internal/projector/`; design: ADR-0032 (per-source tables as projections), ADR-0029 (soroban_events landing zone, the legacy raw store), ADR-0034 #10 / ADR-0041 (ClickHouse `contract_events` feed switch).

Why P3: under Phase-3 parallel mode the dispatcher's per-source sink is still primary for most sources, so `lag_high` and `error_rate_high` are mostly visibility-only; `row_quarantined`, `decode_error_rate_high`, `wedged` and the other row/decode alerts below can lose or freeze served data. **Exception: `sep41` and `rozo`.** The indexer runs `SinkModeSkipSoleWriter`: the projector is the SOLE writer for every source whose spec sets `SoleWriter` (`internal/pipeline/source_spec.go`; today the sep41 pair and rozo), so trouble there is real, customer-visible data lag, and "disable the projector" is not a safe lever for it (it stops the only writer). Re-promote the family to P2 once `[ingestion.persist_per_source]=false` (Phase 4, `SinkModeSkipProjected`); flipping the writer is unsafe while lag is unbounded.

**Where the projector reads from.** By default it tails the ClickHouse Tier-1 lake's `contract_events`: `storage.clickhouse_projector_source` defaults to **true** (it requires `clickhouse_live_sink`). Postgres `soroban_events` is the legacy fallback, used only when that flag is off. Both paths share the same `ingestion_cursors` rows and the same lag gauge. Confirm which this host is on:

```sh
journalctl -u stellarindex-indexer --no-pager | \
  grep -F 'projector reading from ClickHouse lake' | tail -1
# present = CH lake (default);  absent = legacy soroban_events read
```

**Skip semantics (shared by the row and decode alerts).** The projector's per-row failure paths eventually skip the row (after the retry budget, one row per cycle; `sink_permanent` holds the cursor for about 1 h first) and advance the cursor, because holding would re-wedge a sole-writer source on a deterministic failure (COR-11). The raw event always survives in the lake and is re-driven with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write` once the defect is fixed (see [the replay section](#stellarindex_projector_replay_stalled)). Re-driving without fixing the defect just re-drops the same rows. A sustained skip on a source eventually shows as `complete=false` on the ADR-0033 completeness verdict (`data-freshness.md#stellarindex_completeness_incomplete`); after a repair, the verdict returns to `complete=true` at the next `compute-completeness` run.

Shared commands:

```sh
# Per-source projector cursors (should be close to the ledgerstream tip)
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors \
   WHERE source = '"'"'projector'"'"' ORDER BY sub_source"'

# Live ledgerstream tip
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT last_ledger FROM ingestion_cursors \
   WHERE source = '"'"'ledgerstream'"'"' AND sub_source = '"'"''"'"'"'

# Metrics (indexer): curl -s http://indexer:9464/metrics | grep stellarindex_projector_
# Per-cycle log lines: journalctl -u stellarindex-indexer | grep "projector cycle"
#   (rows_scanned, events_emitted, decode_errors, lag_ledgers, elapsed)
```

## At a glance

- [`stellarindex_projector_lag_high`](#stellarindex_projector_lag_high): a source is more than 256 ledgers behind the tip for 10m.
- [`stellarindex_projector_error_rate_high`](#stellarindex_projector_error_rate_high): cycle errors sustained for 15m.
- [`stellarindex_projector_replay_stalled`](#stellarindex_projector_replay_stalled): an operator replay stopped advancing; also the full `projector-replay` operator reference.
- [`stellarindex_projector_row_quarantined`](#stellarindex_projector_row_quarantined): the poison-row escape hatch skipped one row.
- [`stellarindex_projector_row_dropped_permanent`](#stellarindex_projector_row_dropped_permanent): the sink permanently rejected a row's values.
- [`stellarindex_projector_i128_overflow`](#stellarindex_projector_i128_overflow): SEV-1, an int64 reached an amount path.
- [`stellarindex_projector_decode_error_rate_high`](#stellarindex_projector_decode_error_rate_high): sustained decode failures, likely a decoder regression.
- [`stellarindex_projector_decode_error_ratio_high`](#stellarindex_projector_decode_error_ratio_high): same, on a quiet source.
- [`stellarindex_projector_wedged`](#stellarindex_projector_wedged): cursor stuck at the 25-ledger window floor.

## stellarindex_projector_lag_high

Trips: `max by (source) (stellarindex_projector_lag_ledgers) > 256` for `10m`, `unless max by (source) (stellarindex_projector_replay_window_active) == 1`. Typical MTTR 30 min.

Impact: per-source projection tables (`trades`, `blend_*`, `phoenix_*`, `cctp_events`, ...) increasingly diverge from the authoritative event store. For the sole-writer sources (sep41, rozo) that is served-data lag; for other sources in Phase 3 the dispatcher's sink still writes, so customer-facing rows are unaffected.

Symptoms:

- `stellarindex_projector_lag_ledgers{source="<name>"}` > 256 for 10+ min.
- `stellarindex_projector_cycle_duration_seconds_bucket{source="<name>"}` p99 > 30 s (`projector.PerSourceTimeout` is 60 s).

Quick diagnosis (5 min): run the shared cursor and tip queries above, then tail the log for the lagging source: `ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 | grep "projector cycle" | grep <source>'`. If the cursor isn't moving at all, go to mitigation. If it moves but slower than the live tip, this is honest catch-up after an outage; let it run unless lag exceeds a few hours.

**Held at a lake hole.** In ClickHouse read mode the scan is clamped to the lake's contiguous-completeness watermark, so a missing lake ledger stops the source there on purpose (reading past it would lose that ledger's events). Such a cycle counts as `outcome="watermark_held"`, not `idle`, and the lag gauge still measures against the ledgerstream tip, so it grows:

```promql
sum by (source) (increase(stellarindex_projector_runs_total{outcome="watermark_held"}[15m]))
```

Non-zero means the lake has a hole, not the projector: the log line `held at the lake's contiguous watermark` names the `watermark` ledger, and every source that has reached it stops at the same one. Heal the lake with the `ch-live-catchup` timer ([ch-live-sink-drops](clickhouse.md#stellarindex_ingestion_ch_live_sink_drops)); the projector resumes on its own once the watermark moves. Do not rewind the projector cursor for this.

Mitigation (15 min):

- [ ] Step 1: check the dispatcher's per-source sink is still writing rows (Phase 3 safety net). If yes, customer impact is bounded and this alert is operational only (not for sep41).
- [ ] Step 2: if `outcome="error"` rate is high, inspect log lines tagged `component=projector` for the failing source. Common causes: postgres connection saturation, downstream PK constraint failure on a malformed event, decoder panic.
- [ ] Step 3: **catch-up after a real outage.** If the cursor is simply behind (moves too slowly, or restarted from a lower watermark), rewind it and let the running projector tail forward. `-config`, `-source` and `-from` are ALL required; the source name is the projector's own name (`internal/projector/registry.go`), not the hyphenated table name `find-data-gaps` prints:

  ```sh
  # Dry run is the DEFAULT: prints the intended rewind, writes nothing.
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source <name> -from <ledger>

  # -write is REQUIRED to actually rewind the cursor.
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source <name> -from <ledger> -write
  ```

  This is a one-shot cursor rewind, not a heavy job (it does block while the projector catches up; see the timeout below). The projector goroutine in `stellarindex-indexer` does the re-projection on its next cycle; every per-source writer's generation-guarded upsert makes it idempotent. The projector writes at `derive_generation` 0, so a replay cannot correct a row a re-derive stamped higher; use `projected-rebuild -write` for that. An unknown `-source` fails loudly. A whole-history re-derive is `projected-rebuild`, run under `run-heavy-job.sh`. Full procedure: [replay section](#stellarindex_projector_replay_stalled).
- [ ] Step 4: if the projector is wedged on one source, go to [stellarindex_projector_wedged](#stellarindex_projector_wedged) FIRST. Disabling the projector (`[ingestion.projector] enabled = false` in `/etc/stellarindex.toml` + `systemctl restart stellarindex-indexer.service`) is a last resort; for sep41 it STOPS the only writer.
- [ ] Verification: `stellarindex_projector_lag_ledgers` drops below 256 within 30 minutes (or the alert clears).

Root cause analysis:

- `journalctl -u stellarindex-indexer | grep projector` for the lag episode.
- `sum by (source, outcome) (increase(stellarindex_projector_runs_total[6h]))` for the failure-class breakdown.
- `sum by (source, outcome) (increase(stellarindex_projector_events_decoded_total[6h]))` to separate decode failures from sink failures.

Known false positives:

- Fresh deploy: the projector starts at `(projector, <source>)` cursor = 0 and catches up from the soroban-era genesis ledger. The 10-minute `for:` absorbs cold start; if it fires, raise to 30 min temporarily.
- After a soroban-events landing-zone backfill, lag spikes while the projector catches up to the new rows. Let it drain.
- An OPERATOR-INITIATED `projector-replay` rewind does not reach this alert (issue #325): the replay tool records the rewind window, the projector publishes `stellarindex_projector_replay_window_active{source}=1` while the cursor climbs back through it, and the rule carries `unless ... == 1`. So if this alert IS firing, no recorded `projector-replay` rewind explains it; treat the lag as real. A pending `projected-rebuild` window does NOT excuse lag (the flag is gated on the window's `reason`), so a source held at a rebuild's range stays alertable. The mirror signal for a replay that stopped climbing is [stellarindex_projector_replay_stalled](#stellarindex_projector_replay_stalled).

## stellarindex_projector_error_rate_high

Trips: `sum by (source) (rate(stellarindex_projector_runs_total{outcome="error"}[15m])) > 0.05` for `15m`. Same impact, severity and runbook body as [stellarindex_projector_lag_high](#stellarindex_projector_lag_high); the difference is that this fires on failing cycles rather than on distance from the tip. The cursor may not be advancing; check before assuming the backlog is growing.

Work it from lag_high's mitigation Step 2 (inspect `component=projector` log lines for the failing source: postgres connection saturation, PK constraint failure on a malformed event, decoder panic) and its root-cause queries (`runs_total` and `events_decoded_total` by outcome). Verification: the `outcome="error"` rate returns to ~0 and lag falls. If one source's cursor is pinned at the window floor, see [stellarindex_projector_wedged](#stellarindex_projector_wedged).

## stellarindex_projector_replay_stalled

Trips: replay window flag is 1 AND lag > 256 AND the current lag is at its own 15m maximum (it has not fallen for 15 min), `for: 5m` (about 20 min total). Meaning: an operator-initiated `projector-replay` is in progress (cursor still inside the recorded rewind window) but has stopped advancing, or the live tip is outrunning it. The served-row deficit the replay was started to repair is still open. The `> 256` floor is the same bound `lag_high` uses and is load-bearing: without it a fully caught-up source would ticket whenever its window flag was 1.

Triage first: [stellarindex_projector_wedged](#stellarindex_projector_wedged), the `runs_total{outcome="error"|"sink_retry"}` rates for the source (a floored window or a held sink is the usual cause), and the [decompress-first pre-flight](#pre-flight-decompress-the-replay-window-first) below. If the replay was started with a stalled `-wait`, the command itself exits non-zero (see [After the rewind](#after-the-rewind-the-command-waits-then-refreshes-the-price-caggs)).

The rest of this section is the operator reference for `projector-replay`.

### At a glance: the tool

| Field | Value |
| ----- | ----- |
| Trigger | Per-source projection is stale or missing rows for a known ledger range (e.g. an outage gap; a post-decoder-fix re-walk over rows a re-derive already stamped needs `projected-rebuild -write`). |
| Tool | `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write` (fail-closed: no `-write` = dry run) |
| Typical wall time | The rewind is at most 5 s of SQL, but the command does **not** return then: by default it blocks until the projector has re-walked the range (about 1 min per 100k ledgers per source, bounded by `-wait-timeout`, default 30 min) and then re-materializes the seven `prices_*` continuous aggregates over it (its context allows a further 30 min). Run it under `tmux`/`screen`, not a bare ssh session. `-wait=false` or `-refresh-caggs=false` return immediately and hand the refresh to you; see [After the rewind](#after-the-rewind-the-command-waits-then-refreshes-the-price-caggs). |
| Impact | Data-safe, not load-free. The rewind only moves a cursor, and the per-source writers' generation-guarded upsert makes re-writes idempotent. The load is what follows: the projector re-walks the range (a replay through compressed chunks livelocks; see the pre-flight below), and the post-replay refresh runs seven `refresh_continuous_aggregate` calls over the replayed time range. Each is padded to its view's minimum window (up to about 93 days for `prices_1mo`), reads `trades`, and can contend with that view's own refresh policy (Timescale rejects the loser with 55P03; the store retries within a bound). |

### Is this the right tool?

`projector-replay` is bound by the live projector's tick cadence (5s `Interval`) and 60s `PerSourceTimeout` per cycle: roughly a 720k-ledger/hour ceiling. For a rewind bigger than about **1M ledgers**, use **`stellarindex-ops projected-rebuild`** (ADR-0048 D3): parallel workers, no per-cycle deadline, 10-20x the throughput, same decoders and idempotent writes. See [docs/architecture/ingest-pipeline.md](../../architecture/ingest-pipeline.md#re-deriving-from-the-lake) ("`projector-replay` vs `projected-rebuild`") and the doc comment in `internal/ops/chops/projected_rebuild.go` for the one-writer contract (the two tools must never run concurrently against overlapping history for the same source). `projected-rebuild` exits non-zero when a run held any window (un-checkpointed after failed inserts; re-run to retry) or permanently dropped any trade; its summary says which.

This holds for a migration's follow-up too: one written as `projector-replay -config /etc/stellarindex.toml -source X -from N` (0137, 0164, 0203) goes through `projected-rebuild` when `N` is more than about 1M ledgers behind the tip.

`projected-rebuild -resume` (the default) skips every window that has a checkpoint (`ingestion_cursors`, `source = 'projected-rebuild'`, `sub_source = 'X:<from>-<to>'`) without checking the table still holds its rows. A migration that empties a projected table therefore deletes that source's checkpoints in the same file (0206 did it for comet, cctp, rozo and sushiswap_v3; `lint-migrations.sh` pass 10 enforces it). Do not run a `projected-rebuild` for that source while the migration applies: a window it checkpoints before the `DELETE` is skipped afterwards. If a table is empty over a range its checkpoints claim, delete that source's `projected-rebuild` rows from `ingestion_cursors` by hand, in the shape of 0206's `DELETE`, then re-run.

### Why it exists

ADR-0032 Phase 5 (rc.97) **deleted** the `*-backfill` operator subcommands (`cctp-backfill`, `rozo-backfill`, `soroswap-skim-backfill`, `comet-liquidity-backfill`, `phoenix-backfill`, `blend-backfill`, `sep41-transfers-backfill`, `drain-cascade-window`). `projector-replay` is the primary catch-up path for SMALL projected-source rewinds, as one cursor rewind:

```sh
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
  -source <name> -from <ledger> -write
```

Fail-closed: without `-write` it prints the intended rewind and writes nothing. The projector goroutine in `stellarindex-indexer` is already tailing; rewinding the per-source cursor makes it re-project the window on its next cycle (5 s projector interval). Per-source writers upsert guarded by `derive_generation <= EXCLUDED.derive_generation`, and the projector writes at generation 0: a replay re-writes gen-0 rows but cannot correct a row a re-derive (`projected-rebuild`, `ch-rebuild`) already stamped higher. Correct those with `projected-rebuild -write`.

### Quick diagnosis (5 min)

```sh
# 1. Where is the projector's per-source cursor? (shared cursor query above)

# 2. What rows are present in the per-source table for the range?
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT MIN(ledger), MAX(ledger), COUNT(*) FROM trades \
   WHERE source = '"'"'aquarius'"'"' AND ledger BETWEEN 62000000 AND 62100000"'

# 3. What rows are in soroban_events for that range + the source's topic?
#    Events but no per-source rows: replay will populate. No events: nothing to do.
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT COUNT(*) FROM soroban_events \
   WHERE ledger BETWEEN 62000000 AND 62100000 AND topic_0_sym = '"'"'swap'"'"'"'
```

### Replay procedure

```sh
# Dry-run first (also the default).
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
  -source aquarius -from 62000000 -dry-run

# Live: -write is REQUIRED to actually rewind the cursor.
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
  -source aquarius -from 62000000 -write
```

Source names match the projector registry (`internal/projector/registry.go`): `aquarius`, `soroswap`, `phoenix`, `comet`, `blend`, `cctp`, `rozo`, `defindex`, `sep41_transfers`, `sep41_supply`, `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`. Soroswap skim rows replay under `soroswap`; there is no soroswap-skim projector source.

Spelling matters and the list is not exhaustive; the registry is the authority. The `sep41_*` and `blend_*` sources are **underscored** (`sep41_transfers`, `sep41_supply`, `blend_backstop`); the hyphenated per-table names `find-data-gaps` prints are not valid here. An unknown `-source` fails non-zero rather than printing "no action". So does a source that has never run: it has no cursor row until its first cycle (which starts at the source's declared `Source.Genesis` or the lake floor), so there is nothing to rewind. A `-from` at or above the current cursor exits 0 with "nothing to rewind" and the number of ledgers still ahead of the cursor: the forward pass has not reached that range yet, so it has not been projected.

### After the rewind: the command waits, then refreshes the price CAGGs

A replay re-projects trades into a **historical** time range (aquarius, soroswap, phoenix and comet all persist trades through the projector). Every continuous aggregate over `trades` only rolls its refresh policy over its own `start_offset` window (five minutes for `prices_1m`), so nothing picks those historical buckets up on its own: the re-projected rows are durable in the hypertable but invisible to `/v1/ohlc`, `/v1/chart`, `/v1/vwap` and `/v1/history/since-inception`, which read the aggregates.

With `-write` the command finishes the job itself:

1. records the dirty window and rewinds the cursor;
2. polls the projector cursor every 5 s until it is back at the ledger it sat at **before** the rewind (the range is actually re-projected; refreshing earlier would materialize the old, short answer);
3. re-materializes `prices_1m`, `prices_15m`, `prices_1h`, `prices_4h`, `prices_1d`, `prices_1w` and `prices_1mo` over the replayed range and prints `price CAGGs re-materialized over the replayed range [from,to]`.

| Flag | Default | Effect |
| ---- | ------- | ------ |
| `-refresh-caggs` | `true` | `false` skips steps 2-3 and logs a warning: the range stays unmaterialised until a manual refresh covers it. |
| `-wait` | `true` | `false` returns right after the rewind and logs that the refresh is now yours. |
| `-wait-timeout` | `30m` | Budget for step 2. Size it from the rewind: about 1 min per 100k ledgers, plus head-room. |

The dry run prints the wait-and-refresh it would perform.

**Exit codes.** Both opt-outs exit 0. Non-zero after the `projector cursor rewound` line means the rewind IS durable and the refresh did NOT complete:

- *cursor still short of the pre-rewind ledger after `-wait-timeout`*: the projector is slow or wedged (check the pre-flight below and [lag_high](#stellarindex_projector_lag_high)). No view was refreshed.
- *post-replay CAGG refresh ... failed*: every view is still attempted after one fails, and the error names the ones that did not materialize.

Either way, once the projector has caught up, finish one of two ways. Do NOT re-run with the same `-from`: a plain re-run recomputes the rewind target from the now partially-advanced cursor and re-walks the whole range again, never refreshing the gap left by the timed-out run. Recover with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -refresh-only -refresh-to <ledger>` (refuses until the projector has re-walked up to `-refresh-to`). For a large range you can also refresh the `trades` rollups with `stellarindex-ops trades-cagg-refresh -config PATH -from <ledger> -to <ledger>`, which runs the same safe order.

The refresh set is the one `backfill` uses: every `trades` rollup (`timescale.TradesCAGGs`, including `twap_1h` / `twap_1d` after a forced `prices_1m`) and, if the range wrote oracle rows, every `oracle_prices_*` rung (`timescale.OracleCAGGs`). While `prices_1m`'s retention policy (migration 0156) is armed the twap refresh is refused and the command fails naming them; disarm it as that migration states and re-run.

### Verification

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 100 -f | grep projector'
```

Cycle log lines (one per minute when catching up) show progress; `stellarindex_projector_lag_ledgers{source="<name>"}` falls to 0 once caught up to the live tip. The command's own last line is the other half: `price CAGGs re-materialized over the replayed range [from,to]` and exit 0. If you ran with `-wait=false` or `-refresh-caggs=false`, or it exited non-zero after the rewind, the projection is repaired but the served OHLC/VWAP history over the range is not.

### What the alerts do while a replay runs (issue #325)

The rewind is recorded (`projection_dirty_windows`, migration 0125) BEFORE the cursor moves, and the projector republishes `stellarindex_projector_replay_window_active{source}` = 1 every 30s while the cursor is inside that recorded window. Expect:

- **`stellarindex_projector_lag_high` does NOT ticket for this source** while the replay climbs (its `unless replay_window_active == 1` arm). Before 2026-08-29 the 2,574,496-ledger reflector-fx replay held that ticket open for about 4h, telling the operator nothing new and masking any genuine lag on the same source.
- **`stellarindex_projector_replay_stalled` DOES ticket** (after about 20 min) if the replay stops advancing: lag still over 256 and flat or rising for 15 min inside the window. The two rules partition the space: under 256 ledgers neither speaks, above it exactly one does.
- **The excuse expires with the catch-up, not with the row.** The flag clears the moment the cursor regains its pre-rewind position, even though the dirty-window row survives until `compute-completeness` re-verifies the range. Any lag after that is ordinary lag and tickets normally, including a projector wedged exactly AT that ledger.
- **A `projected-rebuild` window never raises the flag, because of its `reason`, not its range.** `projected-rebuild -write` records into the same `projection_dirty_windows` table and its range is NOT bounded below the live cursor: `-to` defaults to the live cursor, the one-writer guard admits `liveLastLedger >= to`, and `-allow-live-overlap` skips the guard (as on r1 2026-07-27, `-source sep41_supply -from 63419138 -to 63671020 -write -allow-live-overlap`). The flag is gated on the window's `reason` recording a `projector-replay` rewind, so lag alerting is unchanged while a rebuild is pending, including for a source held at the rebuild's top ledger. If a replay is recorded while a rebuild window is pending, the single per-source row widens and the flag expires at the higher `to_ledger`; clear the pending window with a `compute-completeness` run first for the tighter bound.

Known false positive: replaying a range earlier than the source's Soroban genesis is a no-op (no events). The cursor rewinds, the next cycle scans an empty range and advances back to the same `toLedger`.

### Pre-flight: decompress the replay window FIRST

Replays through compressed Timescale history **livelock** (twice on 2026-07-31/08-01): retro-fill upserts into compressed `trades` and `aquarius_*` chunks blew write deadlines, the projector held the cursor and retry-stormed (100k+ abandoned inserts/hour), and the replay froze until the chunks were decompressed. Compressed-chunk upserts decompress segments per conflict; batch replays hit the same segments repeatedly. The nightly 22:45Z policies recompress, so a chunk decompressed yesterday re-blocks today, and any table newly given a policy joins the trap silently.

**Before ANY replay whose window overlaps compressed chunks**, decompress every compressed chunk of EVERY table the source writes (`trades` plus its per-source hypertables) across the window's time range:

```sql
SELECT format('SELECT decompress_chunk(%L);',
              c.chunk_schema||'.'||c.chunk_name)
FROM timescaledb_information.chunks c
WHERE c.hypertable_name IN ('trades', '<source tables...>')
  AND c.is_compressed
  AND c.range_end > '<window start ts>';
```

Run the emitted statements (large chunks under `run-heavy-job.sh`), then start the replay. The nightly policy recompresses afterwards; no manual re-compression needed. If a replay is ALREADY wedged, the signature is `Insert*: context deadline exceeded` storms with a frozen cursor; decompress the blocking chunk and (since v0.21.12) the sink-side adaptive shrink converges the window automatically.

## stellarindex_projector_row_quarantined

Trips: `increase(stellarindex_projector_events_decoded_total{outcome="sink_quarantined"}[15m]) > 0`, or the `sink_quarantined` child born non-zero inside those 15m (the child is unseeded, so `increase()` alone reads 0 on the first one), `for: 0m`. One quarantine tickets at once and the ticket clears 15m later on its own; the row is still skipped, so work from the journal, not from whether the alert is still up. Known false positive of the born-inside-the-window arm: a Prometheus scrape gap at t-15m makes an already-non-zero child read as newly born and tickets for up to 15m with no new quarantine. Otherwise none: a quarantine is a rare, deliberate event (at most one per source per cycle), so investigate every one; repeated quarantines on a source are themselves the signal of a systemic decoder or schema regression.

Impact: ONE event is durably skipped from the served tier for the source. Not data loss (the raw event is in the ClickHouse lake) but a real, silent gap until re-driven. Typical MTTR: minutes to diagnose; re-drive is fast once the defect is fixed.

Why it exists (COR-11/COR-01): the sink can hold a row that fails deterministically forever (a poison row), and the cursor can only advance past a ledger once every event in it has committed, so one such row used to wedge the source permanently. `quarantineCandidate` (`internal/projector/projector.go`) now gives up on **at most one** held row per cycle once its per-row retry budget is exhausted, bumps `stellarindex_projector_events_decoded_total{source,outcome="sink_quarantined"}`, and lets the cursor advance. This alert is the only signal that it happened.

Symptoms:

- `stellarindex_projector_events_decoded_total{source="X",outcome="sink_quarantined"}` incremented.
- Indexer journal: `projector: QUARANTINED un-processable row after exhausting the retry budget`, carrying the row's `ledger`, `tx`, `op_index`, `event_index`, `consecutive_cycles` and the underlying `err`.
- Lag for that source should otherwise be normal: quarantine is what UNSTICKS the cursor, so this fires while lag looks healthy.

Diagnosis (5 min):

```sh
journalctl -u stellarindex-indexer --since -1h | grep -i "QUARANTINED"
curl -s http://indexer:9464/metrics | grep 'stellarindex_projector_events_decoded_total{.*sink_quarantined'
```

Quarantine is always the SECOND-order signal; the `err` names the real defect (`internal/projector/sinkfault.go` classifies it): a decoder regression that panics on a payload shape, a downstream PK/unique constraint rejecting the row for a non-transient reason (e.g. an unexpected duplicate), or malformed on-chain data for a contract version the decoder doesn't handle.

Mitigation:

- [ ] Read the `err` field to learn WHY the row was un-processable. Fix that first; re-driving without a fix re-quarantines the same row.
- [ ] Re-drive: `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write`.
- [ ] Verification: the re-drive succeeds, no new `sink_quarantined` increments for the source, and the completeness verdict returns to `complete=true` after the next `compute-completeness` run.

## stellarindex_projector_row_dropped_permanent

Trips: same shape as quarantine but on `outcome="sink_permanent"` (15m increase, or child born inside the window), `for: 0m`, P3. The sink PERMANENTLY rejected one of a row's outputs: SQLSTATE class 22/23 (`classifySinkFault` returns `dispositionSkip` for any class 22/23 error) or a canonical value-shape rejection before the statement ran. Retrying can never make it land, so the served tier is missing it; the raw event survives in the lake.

**The counter is the rejection, not the shed.** It increments on every cycle that re-reads the row, so a row the per-cycle shed cap is still HOLDING keeps counting. That is deliberate: it makes the fault visible while the projector is only stalling the cursor, about an hour before the no-progress budget (`QuarantineAfterCyclesNoProgress`, about 1 h) lets anything leave.

**Read the rate, not just the event.** One row is a poison value. A burst across many ledgers of one source is the GLOBAL form of the same SQLSTATE, usually a migration whose NOT NULL / CHECK the live rows violate (class 22/23 errors are not always row-local). The projector stalls (`runs_total{outcome="sink_retry"}`, rising lag) while it holds the window, then bleeds one row per cycle. Fix the schema before that budget expires.

Diagnose and fix as for [row_quarantined](#stellarindex_projector_row_quarantined): find the log line and its error, fix the defect (value or schema), then re-drive with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write`. The source's metric is `{source="X",outcome="sink_permanent"}`.

## stellarindex_projector_i128_overflow

Trips: same shape on `outcome="sink_i128_overflow"`, `for: 0m`, **`severity: page` (SEV-1)**. ADR-0003 (`docs/adr/0003-i128-no-truncation.md`, Consequences): any observed `errors.Is(err, canonical.ErrI128Overflow)` in production is a SEV-1; it means an int64 has crept into one of our amount paths. (The sentinel is in the projector's `valueShapeSentinels`; it used to be counted as an ordinary `sink_permanent` drop.) ONE observation is the whole event.

It is NOT a data-quality verdict. i128 is the width every canonical amount is carried at, so this is never a property of the chain data: one of OUR paths narrowed a value to int64. **Treat every amount that path touched as suspect**, not just the dropped row: a narrowed int64 truncates silently and only the value that exceeded the bound raises this. The rows that did NOT overflow are the dangerous ones, because they landed.

Response:

- [ ] Find the conversion (`git log` the decoder and the sink for the source; look for `int64(parts.Lo)`-style narrowing; use `canonical.FromInt128Parts` / `FromUInt128Parts`).
- [ ] Fix it, then re-drive the WHOLE affected range with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write` (or `projected-rebuild` if over about 1M ledgers; see the replay section).

## stellarindex_projector_decode_error_rate_high

Trips: `sum by (source) (rate(stellarindex_projector_events_decoded_total{outcome=~"decode_error|reconstruct_error"}[10m])) > 0.1` for `15m`, P3. Typical MTTR: minutes to confirm the regression; a decoder fix plus re-drive to fully recover.

Impact: every event of the affected class is SILENTLY dropped from the served per-source tier while the cursor advances. Not data loss (raw events are in the lake), but a real, growing gap until the decoder is fixed and the range re-driven.

Why it exists (DATA-6 / NS-2): the projector decodes lake rows with the SAME per-source decoders as live ingest. A decode soft-fail (a returned error or a recovered panic, `processEventSafely`) is counted as `outcome="decode_error"`, the row is skipped, and the cursor advances. That is right for a scattered poison row, but a shipped decoder **regression** breaks a whole class of valid events the same way (the phoenix 5,161-orphaned-swap / I-L4 class), draining them while the cursor sails to tip. The cycle is marked `runs_total{outcome="decode_degraded"}` (no longer `ok`) but still advances. This alert separates a **sustained** rate (regression) from the odd poison row (below threshold) and pages before a reconcile notices months later.

Two outcomes count as failures:

- `decode_error`: the decoder returned an error or panicked.
- `reconstruct_error`: the landing-zone row could not be rebuilt into an event (e.g. missing or unparseable `topic_0_xdr`), so no decoder saw it. A spike here points at the lake writer or a landing-zone schema change, not a decoder.

Symptoms:

- `rate(stellarindex_projector_events_decoded_total{source="X",outcome="decode_error"}[10m])` sustained above 0.1/s.
- `stellarindex_projector_runs_total{source="X",outcome="decode_degraded"}` incrementing.
- Journal: `decode failed; row SKIPPED` (carries `err`), `decoder panicked; row SKIPPED` (carries `stack`), or `malformed landing-zone row — skipped` (ledger/tx/op_index/event_index/contract/err; logged for the first and every 20th failure per cycle); cycle summary shows nonzero `decode_errors` or `reconstruct_errors`.
- Lag may look HEALTHY: the cursor advances normally. That is the trap this alert closes.

Diagnosis (5 min):

```sh
curl -s http://indexer:9464/metrics | grep -E 'stellarindex_projector_events_decoded_total\{.*(decode|reconstruct)_error'
journalctl -u stellarindex-indexer --since -1h \
  | grep -iE "row SKIPPED|malformed landing-zone row|(decode|reconstruct)_errors=[1-9]"
```

One source spiking right after a deploy that changed a decoder is the textbook regression; multiple sources at once points at a shared decode/reconstruct helper. Root causes: a field-mapping regression (the phoenix 7-field swap class), an unhandled upgraded-WASM event shape, or a panic on a specific payload. The projector runs the same decoders as ingest, so check the sibling `stellarindex_ingestion_decode_error` too.

Mitigation:

- [ ] Identify the offending decoder from the panic/error (`internal/sources/<protocol>/decode.go`); if the spike started at a deploy, diff that decoder against the previous release.
- [ ] Fix the regression (or roll back the change). Re-driving without a fix re-drops the same class.
- [ ] Re-drive: `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger>` (add `-write`).
- [ ] Verification: `decode_error` rate ~0, no new `decode_degraded` cycles, completeness verdict back to `complete=true` after the next `compute-completeness` run.

Known false positive: a brief burst during a genuinely novel contract deployment (an event shape no decoder handles yet) can trip it. Still worth a ticket (a real class of events is unprojected) but the fix is a decoder addition, not a rollback.

## stellarindex_projector_decode_error_ratio_high

Same failure, diagnosis, mitigation and false positives as [stellarindex_projector_decode_error_rate_high](#stellarindex_projector_decode_error_rate_high); the difference is the trigger. The rate rule is blind to a quiet source: one emitting a few dozen events an hour (`blend_emitter`, `rozo`, `cctp`) can lose every event and stay far below 0.1/s. This rule trips when, over 1h, failures / (`ok` + failures) (outcomes `decode_error|reconstruct_error`) is `> 0.1` AND at least 10 events failed in that hour, `for: 15m`. The 10-event floor keeps the odd poison row on a quiet source quiet. It usually means a quiet source's decoder broke on a whole event class.

## stellarindex_projector_wedged

Trips: `max by (source) (stellarindex_projector_wedged) > 0` for `5m`, P3 (no customer-facing outage, but the source will NOT self-recover). Gauge set by `internal/projector/projector.go::cycleOneSource`. Typical MTTR: 15 min (raise the budget) to hours (decompress a range).

Impact: the named per-source projection (trades, `phoenix_*`, `blend_*`, ...) stops advancing and the served tier for that source freezes behind the lake. Every OTHER source keeps flowing. No data loss; raw events are durable in the ClickHouse lake / `soroban_events`.

Symptoms:

- `stellarindex_projector_wedged{source="..."}` reads `1` for 5+ minutes.
- `stellarindex_projector_lag_ledgers{source="..."}` has stopped falling (flat).
- `rate(stellarindex_projector_runs_total{source="...",outcome="error"}[15m])` is non-zero and steady: the source cycles, errors, and never advances.

What "wedged" means: on a per-cycle deadline (`PerSourceTimeout`, 60s) the projector halves the source's read window down to the `MinBatchLimit` floor of 25 ledgers, so a dense stretch converges instead of retrying a huge range. A **wedge** is the terminal case: a *floor-sized* (25-ledger) range that **still** takes longer than `PerSourceTimeout` to decode + sink (a dense window over a **compressed** ClickHouse chunk). Nothing is left to halve, so the identical range is retried every cycle forever. `cycleOneSource` flips the gauge to `1` once the source has sat at the floor and failed to advance for `WedgeCycles` (5) consecutive cycles; any advancing cycle clears it.

At the floor the projector escalates that source's per-cycle deadline: `PerSourceTimeout` doubles for each consecutive floor-stall, capped at `MaxCycleBudgetMultiple` (8x, 8 minutes), and resets on the first advancing cycle. The gauge stays raised until a cycle advances, so a wedge that survives the cap still needs the mitigation below. Same failure class as the 2026-07-10 aquarius-rewards stall and the 2026-08-01 aquarius-reserves stall (wedged 3.5h at ledger 63,488,687).

Quick diagnosis (5 min):

```sh
# Which sources are wedged:  max by (source) (stellarindex_projector_wedged) > 0
# Confirm stuck: stellarindex_projector_lag_ledgers{source="<src>"} flat and
#   rate(stellarindex_projector_runs_total{source="<src>",outcome="error"}[15m]) steady.

# The projector logs the held range each cycle; from/to is the wedged 25-ledger
# range. Note whether the chunk it lands in is compressed.
ssh r1 'journalctl -u stellarindex-indexer --since "30 min ago" --no-pager \
  | grep -i "shrinking window\|holding cursor" | grep "<src>" | tail -20'
```

Mitigation is **manual and operator-owned**; the projector does not auto-decompress (it could starve the host or thrash merges). Pick the smallest safe lever:

- [ ] **Decompress the offending range** (preferred for a single cold/compressed chunk): recompress the partition to a faster/idle-decompressible codec, or materialise the range into an uncompressed staging part, so the 25-ledger window finishes inside the budget.
- [ ] **Raise the per-cycle budget** (`PerSourceTimeout`) if the range is legitimately heavy and decompression is not practical now. Code/config change plus indexer redeploy; size it so the floor window finishes with headroom and watch that other sources' cycle latency stays healthy.
- [ ] **Verification**: `stellarindex_projector_wedged{source="..."}` returns to `0` and lag resumes falling. A clean advance clears the gauge on the next cycle.

Known false positives: a single very slow cycle does NOT wedge (needs 5 consecutive floor-stalls). A source blowing the deadline while the window is still *above* the floor is adapting normally and is not flagged.

Once the density/compression cause is fixed, re-drive with `projector-replay` (see [replay](#stellarindex_projector_replay_stalled)). Lag is the softer, self-recovering signal ([lag_high](#stellarindex_projector_lag_high)); a wedge is where lag stops falling for good.

## stellarindex_spectra_unlisted_infrastructure

Trips: a Spectra registry `*_change` event named a factory, router, order engine or token WASM that is not in the hand-kept audited set. The gate is incomplete until it is listed; events from the new contract are not decoded meanwhile.

Response:

- [ ] Identify the id from the indexer log line for the event (`kind` label says which class).
- [ ] A new id needs a code change in `internal/sources/spectra/events.go` plus a WASM audit (string-check the bytes, record it in `internal/wasmaudit/audited_wasm.json`). It cannot be admitted at runtime.
- [ ] After it ships, re-drive with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source spectra -from <ledger of the change> -write`.

## Related

- `data-freshness.md#stellarindex_completeness_incomplete`: ADR-0033 completeness verdict that sustained skips eventually flip to `complete=false`.
- `ingestion.md#stellarindex_ingestion_source_stopped`: per-source ingest cadence alerts (live-ingest writes, not projection).
- `docs/architecture/ingest-pipeline.md`: `projector-replay` vs `projected-rebuild`.
- ADR-0048 D3: the `projected-rebuild` bulk catch-up path.
- `internal/projector/` (`projector.go`, `sinkfault.go`, `registry.go`), `internal/ops/chops/projected_rebuild.go`.
