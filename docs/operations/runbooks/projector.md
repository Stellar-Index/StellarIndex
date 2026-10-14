---
title: Runbook — projector
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — projector alerts

Rules: `configs/prometheus/rules.r1/projector.yml` (same as `deploy/monitoring/rules/projector.yml`), `component: projector`. All are `severity: ticket` (P3) except `stellarindex_projector_i128_overflow` (`page`, SEV-1). Code: `internal/projector/`; design: ADR-0032, ADR-0034 #10 / ADR-0041.

The dispatcher's per-source sink is still primary for most sources, so lag/error alerts are mostly visibility-only. **Exception: sole-writer sources** (`SoleWriter` in `internal/pipeline/source_spec.go`: the sep41 pair and rozo). The projector is their only writer, so trouble there is real data lag and "disable the projector" is NOT a safe lever.

The projector tails the ClickHouse lake's `contract_events` by default (`storage.clickhouse_projector_source`, requires `clickhouse_live_sink`); Postgres `soroban_events` is the legacy fallback. Both share the `ingestion_cursors` rows and lag gauge. Which one this host is on:

```sh
journalctl -u stellarindex-indexer --no-pager | \
  grep -F 'projector reading from ClickHouse lake' | tail -1
# present = CH lake (default);  absent = legacy soroban_events read
```

**Skip semantics (row and decode alerts).** A failing row is skipped after its retry budget (one row per cycle; `sink_permanent` holds the cursor ~1 h first) and the cursor advances, because holding would wedge a sole-writer source on a deterministic failure (COR-11). The raw event survives in the lake; re-drive with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write` AFTER fixing the defect (otherwise the same rows are re-dropped). Sustained skips show as `complete=false` on the completeness verdict (`data-freshness.md#stellarindex_completeness_incomplete`), back to `true` at the next `compute-completeness` run.

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
- [`stellarindex_projector_replay_stalled`](#stellarindex_projector_replay_stalled): an operator replay stopped advancing; also the full `projector-replay` reference.
- [`stellarindex_projector_row_quarantined`](#stellarindex_projector_row_quarantined): the poison-row escape hatch skipped one row.
- [`stellarindex_projector_row_dropped_permanent`](#stellarindex_projector_row_dropped_permanent): the sink permanently rejected a row's values.
- [`stellarindex_projector_i128_overflow`](#stellarindex_projector_i128_overflow): SEV-1, an int64 reached an amount path.
- [`stellarindex_projector_decode_error_rate_high`](#stellarindex_projector_decode_error_rate_high) / [`..._ratio_high`](#stellarindex_projector_decode_error_ratio_high): sustained decode failures.
- [`stellarindex_projector_wedged`](#stellarindex_projector_wedged): cursor stuck at the 25-ledger window floor.

## stellarindex_projector_lag_high

Trips: `max by (source) (stellarindex_projector_lag_ledgers) > 256` for `10m`, `unless max by (source) (stellarindex_projector_replay_window_active) == 1`. MTTR 30 min. Per-source tables diverge from the event store; for sole-writer sources that is served-data lag. Also `stellarindex_projector_cycle_duration_seconds_bucket{source}` p99 > 30 s (`PerSourceTimeout` is 60 s).

Check: run the shared cursor and tip queries, then `ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 | grep "projector cycle" | grep <source>'`. A cursor moving slower than the tip is honest catch-up after an outage; let it run unless lag exceeds a few hours.

**Held at a lake hole.** In ClickHouse mode the scan is clamped to the lake's contiguous-completeness watermark, so a missing lake ledger stops the source there on purpose (`outcome="watermark_held"`, not `idle`; lag grows):

```promql
sum by (source) (increase(stellarindex_projector_runs_total{outcome="watermark_held"}[15m]))
```

Non-zero means the lake has a hole: the log line `held at the lake's contiguous watermark` names the ledger, and every source stops at the same one. Heal the lake with the `ch-live-catchup` timer ([ch-live-sink-drops](clickhouse.md#stellarindex_ingestion_ch_live_sink_drops)); the projector resumes alone. Do not rewind the cursor.

Likely causes:

- Postgres connection saturation, a PK failure on a malformed event, or a decoder panic (log lines `component=projector`; `outcome="error"` rate).
- A wedged source: [stellarindex_projector_wedged](#stellarindex_projector_wedged) FIRST.
- Cursor restarted from a lower watermark after an outage.
- Cold start (cursor 0 catching up from genesis) or a landing-zone backfill; let it drain.

Fix:

1. Confirm the dispatcher's per-source sink is still writing (not true for sep41/rozo).
2. Inspect `component=projector` logs for the failing source (`sum by (source, outcome) (increase(stellarindex_projector_runs_total[6h]))` and `..._events_decoded_total[6h]` split decode from sink failures).
3. Catch-up after a real outage: rewind the cursor and let the running projector tail forward. `-config`, `-source`, `-from` are ALL required; `-source` is the projector's own name (`internal/projector/registry.go`), as `find-data-gaps` prints in its backfill plan (not the hyphenated `source=` header):

   ```sh
   # Dry run is the DEFAULT: prints the intended rewind, writes nothing.
   stellarindex-ops projector-replay -config /etc/stellarindex.toml \
     -source <name> -from <ledger>

   # -write is REQUIRED to actually rewind the cursor.
   stellarindex-ops projector-replay -config /etc/stellarindex.toml \
     -source <name> -from <ledger> -write
   ```

   Full procedure: [replay section](#stellarindex_projector_replay_stalled).
4. Last resort: `[ingestion.projector] enabled = false` in `/etc/stellarindex.toml` + `systemctl restart stellarindex-indexer.service`. For sep41/rozo it STOPS the only writer.
5. Verify lag drops below 256 within 30 min.

An operator `projector-replay` rewind does not reach this alert (`stellarindex_projector_replay_window_active{source}=1` excuses it); a pending `projected-rebuild` window does NOT. A firing alert therefore means real lag; a replay that stopped climbing fires [replay_stalled](#stellarindex_projector_replay_stalled).

## stellarindex_projector_error_rate_high

Trips: `sum by (source) (rate(stellarindex_projector_runs_total{outcome="error"}[15m])) > 0.05` for `15m`. Same impact and body as [lag_high](#stellarindex_projector_lag_high) but on failing cycles; the cursor may not be advancing. Work lag_high's steps 2 and 5; if one source is pinned at the window floor see [wedged](#stellarindex_projector_wedged).

## stellarindex_projector_replay_stalled

Trips: replay window flag is 1 AND lag > 256 AND lag is at its own 15m maximum, `for: 5m` (~20 min total). An operator `projector-replay` is inside its recorded rewind window but stopped advancing (or the tip outruns it); the deficit it was started to repair is still open.

Triage: [wedged](#stellarindex_projector_wedged), the `runs_total{outcome="error"|"sink_retry"}` rates for the source, and the [decompress-first pre-flight](#pre-flight-decompress-the-replay-window-first). If started with a stalled `-wait`, the command itself exits non-zero.

### The tool

`stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write` (fail-closed: no `-write` = dry run). Use it for a source with stale or missing rows over a known ledger range. The rewind is at most 5 s of SQL but the command then BLOCKS until the projector re-walked the range (~1 min per 100k ledgers, bounded by `-wait-timeout`, default 30 min) and re-materializes the seven `prices_*` aggregates (a further 30 min allowed): run it under `tmux`/`screen`. Load: the projector re-walks the range and the refresh runs `refresh_continuous_aggregate` over padded windows (up to ~93 days for `prices_1mo`), which can contend with each view's policy (55P03, retried within a bound).

Per-source writers upsert guarded by `derive_generation <= EXCLUDED.derive_generation` and the projector writes at generation 0, so a replay cannot correct a row a re-derive already stamped higher; use `projected-rebuild -write` for that.

Bigger than ~1M ledgers (replay ceiling ~720k ledgers/hour: 5 s tick, 60 s per-cycle timeout), use `stellarindex-ops projected-rebuild` (ADR-0048 D3; parallel workers, 10-20x throughput; see [ingest-pipeline.md](../../architecture/ingest-pipeline.md#re-deriving-from-the-lake)). The two tools must never run concurrently over overlapping history for one source. `projected-rebuild` exits non-zero if any window was held or any trade permanently dropped. A migration follow-up written as `projector-replay ... -from N` goes through `projected-rebuild` when `N` is over ~1M ledgers behind the tip.

`projected-rebuild -resume` (default) skips any window with a checkpoint (`ingestion_cursors`, `source = 'projected-rebuild'`, `sub_source = 'X:<from>-<to>'`) without checking the table still holds the rows. A migration that empties a projected table deletes that source's checkpoints in the same file (`lint-migrations.sh` pass 10). If a table is empty over a range its checkpoints claim, delete that source's `projected-rebuild` rows from `ingestion_cursors` by hand, then re-run.

### Check

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

Source names (registry, `internal/projector/registry.go`, the authority): `aquarius`, `soroswap`, `phoenix`, `comet`, `blend`, `cctp`, `rozo`, `defindex`, `sep41_transfers`, `sep41_supply`, `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`. Soroswap skim rows replay under `soroswap`. `sep41_*` and `blend_*` are underscored. An unknown `-source`, or one that never ran (no cursor row yet), fails non-zero. A `-from` at or above the current cursor exits 0 with "nothing to rewind": that range has not been projected yet.

### After the rewind: the command waits, then refreshes the price CAGGs

Replayed trades land in a historical range that no aggregate refresh policy revisits, so they stay invisible to `/v1/ohlc`, `/v1/chart`, `/v1/vwap` and `/v1/history/since-inception` until refreshed. With `-write` the command: (1) records the dirty window and rewinds the cursor; (2) polls the cursor every 5 s until it is back at its pre-rewind ledger; (3) re-materializes `prices_1m`, `prices_15m`, `prices_1h`, `prices_4h`, `prices_1d`, `prices_1w`, `prices_1mo` and prints `price CAGGs re-materialized over the replayed range [from,to]`.

| Flag | Default | Effect |
| ---- | ------- | ------ |
| `-refresh-caggs` | `true` | `false` skips steps 2-3; the range stays unmaterialised until a manual refresh. |
| `-wait` | `true` | `false` returns right after the rewind; the refresh is yours. |
| `-wait-timeout` | `30m` | Budget for step 2; about 1 min per 100k ledgers plus head-room. |

Both opt-outs exit 0. Non-zero after `projector cursor rewound` means the rewind IS durable and the refresh did NOT complete: cursor short of its pre-rewind ledger after `-wait-timeout` (projector slow or wedged; no view refreshed), or `post-replay CAGG refresh ... failed` (the error names the views not materialized). Do NOT re-run with the same `-from` (it re-walks from the partially advanced cursor and never refreshes the gap). Once the projector has caught up:

```sh
stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -refresh-only -refresh-to <ledger>
stellarindex-ops trades-cagg-refresh -config PATH -from <ledger> -to <ledger> -write
```

The first refuses until the projector re-walked up to `-refresh-to`; the second suits large ranges. The refresh set is `timescale.TradesCAGGs` (incl. `twap_1h` / `twap_1d`) plus `timescale.OracleCAGGs` if oracle rows were written. While `prices_1m`'s retention policy (migration 0156) is armed the twap refresh is refused and the command names them; disarm as that migration states and re-run.

Verify: `ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 100 -f | grep projector'`; `stellarindex_projector_lag_ledgers{source="<name>"}` falls to 0 and the command's last line is the re-materialized message with exit 0. After `-wait=false` / `-refresh-caggs=false` / a non-zero exit, the projection is repaired but served OHLC/VWAP over the range is not.

### Alert behaviour while a replay runs

The rewind is recorded in `projection_dirty_windows` (migration 0125) before the cursor moves, and the projector publishes `stellarindex_projector_replay_window_active{source}` = 1 every 30 s while the cursor is inside it.

- `lag_high` does not ticket for that source meanwhile; `replay_stalled` does (after ~20 min) if lag stays over 256 and flat or rising for 15 min. Under 256 neither speaks.
- The flag clears when the cursor regains its pre-rewind position; later lag tickets normally.
- A `projected-rebuild` window never raises the flag (gated on the window's `reason`), even with `-allow-live-overlap`. If a replay is recorded while a rebuild window is pending the row widens to the higher `to_ledger`; clear the pending window with `compute-completeness` first for a tighter bound.
- Replaying a range before the source's Soroban genesis is a no-op.

### Pre-flight: decompress the replay window FIRST

Replays through compressed Timescale history livelock: upserts into compressed `trades` and `aquarius_*` chunks blow write deadlines, the projector holds the cursor and retry-storms (100k+ abandoned inserts/hour). The nightly 22:45Z policies recompress, so a chunk decompressed yesterday re-blocks today, and any table newly given a policy joins the trap.

Before ANY replay whose window overlaps compressed chunks, decompress every compressed chunk of EVERY table the source writes (`trades` plus its per-source hypertables) across the window:

```sql
SELECT format('SELECT decompress_chunk(%L);',
              c.chunk_schema||'.'||c.chunk_name)
FROM timescaledb_information.chunks c
WHERE c.hypertable_name IN ('trades', '<source tables...>')
  AND c.is_compressed
  AND c.range_end > '<window start ts>';
```

Run the emitted statements (large chunks under `run-heavy-job.sh`), then start the replay; the nightly policy recompresses. An already-wedged replay shows `Insert*: context deadline exceeded` storms with a frozen cursor; decompress the blocking chunk and the sink-side adaptive shrink converges the window.

## stellarindex_projector_row_quarantined

Trips: `increase(stellarindex_projector_events_decoded_total{outcome="sink_quarantined"}[15m]) > 0`, or the child born non-zero inside 15m, `for: 0m`. The ticket clears after 15m on its own but the row is still skipped: work from the journal. A scrape gap at t-15m can ticket once with no new quarantine; otherwise investigate every one (repeats signal a systemic decoder/schema regression).

Impact: ONE event is skipped from the served tier; the raw event is in the lake but the gap is silent until re-driven. `quarantineCandidate` (`internal/projector/projector.go`) gives up on at most one held row per cycle after its retry budget (COR-11) so a poison row cannot wedge the source; this alert is the only signal.

Check:

```sh
journalctl -u stellarindex-indexer --since -1h | grep -i "QUARANTINED"
curl -s http://indexer:9464/metrics | grep 'stellarindex_projector_events_decoded_total{.*sink_quarantined'
```

The log line `projector: QUARANTINED un-processable row after exhausting the retry budget` carries `ledger`, `tx`, `op_index`, `event_index`, `consecutive_cycles` and `err`; lag otherwise looks healthy. The `err` names the real defect (`internal/projector/sinkfault.go`): a decoder panic on a payload shape, a PK/unique constraint rejecting the row non-transiently, or malformed data for an unhandled contract version.

Fix:

1. Fix the defect named by `err`; re-driving without a fix re-quarantines.
2. Re-drive: `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write`.
3. Verify no new `sink_quarantined` increments and `complete=true` after the next `compute-completeness`.

## stellarindex_projector_row_dropped_permanent

Trips like quarantine on `outcome="sink_permanent"`, `for: 0m`, P3. The sink PERMANENTLY rejected one of a row's outputs (SQLSTATE class 22/23, `classifySinkFault` -> `dispositionSkip`, or a canonical value-shape rejection); the served tier is missing it, the raw event survives in the lake. The counter increments on every cycle that re-reads the row, so it is visible while the cursor is held (~1 h before `QuarantineAfterCyclesNoProgress`).

Read the rate: one row is a poison value; a burst across many ledgers of one source is the global form, usually a migration whose NOT NULL / CHECK live rows violate (`runs_total{outcome="sink_retry"}` and lag rise). Fix the schema before the budget expires.

Diagnose as for [row_quarantined](#stellarindex_projector_row_quarantined) (metric `{source="X",outcome="sink_permanent"}`), fix the value or schema, then re-drive with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write`.

## stellarindex_projector_i128_overflow

Trips on `outcome="sink_i128_overflow"`, `for: 0m`, **`severity: page` (SEV-1)**. Per ADR-0003 (`docs/adr/0003-i128-no-truncation.md`) any `errors.Is(err, canonical.ErrI128Overflow)` in production means an int64 crept into one of OUR amount paths; ONE observation is the whole event. It is never a property of chain data. Treat every amount that path touched as suspect: narrowed int64 truncates silently, and the rows that did NOT overflow are the dangerous ones because they landed.

Response:

1. Find the conversion (`git log` the decoder and sink for the source; look for `int64(parts.Lo)`-style narrowing; use `canonical.FromInt128Parts` / `FromUInt128Parts`).
2. Fix it, then re-drive the WHOLE affected range with `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger> -write` (or `projected-rebuild` over ~1M ledgers).

## stellarindex_projector_decode_error_rate_high

Trips: `sum by (source) (rate(stellarindex_projector_events_decoded_total{outcome=~"decode_error|reconstruct_error"}[10m])) > 0.1` for `15m`, P3. Every event of the affected class is silently dropped from the served tier while the cursor advances (raw events stay in the lake). The projector uses the SAME per-source decoders as live ingest; a decoder regression breaks a whole class of valid events (the phoenix orphaned-swap class). The cycle is marked `runs_total{outcome="decode_degraded"}` yet still advances, so lag looks HEALTHY.

- `decode_error`: the decoder returned an error or panicked.
- `reconstruct_error`: the landing-zone row could not be rebuilt (missing or unparseable `topic_0_xdr`); points at the lake writer or a landing-zone schema change, not a decoder.

Check:

```sh
curl -s http://indexer:9464/metrics | grep -E 'stellarindex_projector_events_decoded_total\{.*(decode|reconstruct)_error'
journalctl -u stellarindex-indexer --since -1h \
  | grep -iE "row SKIPPED|malformed landing-zone row|(decode|reconstruct)_errors=[1-9]"
```

Likely causes: one source right after a decoder deploy (textbook regression); several sources at once (a shared decode/reconstruct helper); a field-mapping regression, an upgraded-WASM event shape, or a payload panic. Compare with `stellarindex_ingestion_decode_error`.

Fix:

1. Identify the decoder from the panic/error (`internal/sources/<protocol>/decode.go`); diff it against the previous release if the spike began at a deploy.
2. Fix or roll back; re-driving without a fix re-drops the same class.
3. Re-drive: `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <X> -from <ledger>` (add `-write`).
4. Verify `decode_error` ~0, no new `decode_degraded` cycles, `complete=true` after the next `compute-completeness`.

A burst on a genuinely novel contract deployment still deserves a ticket; the fix is a decoder addition, not a rollback.

## stellarindex_projector_decode_error_ratio_high

Same failure, diagnosis and fix as [decode_error_rate_high](#stellarindex_projector_decode_error_rate_high), for quiet sources (`blend_emitter`, `rozo`, `cctp`) that can lose every event and stay under 0.1/s. Trips when, over 1h, failures / (`ok` + failures) is `> 0.1` AND at least 10 events failed, `for: 15m`. Usually a quiet source's decoder broke on a whole event class.

## stellarindex_projector_wedged

Trips: `max by (source) (stellarindex_projector_wedged) > 0` for `5m`, P3. The named source stops advancing and will NOT self-recover; other sources flow. No data loss. MTTR 15 min (raise the budget) to hours (decompress a range). Gauge set in `internal/projector/projector.go::cycleOneSource`.

On a per-cycle deadline (`PerSourceTimeout`, 60 s) the projector halves the read window down to the `MinBatchLimit` floor of 25 ledgers. A wedge is a floor-sized range that STILL exceeds the timeout (a dense window over a COMPRESSED ClickHouse chunk), retried forever. The gauge flips to `1` after `WedgeCycles` (5) consecutive floor-stalls and clears on any advancing cycle. At the floor the per-cycle deadline doubles per consecutive stall up to `MaxCycleBudgetMultiple` (8x = 8 min); a wedge surviving the cap needs the fix below.

Check:

```sh
# Which sources are wedged:  max by (source) (stellarindex_projector_wedged) > 0
# Confirm stuck: stellarindex_projector_lag_ledgers{source="<src>"} flat and
#   rate(stellarindex_projector_runs_total{source="<src>",outcome="error"}[15m]) steady.

# The projector logs the held range each cycle; from/to is the wedged 25-ledger
# range. Note whether the chunk it lands in is compressed.
ssh r1 'journalctl -u stellarindex-indexer --since "30 min ago" --no-pager \
  | grep -i "shrinking window\|holding cursor" | grep "<src>" | tail -20'
```

Fix (manual and operator-owned; the projector never auto-decompresses):

1. Preferred for a single cold chunk: decompress the offending range (recompress to a faster codec, or materialise it into an uncompressed staging part) so the 25-ledger window finishes inside the budget.
2. Otherwise raise `PerSourceTimeout` (code/config plus indexer redeploy), sized so the floor window finishes with headroom, watching other sources' cycle latency.
3. Verify `stellarindex_projector_wedged{source="..."}` returns to `0` and lag falls; then re-drive with `projector-replay` ([replay](#stellarindex_projector_replay_stalled)).

A single slow cycle does not wedge (needs 5 consecutive floor-stalls); a window still above the floor is adapting normally.

## stellarindex_spectra_unlisted_infrastructure

A Spectra registry `*_change` event named a factory, router, order engine or token WASM outside the hand-kept audited set; events from the new contract are not decoded until it is listed.

1. Identify the id from the indexer log line (`kind` label says the class).
2. Code change in `internal/sources/spectra/events.go` plus a WASM audit (string-check the bytes, record in `internal/wasmaudit/audited_wasm.json`); it cannot be admitted at runtime.
3. After it ships, re-drive: `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source spectra -from <ledger of the change> -write`.

## Preview what a re-derive would change

`ch-reproject` re-derives a range from the lake and compares per-ledger counts with the served tables per source. It writes nothing and exits non-zero on any divergence. Soroswap is re-derived without pair seeding, so a range with pairs created before `-from` diverges by construction.

```sh
stellarindex-ops ch-reproject -config /etc/stellarindex.toml -from <ledger> -to <ledger>
```

## Related

- `data-freshness.md#stellarindex_completeness_incomplete`, `ingestion.md#stellarindex_ingestion_source_stopped`.
- `docs/architecture/ingest-pipeline.md` (`projector-replay` vs `projected-rebuild`), ADR-0048 D3.
- `internal/projector/` (`projector.go`, `sinkfault.go`, `registry.go`), `internal/ops/chops/projected_rebuild.go`.
