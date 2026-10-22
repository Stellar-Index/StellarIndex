---
title: Runbook — ClickHouse alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — ClickHouse server health

## At a glance

Rules: `deploy/monitoring/rules/clickhouse.yml` and `configs/prometheus/rules.r1/clickhouse.yml`. P1 for `_server_down`, P3 for the rest. ClickHouse is the lake ([ADR-0034](../../adr/0034-tiered-clickhouse-architecture.md)): explorer reads, rollups and the CH-fed projector all read it.

Producer: clickhouse-server itself on port 9363 (no exporter process), enabled by the archival-node drop-in `configs/ansible/roles/archival-node/tasks/22-clickhouse-exporter.yml` (renders `/etc/clickhouse-server/config.d/si-prometheus.xml`), scraped by the `clickhouse` job in `configs/prometheus/prometheus.r1.yml`. Metric prefixes: `ClickHouseProfileEvents_*` (`FailedQuery`, `RejectedInserts`), `ClickHouseMetrics_*`, `ClickHouseAsyncMetrics_*`.

## Quick diagnosis

```sh
ssh root@136.243.90.96

# 1. Endpoint there? (000/refused = not enabled or server down; 200 = answering)
curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 http://127.0.0.1:9363/metrics

# 2. Enabled? The listener is the proof.
ls -l /etc/clickhouse-server/config.d/si-prometheus.xml
ss -ltnp 'sport = :9363'

# 3. Server healthy?
systemctl status clickhouse-server
journalctl -u clickhouse-server -n 50 --no-pager

# 4. _query_failures_high: what is failing?
clickhouse-client --port 9300 -q "SELECT type, exception_code, count() \
  FROM system.query_log WHERE event_time > now() - INTERVAL 1 HOUR \
  AND type != 'QueryFinish' GROUP BY 1, 2 ORDER BY 3 DESC FORMAT PrettyCompact"

# 5. _inserts_rejected: which partition has too many parts?
clickhouse-client --port 9300 -q "SELECT database, table, partition, count() \
  FROM system.parts WHERE active GROUP BY 1, 2, 3 ORDER BY 4 DESC LIMIT 20 \
  FORMAT PrettyCompact"
clickhouse-client --port 9300 -q "SELECT * FROM system.merges FORMAT Vertical"
```

## stellarindex_clickhouse_server_down

Page. `up{job="clickhouse"} == 0 OR absent_over_time(up{job="clickhouse"}[10m]) == 1` for 2m. Normally the server is down; the absent arm covers a missing scrape job or never-enabled endpoint. While it holds, every other ClickHouse rule is blind.

**A. Endpoint never enabled** (rules ship with `deploy.yml`, the drop-in does not). From `configs/ansible`:

1. `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter --check --diff`
2. `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter` (tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3`). No restart is needed; its verify task polls `http://127.0.0.1:9363/metrics`.
3. If `curl -s http://127.0.0.1:9090/api/v1/targets | grep clickhouse` is empty, ship `configs/prometheus/prometheus.r1.yml` and `systemctl reload prometheus`.
4. The alert clears ~2 min after the first good scrape.

**B. Server actually down**

1. `systemctl start clickhouse-server`; read the journal for the refusal (bad `config.d` drop-in, full pool, corrupt part).
2. Expect ingest gaps. `ch-live-catchup.timer` is the only thing that heals a Tier-1 hole; confirm it runs and `ContiguousWatermark` climbs, or the projector stays clamped ([projector-lag](projector.md#stellarindex_projector_lag_high)).

False positive: a deliberate restart. Silence for the window.

Escalate: if the server will not start, page the storage owner.

## stellarindex_clickhouse_query_failures_high

Ticket. `rate(ClickHouseProfileEvents_FailedQuery[10m]) > 0.1` for 15m. Served reads surface this as 5xx (8 s serving ceiling).

Check: step 4 above, before changing anything.

Likely causes:

- `READONLY`: the ADR-0048 D4 serving profile (`20-clickhouse-serving-profile.yml`).
- `MEMORY_LIMIT_EXCEEDED`: a heavy job competing with served reads.
- `UNKNOWN_TABLE` / `TYPE_MISMATCH`: a schema change landed on one side only (cross-check `ch-schema-drift.service`).

Fix: address the exception class. Do NOT widen the serving profile's limits to silence this.

## stellarindex_clickhouse_inserts_rejected

Ticket. `increase(ClickHouseProfileEvents_RejectedInserts[1h]) > 0` for 5m. "Too many parts": merges are not keeping up. Holds until the trailing hour is clear, so a stopped burst still shows; `rate(...[5m]) > 0` says if it is ongoing. A rejected block is a GAP that is not retried; the projector clamps to the contiguous watermark.

Fix:

1. Find the partition (step 5); check whether merges are running, starved or stuck (`system.merges`, `system.mutations`).
2. After back-pressure clears, verify `ch-live-catchup.timer` healed the range.

False positive: during a bulk `ch-backfill`, rejections are back-pressure, not loss, provided the backfill's resume state records the window incomplete. Verify first.

## stellarindex_counts_rollup_plan_unusable

Ticket. `stellarindex_counts_rollup_plan_ok{rollup} == 0` for 30m, exported by the API on each plan refresh (1/min, on page traffic). The explorer reader has no usable plan for `account_op_type_counts` (op-types) or `account_movement_counts` (movements), so account activity, transactions/operations and movements read the source table whole: correct, but seconds per page.

Fix:

1. Table missing: build it per `deploy/clickhouse/account_op_types.sql` Step 6 (`ch-optype-rollup -max-closed 0 -write` under `run-heavy-job.sh`), then enable `optype-rollup.timer`.
2. Table present: `stellarindex-ops ch-optype-rollup` (dry run) lists the partitions it would rebuild; check the API log for the sentinel read error.

## stellarindex_counts_rollup_stale_partitions

Ticket. `stellarindex_counts_rollup_stale_partitions{rollup} > 0` for 2h. Partitions whose rollup row count no longer matches the source are read from source. `optype-rollup.timer` rebuilds 4 per run; check it is enabled and `journalctl -u optype-rollup` shows runs. After a backfill into many closed partitions, run `ch-optype-rollup -max-closed 0 -write` under `run-heavy-job.sh`. `ErrCountsShrink` in the log means a recompute counted fewer rows than the live partition: find out why before passing `-shrink-ok`.

## ch-live-sink

Alerts on `stellarindex_ch_live_sink_*` counters (rules: `deploy/monitoring/rules/ingestion.yml`, `configs/prometheus/rules.r1/ingestion.yml`). The indexer writes each ledger twice ([ADR-0034](../../adr/0034-tiered-clickhouse-architecture.md), [ADR-0041](../../adr/0041-ingest-durability-semantics.md)): `clickhouse.ExtractLedger` to the lake's live edge and `dispatcher.CensusLedger` to `ledger_ingest_log`. Served pricing is unaffected by drops and errors; under `read_undercount` projected Soroban sources (soroswap, aquarius, phoenix) under-read the short ledgers until replayed. Identify which counter fired; remedies differ:

- drop: accepted then shed under buffer pressure; healed by `ch-live-catchup`.
- error: never written; a systematic extract break is NOT healed by the timer.
- read undercount: written short; the `stellar.ledgers` row exists, so `ch-live-catchup` never revisits it.

### stellarindex_ingestion_ch_live_sink_drops

Ticket; `stellarindex_ingestion_ch_live_sink_drops_sustained` pages at 1h. `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[10m]) > 0`. The sink is non-blocking by design: under buffer pressure it drops a whole ledger extract instead of stalling ingest. The alert means dropping is continuous. The lake tail lags and the ADR-0033 substrate claim degrades for recent ledgers until caught up.

Check:

```sh
curl -s 'http://127.0.0.1:8123/' --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'
sudo -u postgres psql -d stellarindex -c "SELECT last_ledger FROM ingestion_cursors WHERE source='ledgerstream'"
curl -s localhost:9464/metrics | grep ch_live_sink_ledgers_total
systemctl status ch-live-catchup.timer ch-live-catchup.service
journalctl -u ch-live-catchup.service --since -2h | tail -50
curl -s 'http://127.0.0.1:8123/' --data-binary "SELECT metric, value FROM system.metrics WHERE metric IN ('BackgroundMergesAndMutationsPoolTask','DelayedInserts')"
df -h /
```

Likely causes:

- ClickHouse down, wedged or slow (merges, disk).
- Sink buffer undersized for a burst.
- Indexer host CPU/IO-starved.

Fix:

1. CH down/wedged: restart `clickhouse-server`; watch the root filesystem.
2. Saturated buffer: raise the sink buffer in indexer config; restart the indexer in a quiet window.
3. Force a heal: `systemctl start ch-live-catchup.service`, then confirm contiguity: `stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <pre-gap ledger> -write`.

Escalate: sustained page and catch-up cannot keep up: SEV-2, follow `docs/operations/sev-playbook.md`.

### stellarindex_ingestion_ch_live_sink_errors

Ticket. `increase(stellarindex_ch_live_sink_ledgers_total{outcome="errored"}[30m]) > 0`. Ledgers are missing from the lake's live edge; the projector clamps behind the hole and `/v1/coverage` reports `complete=false`. Two producers: (1) `clickhouse.ExtractLedger` failed in the indexer read loop (a `TransactionMeta`/LCM version break fails every ledger; one bad transaction fails one); (2) the sink's `Add`/`Flush` failed (ClickHouse write-path fault).

Check:

```sh
journalctl -u stellarindex-indexer --since -1h | grep -i 'ch extract'
curl -s localhost:9464/metrics | grep ch_live_sink_ledgers_total
clickhouse-client --port 9300 -q 'SELECT 1'
curl -s 'http://127.0.0.1:8123/' --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'
df -h /var/lib/clickhouse
sudo -u postgres psql -d stellarindex -tAc "SELECT max(last_ledger) FROM ingestion_cursors"
```

Every ledger errors with a decode message: case 1 systematic (upstream protocol/SDK break). Isolated ledgers: case 1 single-transaction. No `ch extract` lines: case 2.

Fix:

1. Case 2: restore ClickHouse (restart, free disk), then `systemctl start ch-live-catchup.service`; watch `journalctl -u ch-live-catchup.service -f`.
2. Case 1 isolated: run the same heal pass. If the same ledger fails again, treat as systematic.
3. Case 1 systematic: `ch-live-catchup` uses the same decoder and cannot heal it. Identify the meta version from the journal and treat as a decoder / SDK upgrade; triage matrix in [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).
4. Verify the counter stops and the range is contiguous:

```sh
clickhouse-client --port 9300 -q "
  SELECT count() FROM (
    SELECT ledger_seq, ledger_seq - lagInFrame(ledger_seq) OVER (ORDER BY ledger_seq) AS d
    FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= <from>)
  ) WHERE d > 1"
```

The alert clears 30 min after the last error. A short burst during a ClickHouse restart is a false positive.

### stellarindex_ingestion_ch_live_sink_read_undercount

Ticket. `increase(stellarindex_ch_live_sink_read_undercount_total[30m]) > 0`. Ledgers are in the lake with fewer transactions, events or entry changes than the chain, or lack a `ledger_ingest_log` row; nothing downstream can tell. The `kind` label separates paths; the value is transactions affected:

- Lake path (ledger written short; the projector treats it as complete): `tx_read_errors`, `tx_event_read_errors`, `entry_meta_unsupported`; `soroban_fee_meta_unsupported` (fee columns 0 in `stellar.transactions`); `evicted_keys_unreadable` (+1 per ledger; evicted entries still read live in `stellar.ledger_entries_current`); `entry_changes_unencodable` (+1 per change).
- Substrate path: `tx_read_errors_census`, `tx_event_read_errors_census` (the `ledger_ingest_log` row is skipped, leaving a substrate gap).

Check:

```sh
curl -s localhost:9464/metrics | grep ch_live_sink_read_undercount_total
journalctl -u stellarindex-indexer --since -1h \
  | grep -E 'read undercount|census read errors'
curl -s localhost:9464/metrics | grep 'ch_live_sink_ledgers_total{outcome="written"}'
```

Counter advancing about as fast as `written`: systematic (protocol upgrade changed `TransactionMeta`, or SDK bump). A few ledgers: a malformed transaction; record the sequences.

Fix:

1. Systematic: decoder / SDK upgrade; neither `ch-live-catchup` nor a restart helps. See [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).
2. Isolated, lake path, once the reader is fixed: `stellarindex-ops ch-backfill -config PATH -from <ledger> -to <ledger>` (idempotent), then `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>` for each projected source that read them.
3. Isolated, substrate path: `stellarindex-ops census-backfill -config PATH -from <ledger> -to <ledger> -write` (exits non-zero if any ledger is still unreadable).
4. Verify the counter stops and the substrate has rows:

```sh
sudo -u postgres psql -d stellarindex -tAc \
  "SELECT count(*) FROM ledger_ingest_log WHERE ledger_seq BETWEEN <from> AND <to>"
```

The alert clears 30 min after the last increment. If only one path advanced, look at the code that differs between `ExtractLedger` and `CensusLedger`. No known false positives.

## Related

- [ch-schema-restore](ch-schema-restore.md); [exporter-down](meta.md#stellarindex_redis_exporter_down); [projector-lag](projector.md#stellarindex_projector_lag_high).
- [sink-undrained-rows](ingestion-sink.md#stellarindex_ingestion_sink_undrained_rows): the Postgres twin of drops; re-derive from the lake, no timer heals it.
- `ingestion.md#stellarindex_ingestion_all_sources_stopped`: live ingest itself stopped.
- `internal/storage/clickhouse/live_sink.go`, `cmd/stellarindex-indexer/main.go` (`recordCHLiveSinkUndercount`, `recordLedgerIngestCensusSkip`).
