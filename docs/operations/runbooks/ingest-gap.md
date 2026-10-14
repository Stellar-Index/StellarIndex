---
title: Runbook — ingest-gap
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — ingest-gap alerts

Ledger-gap detector alerts. Rules: `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`; twin `deploy/monitoring/rules/ingestion.yml`).

## At a glance

- [`stellarindex_ingest_gap_detected`](#stellarindex_ingest_gap_detected)
- [`stellarindex_ingest_gap_detector_silent`](#stellarindex_ingest_gap_detector_silent)
- [`sdex-gap-detected`](#sdex-gap-detected)

## stellarindex_ingest_gap_detected

P1 page: `stellarindex_ingest_gap_max_size_ledgers{source} > 1000` for 15 min. MTTR 30 min - 4 h. A contiguous block of ingest is missing, so per-decoder coverage is incomplete across the window and price-history queries return holes. The status page's cursor-derived density may still read 100% (it measures process state; this alert measures data state).

Check:

```sh
ssh root@<region-host>
stellarindex-ops find-data-gaps --config /etc/stellarindex.toml --output text
ssh root@<region-host> 'sudo -iu postgres psql -d stellarindex -c "SELECT MAX(ledger_close_time), MAX(ledger) FROM soroban_events;"'
```

`find-data-gaps` prints each `[from, to]` range plus a ready-to-paste `backfill` command. It is ledger-scoped and cannot see CEX/oracle sources: for those run `scripts/ops/find-external-gaps.sh [source...]` on the host (needs `STELLARINDEX_POSTGRES_DSN`; `MIN_GAP_DAYS` defaults to 2; reports day-level holes in `kraken coinbase bitstamp binance`; a human judges each, since a venue may truly not have traded). `MAX(ledger_close_time)` fresh (~30 s) means the writer is healthy and the gap historic; stale means still wedged and growing.

Likely causes:

- One large gap (> 50K ledgers): ingest halt (Redis MISCONF, Postgres back-pressure, AsyncSink wedge); cross-check `stellarindex_redis_writes_blocked`, `stellarindex_postgres_connections_high`.
- Many small gaps (~100-500 ledgers): MinIO blip, ledgerstream reconnect, partial-batch sink failure.
- One small gap at the trailing edge: active backfill or brief Postgres pause; re-check in 5 min.
- First boot after a deploy with a historic gap preserved; a genuinely empty window below `min-gap-size=1000`.

Fix:

1. Healthy writer, historic gap: one invocation per gap (~92K ledgers takes 15-30 min on r1); confirm via `find-data-gaps` or the gauge decaying.

   ```sh
   stellarindex-ops backfill -write --config /etc/stellarindex.toml \
     --from <gap.start> --to <gap.end> --source soroban-events
   ```

2. Wedged writer, growing gap: pause heavy walks (`pkill -INT -f 'stellarindex-ops backfill'`; `verify-archive-tier-a.service`) per `docs/operations/backfill-procedure.md`, check Redis (`redis-cli info persistence`, `rdb_last_bgsave_status: ok`?) and Postgres (`SELECT count(*) FROM pg_stat_activity;`), then `systemctl restart stellarindex-indexer` and watch `/v1/diagnostics/cursors`. Backfill afterwards.
3. After a projector replay, lake outage or SDEX history backfill, trades older than the live sweepers' 30-minute lookback lack derived tags. Each command is a dry run until `-write`, first-wins, safe to re-run, uncompressed chunks only. Run `tag-routed-via` (Soroswap `trades.routed_via`) only after the router record (`ch-rebuild -contract-calls`) is complete:

   ```sh
   stellarindex-ops tag-signer -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
   stellarindex-ops tag-tx-index -config /etc/stellarindex.toml -from <RFC3339> -to <RFC3339> -write
   stellarindex-ops tag-routed-via -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
   ```

Escalate: a gap that keeps growing after the indexer restart.

### Scaling landmine: detector targets with no leading-`ledger` index

Both detector queries predicate only on `ledger BETWEEN $1 AND $2`. Per-source hypertables partition on `ledger_close_time`, so without a btree whose first column is `ledger` (or a `(source, ledger)` btree matched by the target's `WhereFilter`) the query scans the whole table and its cost grows with table lifetime size. `soroban_events` already crossed the line (13-min PG timeout on the LAG scan; its count moved to the `ledger_ingest_log` census). Adding an index is a deliberate migration (`CREATE INDEX CONCURRENTLY`), never an incident action.

Targets with NO ledger-prefixed btree: `cctp_events`, `rozo_events`, `comet_liquidity`, `blend_emitter_events`, `blend_emissions`, `blend_admin`, `soroswap_skim_events`, `soroswap_liquidity`, `soroswap_router_swaps`, `defindex_flows`, `defindex_fees`, `defindex_admin_events`, `phoenix_liquidity`, `phoenix_initialize`, `phoenix_admin_events`, `phoenix_stake_events`, `aquarius_liquidity`, `aquarius_reserves` (dense; the next likely offender), `aquarius_reserves_sync`, `aquarius_protocol_fee`, `aquarius_kill_switches`, `aquarius_rewards_events`, `aquarius_admin`, `credit_positions`, `credit_statements`, `credit_settlements`, `credit_events`. Covered: `trades` and `oracle_updates` by `(source, ledger DESC)`; `sep41_transfers`, `sep41_supply_events`, `blend_auctions`, `blend_positions`, `blend_backstop_events`, `sdex_offer_events` by a ledger-first index or PK.

If a target shows `elapsed_s` in the hundreds in `gap-detector: scan failed` lines, open a migration for a `(ledger DESC)` index, or route its count through `GapDetectorTarget.DistinctLedgerCountSQL`.

## stellarindex_ingest_gap_detector_silent

P2 ticket, MTTR 15 min - 1 h. The detector goroutine is wedged for a target, so the gap gauge is stale and `ingest_gap_detected` cannot fire. Fires on any of: `(time() - stellarindex_ingest_gap_detector_last_success_unix) > 8h` per source/table; the detector metric absent for 15 min (aggregator down); `runs_total{outcome="error"}` present now and 8h ago with no last-success stamp in 8h (a target that never succeeded this process life). `stellarindex_ingest_gap_detector_runs_total{outcome="error"}` climbing with `gap-detector: scan failed` lines in the aggregator log (large `elapsed_s` = timeout) is the usual picture; may coincide with `stellarindex_aggregator_silent` or `stellarindex_postgres_exporter_down`.

Check:

```sh
ssh root@<region-host> 'systemctl status stellarindex-aggregator | head'
ssh root@<region-host> 'sudo -iu postgres pg_isready'
ssh root@<region-host> "sudo -iu postgres psql -d stellarindex -c 'SELECT count(*), state FROM pg_stat_activity GROUP BY state;'"
```

Likely causes:

- Aggregator inactive or crash-looping (`journalctl -u stellarindex-aggregator -n 200`).
- Postgres unreachable: the per-target scan timeout (15 min Go-side / 13 min SQL `statement_timeout`) fires every cycle.
- Pool saturated (`active` near `max_connections`), typically concurrent fill walks; see `docs/operations/backfill-procedure.md`.

Fix:

1. Aggregator down:

   ```sh
   ssh root@<region-host> 'systemctl restart stellarindex-aggregator'
   ssh root@<region-host> 'journalctl -u stellarindex-aggregator -f'
   ```

   Light targets scan within seconds and clear within ~15 min. Heavy `sdex`/`soroban-events` targets (6h cadence) are scanned on restart only if 6h elapsed since their `gap-detector-scan` cursor; otherwise the previous stamp is re-emitted and the alert clears at the next due scan. To scan a heavy target NOW (also widens it to the `GapDetectorFirstScanCap` window) delete its cursor before restarting: `DELETE FROM ingestion_cursors WHERE source = 'gap-detector-scan' AND sub_source = 'soroban-events/soroban_events'`.
2. Postgres degraded: wait for `pg_isready`; the detector recovers on its own cycle.
3. Pool saturation: stop the running fill (a manual invocation, not a unit: find its PID and `kill -INT` it per "Stop a running fill walk" in `backfill-procedure.md`), wait for connections to drop, resume at `-parallel 4` instead of `-parallel 12`.

Staleness never reads higher after a restart than before, and heavy targets sawtooth to ~6.2h, under the 8h threshold; only a genuinely missed cycle fires. Rule tests: `deploy/monitoring/rule-tests/ingestion_test.yml`.

## sdex-gap-detected

P1 page, act within 30 min. This is `stellarindex_ingest_gap_detected` firing with `source="sdex"` (`max by (source) (stellarindex_ingest_gap_max_size_ledgers) > 1000`, 15 min); there is no sdex-specific rule. SDEX does NOT flow through `soroban_events`: rows land in `trades` with `source = 'sdex'`. Gap size gauge refreshes only on the sdex target's 6h `ScanCadence`.

Limits: the detector scans only a trailing window (`GapDetectorSafetyLookback` = 200,000 ledgers; `GapDetectorFirstScanCap` = 2,000,000 on first scan; `internal/storage/timescale/gap_detector.go`), and the sdex target has `MinGapSizeOverride` = 1,000,000 ledgers (`per_source_gaps.go`), so only large recent holes page. Deep-history assurance is the ADR-0033 completeness verdict; full-range diagnosis is `find-data-gaps -from/-to`. There is no `sdex_offer_events` target (no writer exists), so an offer-events halt will not page.

Check:

```sh
ssh root@136.243.90.96
curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap.*sdex'
stellarindex-ops find-data-gaps -config /etc/stellarindex.toml -source sdex
# Healthy writes are silent in the journal; use liveness metrics:
curl -s localhost:9464/metrics | grep 'stellarindex_source_last_insert_unix{source="sdex"}'
curl -s localhost:9464/metrics | grep 'stellarindex_source_events_total{source="sdex"'
curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
```

`last_insert_unix` climbing plus `events_total` rising means the writer is healthy (gap is historic); both frozen means wedged or halted.

Likely causes:

- Active writer halt: investigate the cascade root cause first (Redis MISCONF, Postgres pool exhaustion, disk); restarting the indexer without it deadlocks again within minutes.
- Chain-wide network outage: the Soroban target shows a similar gap; only SDEX short means ingest-side.

Fix (idempotent via the `trades` PK `(source, ledger, tx_hash, op_index, ts)`; there is no `-parallel`, `-config` is required, dry-run first):

1. Dry run, on r1 under the memory-capped wrapper:

   ```sh
   /usr/local/sbin/run-heavy-job.sh sdex-backfill \
     stellarindex-ops backfill \
       -config /etc/stellarindex.toml \
       -from $GAP_START -to $GAP_END \
       -source sdex \
       -dry-run
   ```

2. Apply:

   ```sh
   /usr/local/sbin/run-heavy-job.sh sdex-backfill \
     stellarindex-ops backfill -write \
       -config /etc/stellarindex.toml \
       -from $GAP_START -to $GAP_END \
       -source sdex \
       -resume
   ```

3. Verify the gauge reads 0 within one 6h cycle (a failed cycle retains the last value):

   ```sh
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
   ```

No per-source `*-backfill` subcommand exists (ADR-0032 Phase 5): Soroban sources catch up with `projector-replay -source <name> -from <ledger>`; SDEX has no landing zone, so `backfill -source sdex` is the tool.

## Related

- [projector.md#stellarindex_projector_replay_stalled](projector.md#stellarindex_projector_replay_stalled): per-source projection repair by cursor rewind.
- `docs/operations/backfill-procedure.md`, `aggregator.md#stellarindex_aggregator_silent`.
- ADR-0030 (per-source coverage invariant), ADR-0033 (completeness verdict).
