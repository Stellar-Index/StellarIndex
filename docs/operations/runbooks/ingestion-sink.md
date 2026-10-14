---
title: Runbook — ingestion persistence (insert errors, backpressure, duplicate flood, undrained rows)
last_verified: 2026-10-06
status: living
severity: ticket (P2 for insert_errors, persist_drop, duplicate_flood; P3 for backpressure)
---

# Runbook — ingestion persistence alerts

Alerts on the path from decoded events to the Postgres served tier (durability semantics: [ADR-0041](../../adr/0041-ingest-durability-semantics.md)). Rules: `configs/prometheus/rules.r1/ingestion.yml` (twin `deploy/monitoring/rules/ingestion.yml`). Indexer metrics port on r1 is 9464 (the API serves its own on :3000).

Infrastructure faults on a write are NOT lost: the sink retries with backpressure and the cursor stalls ([trade_insert_backpressure](#stellarindex_ingestion_trade_insert_backpressure)). A firing of [insert_errors](#stellarindex_ingestion_insert_errors) therefore means genuine loss.

## At a glance

| Alert | Severity | Meaning |
| ----- | -------- | ------- |
| [`stellarindex_ingestion_insert_errors`](#stellarindex_ingestion_insert_errors) (+ `stellarindex_ingestion_persist_drop`, `stellarindex_ingestion_trustline_observation_drop`) | P2 ticket; `for: 5m` (insert_errors), `for: 0m` (the two drop tripwires) | events failing to persist: genuine loss |
| [`stellarindex_ingestion_trade_insert_backpressure`](#stellarindex_ingestion_trade_insert_backpressure) | P3 ticket, 10 min | trade sink retrying: Postgres unreachable, ingest intentionally stalled |
| [`stellarindex_ingestion_duplicate_flood`](#stellarindex_ingestion_duplicate_flood) | P2 ticket, `for: 10m` | trades stop landing new rows though cursor and decoder look healthy |
| [`stellarindex_ingestion_sink_undrained_rows`](#stellarindex_ingestion_sink_undrained_rows) | ticket, `for: 0m` | shutdown drain abandoned buffered rows the cursor had already passed |

## stellarindex_ingestion_insert_errors

Also covers `stellarindex_ingestion_persist_drop` (any-nonzero tripwire on the money-flow kinds `trade` / `soroswap_router_swap` / `defindex_flow_strategy` / `defindex_flow_vault`) and `stellarindex_ingestion_trustline_observation_drop` (`kind="trustline_observation"`, no retry path).

Symptom: `stellarindex_source_insert_errors_total{source=...,kind=...}` above 6/min sustained. `kind` is `trade` (permanently dropped), `trade_abandoned` (retry abandoned on shutdown; cursor held, re-derivable, not a loss), `panic`, `dropped` (external retry-buffer overflow), or a per-domain persist kind.

Check:

```sh
# Which source + which kind is failing?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_insert_errors_total'

# Is it the storage layer itself?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com   # rules out upstream; r1 has no local stellar-rpc
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "SELECT now(), pg_is_in_recovery();"'

# Actual failure reason is in the indexer's logs:
journalctl -u stellarindex-indexer --since -2h | grep -E "insert (trade|oracle update) failed" | tail
```

Likely causes (by log line):

- `connection refused`: Timescale down or partitioned; see `postgres.md#stellarindex_timescale_primary_down`. For `kind=trade` this normally lands in backpressure; seeing it here means the retry buffer overflowed (`kind=dropped`).
- `disk full` / `no space`: see `postgres.md#stellarindex_timescale_disk_full`.
- `duplicate key value`: should be impossible (idempotent ON CONFLICT); primary-key invariant broken, a data-integrity incident.
- `violates check constraint`: malformed source data (negative amounts, bad tx_hash); a decoder bug, check `stellarindex_source_decode_errors_total`. One source = decode/schema issue; all sources = shared storage.

Fix:

1. Fix Timescale first (`postgres.md#stellarindex_timescale_primary_down`); insert errors are a symptom.
2. Disk full: grow the volume (bare-metal NVMe + ZFS per [ADR-0008](../../adr/0008-ha-topology.md); `zpool` / Hetzner resize). The indexer retries on its own once there is headroom.
3. Backfill the lost range:

   ```sh
   # detect-gaps is cursor-vs-tip only; find-data-gaps reports the actual missing ledger ranges.
   stellarindex-ops detect-gaps -config /etc/stellarindex.toml \
       -threshold 50
   stellarindex-ops find-data-gaps -config /etc/stellarindex.toml \
       -source <SOURCE_NAME> -output text
   # Dry-run first to confirm scope.
   stellarindex-ops backfill -config /etc/stellarindex.toml \
       -from <FIRST_LEDGER> -to <LAST_LEDGER> \
       -source <SOURCE_NAME> -dry-run
   # Drop -dry-run to commit.
   stellarindex-ops backfill -write -config /etc/stellarindex.toml \
       -from <FIRST_LEDGER> -to <LAST_LEDGER> \
       -source <SOURCE_NAME> -resume
   ```

4. Verify `rate(stellarindex_source_insert_errors_total[1m])` goes to 0; the alert clears when the 5m rate is below 0.1/s.

Escalate: any `duplicate key value`, or loss with Postgres healthy.

## stellarindex_ingestion_trade_insert_backpressure

Symptom: `sum(rate(stellarindex_trade_insert_retries_total{outcome="retry"}[5m])) > 0` for 10 min. The served tier is FROZEN (cursor held) but no data is lost: on-chain trades block-and-retry, external CEX/FX trades buffer in memory. Infra faults are classified by [`timescale.IsInfraError`](../../../internal/storage/timescale/errors.go). Also: `stellarindex_cursor_last_ledger` flat (`stellarindex_ingestion_cursor_stuck` may co-fire), `stellarindex_trade_insert_buffer_depth` climbing, journal `infrastructure fault on sink write — retrying with backpressure`.

Check:

```sh
# Is Postgres actually up? (the near-universal cause)
systemctl status postgresql
sudo -u postgres psql -d stellarindex -c 'SELECT 1'

# Retry vs recovery mix + external buffer depth
curl -s localhost:9464/metrics | grep -E 'trade_insert_retries_total|trade_insert_buffer_depth'

# Indexer's own view
journalctl -u stellarindex-indexer --since -15m | grep -iE 'backpressure|connection refused|abandoned'
```

Likely causes:

- Postgres outage (`SELECT 1` fails).
- Pool exhaustion (`too_many_connections`, SQLSTATE 53300) or a network partition to the DB host.

Fix:

1. Bring Postgres back (restart the service, clear disk, restore the network path). The sink recovers on its own.
2. Pool exhaustion: `SELECT pid, state, query_start, left(query,80) FROM pg_stat_activity ORDER BY query_start;` and kill the hog.
3. Verify `stellarindex_trade_insert_retries_total{outcome="recovered"}` increments, the cursor advances, and `stellarindex_trade_insert_buffer_depth` drains to 0.

Nothing to re-derive for on-chain trades unless the indexer was HARD-restarted meanwhile (then the ADR-0033 completeness verdict flags the residue; a graceful shutdown logs the abandoned range at ERROR). External trades are lost only if the outage outlasted the bounded buffer (`stellarindex_source_insert_errors_total{kind="dropped"}`) and are vendor-refillable via the connector backfill path.

## stellarindex_ingestion_duplicate_flood

Symptom: `stellarindex_trade_insert_outcome_total{source=...,outcome="duplicate"}` > 0.5/s for >= 10 min with `outcome="new"` at 0 or ABSENT (the counter is call-site-seeded; the rule treats absent and zero alike). Events are still decoded (`stellarindex_source_events_total` climbing), `stellarindex_cursor_last_ledger{source="ledgerstream"}` advances, but `max(ts)` for the source in `trades` is frozen and `/v1/markets?source=<X>` shows the same `last_trade_at`. No data loss; `/v1/price` is stale-but-flagged.

`outcome="duplicate"` is a conflation: the upsert is `ON CONFLICT ... DO UPDATE ... WHERE trades.derive_generation <= EXCLUDED.derive_generation` (migration 0109), so the label covers a true duplicate, a generation-guarded corrective UPDATE (a re-derive working as intended), and a guard-skipped write. A running corrective re-derive therefore produces this exact signature. Check for one first:

```sh
ssh root@136.243.90.96 'systemctl list-units "heavy-*.scope" --all --no-pager; pgrep -a stellarindex-ops'
```

If one is running, the alert is expected for the job's duration; the freshness SQL below is the signal that matters.

Check:

```sh
ssh root@136.243.90.96

# 1. Confirm the duplicate vs new split.
curl -sS localhost:9464/metrics | grep stellarindex_trade_insert_outcome_total

# 2. Confirm the trades hypertable is actually stale.
sudo -u postgres psql stellarindex -c "
  SELECT source, max(ts) AT TIME ZONE 'UTC' AS max_ts,
         count(*) FILTER (WHERE ts > NOW() - INTERVAL '1 hour') AS rows_last_hour
    FROM trades
   WHERE ts > NOW() - INTERVAL '24 hours'
   GROUP BY source
   ORDER BY max_ts DESC;"

# 3. Confirm the indexer cursor IS advancing.
curl -sS localhost:9464/metrics | grep cursor_last_ledger

# 4. Look at the ingestion_cursors table for stuck backfills shadowing live ingest.
sudo -u postgres psql stellarindex -c "
  SELECT source, sub_source, last_ledger, last_updated
    FROM ingestion_cursors
   ORDER BY last_updated DESC
   LIMIT 20;"
```

Likely causes:

- Cursor jumped past data without persisting (backpressure absorbed by the channel buffer, events dropped on shutdown, cursor upserted anyway): the gap must be backfilled.
- A leaked event channel from a prior process (unlikely; single sink goroutine).
- A backfill replaying the same range: look for a `backfill` row in `ingestion_cursors` and a running `stellarindex-ops backfill`.

Fix (cause 1; the trades PK `(source, ledger, tx_hash, op_index, ts)` makes backfill idempotent):

1. Find the gap and print the plan:

   ```sh
   # Lowest ledger to backfill is one past max; upper is the current cursor.
   sudo -u postgres psql stellarindex -t -c "SELECT max(ledger) FROM trades WHERE source = 'sdex';"

   stellarindex-ops backfill -config /etc/stellarindex.toml \
     -from <max_ledger+1> -to <current_cursor> \
     -source sdex,aquarius,soroswap,phoenix,comet -parallel 4 -dry-run
   ```

2. Run it through the heavy-job wrapper, one at a time (`-source` is singular, comma-separated; `-parallel` divides the wrapper's `MemoryMax=20G` budget, it does not multiply it; see `docs/operations/maintainer-workflow.md`, "Heavy one-shot jobs"):

   ```sh
   sudo /usr/local/sbin/run-heavy-job.sh dupflood-backfill \
     /usr/local/bin/stellarindex-ops backfill -write \
       -config /etc/stellarindex.toml \
       -from <max_ledger+1> -to <current_cursor> \
       -source sdex,aquarius,soroswap,phoenix,comet \
       -parallel 4
   ```

3. `backfill` refuses sources that are not `BackfillSafe` (`internal/sources/external/registry.go`). For a projected (Soroban-derived) source use `projector-replay` (dry run is the default; `-write` is required):

   ```sh
   sudo /usr/local/sbin/run-heavy-job.sh dupflood-replay \
     /usr/local/bin/stellarindex-ops projector-replay \
       -config /etc/stellarindex.toml -source <name> -from <ledger> -write
   ```

   Beyond roughly 1M ledgers use `projected-rebuild` instead (same `-write` gate, plus `-workers` / `-window`).
4. Cause 2: `systemctl restart stellarindex-indexer`. Cause 3: stop the looping backfill (`systemctl stop` or `kill`).
5. Verify (wait 2x the scrape interval): `outcome=new` climbs again and the table accepts fresh rows:

   ```sh
   curl -sS localhost:9464/metrics | grep stellarindex_trade_insert_outcome_total
   sudo -u postgres psql stellarindex -c "
     SELECT max(ts) AT TIME ZONE 'UTC' FROM trades WHERE source = 'sdex';"
   ```

The alert clears after `for: 10m` with healthy `outcome=new`. Metric emitted in `internal/storage/timescale/trades.go:Store.InsertTrade`; see `docs/reference/metrics/README.md#stellarindex_trade_insert_outcome_total`.

## stellarindex_ingestion_sink_undrained_rows

Symptom: `increase(stellarindex_sink_undrained_rows_total[1h]) > 0` (ticket, `for: 0m`). The sink (`sink="persist_events"`) shut down with rows still buffered; its bounded drain (20 s drain + 5 s final pass inside `pipeline.ShutdownDeadline` = 30 s) could not land them and the cursor had ALREADY advanced past them: a served-tier gap the indexer never revisits. `kind` is `trade` or `event`. The counter increments only in `reportAbandonedTrades`, `reportAbandonedEvent` and the external retry buffer's `finalDrain`.

Caveat: the increment lands 20-25 s after SIGTERM and r1 scrapes every 15 s, so a single loss can fall between scrapes. The journal ERROR line is authoritative; after any deploy during which Postgres was unhealthy, run the check below even if nothing fired.

Check:

```sh
# The authoritative record: what was abandoned, and the range / source to re-derive.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-3h" --no-pager | grep -E "abandoned on shutdown|drain deadline exceeded|retry buffer not fully drained"'

# What the counter saw (may be smaller than the journal).
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=increase(stellarindex_sink_undrained_rows_total%5B1h%5D)" | python3 -m json.tool | grep -E "\"sink\"|\"kind\"|value"'

# Why the drain tripped: was Postgres healthy across the restart?
ssh root@136.243.90.96 'journalctl -u postgresql -u stellarindex-indexer --since "-3h" --no-pager | grep -iE "shutting down|ready to accept|connection refused|infrastructure fault" | tail -40'
```

Record per ERROR line: `ledger_from`/`ledger_to` (trades) or `source` (events), and the restart time. Triage:

- `kind="trade"` with a non-zero ledger range (SDEX and Soroban DEXes): Resolution A.
- `kind="event"` (oracle, supply, blend / cctp / rozo): Resolution B; the line names `source`.
- `kind="trade"` with `ledger_from=0`, or the `retry buffer not fully drained` line (`venues=`): external CEX/FX, Resolution C.
- Alert silent but journal shows the lines: scrape missed it; proceed as if it fired. Alert firing with no ERROR line: a log-shipping problem, read the range from the `PersistEvents drain deadline exceeded` line.

Resolution A: re-derive from the ClickHouse lake (ADR-0034). `-sdex-gaps` restricts the pass to ledgers the served tier is short on. Dry-run, then write (on r1 under `/usr/local/sbin/run-heavy-job.sh`); the default event pass covers Soroban DEX trades too.

```sh
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps -write
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <ledger_from> -write
```

For wide ranges see [sdex-gap-detected](ingest-gap.md#sdex-gap-detected).

Resolution B: `consumer.Event` carries no ledger; take the window from the `drain deadline exceeded` line or the cursor at the restart minus a margin. Oracle / ContractCall sources (band, soroswap-router): `stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -from <from> -to <to> -contract-calls -sources <source>` (dry-run, then `-write`; see [oracle-unknown-symbols](ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols)). Event-based sources: the same without `-contract-calls`. Confirm with the per-source [gap detector](ingest-gap.md#stellarindex_ingest_gap_detected) and the completeness verdict.

Resolution C: external trades have no ledger and no lake copy; record venue and window in the incident record. The next poll refills the feed.

Resolution D: find out why Postgres could not absorb a few hundred rows in 20 s (autovacuum on `trades`, a [lock convoy](postgres.md#stellarindex_pg_lock_convoy), or the deploy stopping Postgres before the indexer). If it was genuinely down, the loss was the designed outcome and the re-derive is the whole remedy.

## Related

- [ch-live-sink](clickhouse.md#ch-live-sink): ClickHouse half of the same class (healed by the `ch-live-catchup` timer; undrained-rows loss only by the re-derive above).
- [ledger-ingest.md#stellarindex_ingestion_cursor_stuck](ledger-ingest.md#stellarindex_ingestion_cursor_stuck), [ingestion.md#stellarindex_ingestion_all_sources_stopped](ingestion.md#stellarindex_ingestion_all_sources_stopped), [ingestion.md#stellarindex_ingestion_decode_error](ingestion.md#stellarindex_ingestion_decode_error).
- [ADR-0041](../../adr/0041-ingest-durability-semantics.md), `docs/adr/0034-tiered-clickhouse-architecture.md`, `internal/pipeline/trade_sink.go`, `internal/pipeline/sink.go`.
