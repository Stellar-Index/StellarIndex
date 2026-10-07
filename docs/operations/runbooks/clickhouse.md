---
title: Runbook — ClickHouse alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — ClickHouse server health

Rules: `deploy/monitoring/rules/clickhouse.yml` and `configs/prometheus/rules.r1/clickhouse.yml` (identical). Severity: P1 (page) for `_server_down`; P3 (ticket) for the other two. Typical MTTR: 10 min (`_server_down` where the config is simply not applied), 30 min otherwise.

ClickHouse is the lake (ADR-0034). The API's explorer reads (`/v1/ledgers`, `/v1/tx`, `/v1/accounts/*`), the supply/census rollups and the CH-fed projector all read it. While `_server_down` holds, none of that has a health signal.

**Producer:** clickhouse-server itself on port 9363; there is no exporter process. Enabled by the archival-node role's `configs/ansible/roles/archival-node/tasks/22-clickhouse-exporter.yml` drop-in (`templates/clickhouse-prometheus.xml.j2` renders `/etc/clickhouse-server/config.d/si-prometheus.xml`); scraped by the `clickhouse` job in `configs/prometheus/prometheus.r1.yml`. The stock `/etc/clickhouse-server/config.xml` ships its `<prometheus>` block inside an XML comment, so without the drop-in nothing listens on 9363.

**Metric namespace** (ClickHouse's own names):

| Prefix | Source table | Used by |
| ------ | ------------ | ------- |
| `ClickHouseProfileEvents_*` | `system.events` (counters) | `FailedQuery`, `RejectedInserts` |
| `ClickHouseMetrics_*` | `system.metrics` (gauges) | ad-hoc triage |
| `ClickHouseAsyncMetrics_*` | `system.asynchronous_metrics` | ad-hoc triage |

## Quick diagnosis

```sh
ssh root@136.243.90.96

# 1. Endpoint there? (000/refused = not enabled or server down; 200 = answering)
curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 http://127.0.0.1:9363/metrics

# 2. Enabled? <prometheus> is a config-file section, not a system.server_settings
#    row, so the listener is the proof.
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

## At a glance

- [`stellarindex_clickhouse_server_down`](#stellarindex_clickhouse_server_down)
- [`stellarindex_clickhouse_query_failures_high`](#stellarindex_clickhouse_query_failures_high)
- [`stellarindex_clickhouse_inserts_rejected`](#stellarindex_clickhouse_inserts_rejected)
- [`ch-live-sink`](#ch-live-sink)

## stellarindex_clickhouse_server_down

Trips: `up{job="clickhouse"} == 0 OR absent_over_time(up{job="clickhouse"}[10m]) == 1`, `for: 2m`, severity page.

The scrape target is down 2+ min, or no `up{job="clickhouse"}` series exists. The endpoint is inside the server, so a down target is normally the SERVER being down. The absent arm covers a missing scrape job or never-enabled endpoint (`== 0` over an empty vector can never fire). This alert is expected to fire from the moment the rules deploy until the drop-in is applied; that firing is the finding. While it holds, every other ClickHouse rule is blind.

**A. Endpoint never enabled** (rules ship with `deploy.yml`, the ansible drop-in does not). From `configs/ansible`:

- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter --check --diff`
- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter` (tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3`)
- [ ] The play's `Verify the ClickHouse Prometheus endpoint is serving` task must pass: it polls `http://127.0.0.1:9363/metrics` for a `ClickHouseProfileEvents_` line. No clickhouse-server restart is needed or performed; the `config.d` reloader picks the drop-in up within seconds.
- [ ] The alert clears within ~2 min of the first successful scrape.

If Prometheus has no `clickhouse` job (`curl -s http://127.0.0.1:9090/api/v1/targets | grep clickhouse` empty), ship `configs/prometheus/prometheus.r1.yml` and `systemctl reload prometheus` as well.

**B. Server actually down**

- [ ] `systemctl start clickhouse-server`; read the journal for the refusal (bad `config.d` drop-in, full pool, corrupt part).
- [ ] Expect ingest gaps for the outage window. `ch-live-catchup.timer` is the only thing that heals a Tier-1 hole; confirm it runs and that `ContiguousWatermark` resumes climbing, or the projector stays clamped ([projector-lag](projector.md#stellarindex_projector_lag_high)).

False positive: a deliberate ClickHouse restart (version upgrade, an `si-*.xml` change that needs one). Silence for the window rather than removing the alert.

## stellarindex_clickhouse_query_failures_high

Trips: `rate(ClickHouseProfileEvents_FailedQuery[10m]) > 0.1`, `for: 15m`, severity ticket.

More than 0.1 failed queries/s (about 6/min) for 15 min; each is a served read, rollup or ops job that got an exception. The API's ClickHouse-backed explorer routes surface it as a 5xx (the serving reader has an 8 s ceiling).

- [ ] Read the exception class (step 4) before changing anything. `READONLY` points at the ADR-0048 D4 serving profile (`20-clickhouse-serving-profile.yml`); `MEMORY_LIMIT_EXCEEDED` at a heavy job competing with served reads; `UNKNOWN_TABLE` / `TYPE_MISMATCH` at a schema change that landed on one side only (cross-check `ch-schema-drift.service`).
- [ ] Do NOT widen the serving profile's limits to silence this: the profile bounds public traffic, it is not a budget to grow.

## stellarindex_clickhouse_inserts_rejected

Trips: `increase(ClickHouseProfileEvents_RejectedInserts[1h]) > 0`, `for: 5m`, severity ticket.

An INSERT was refused with "Too many parts": merges are not keeping up on some partition. Fires 5 min after the first rejection and holds until the trailing hour is clear (the short `for` is a scrape-flap debounce), so a burst that has already stopped is still on the board: read it as "this happened". `rate(ClickHouseProfileEvents_RejectedInserts[5m]) > 0` says whether it is still going. A rejected block is a GAP: the in-dispatcher dual-sink does not retry it, and the projector clamps to the contiguous watermark, so an unhealed gap stalls it. Burst shapes are pinned in `deploy/monitoring/rule-tests/clickhouse_test.yml`.

- [ ] Find the partition (step 5); confirm whether merges are running, starved or stuck (`system.merges`, `system.mutations`).
- [ ] After back-pressure clears, verify `ch-live-catchup.timer` has healed the range.

False positive: during a bulk `ch-backfill` (writes far faster than live ingest), a burst of rejections is back-pressure working, not data loss, as long as the backfill's own resume state records the window as incomplete. Verify that before dismissing.

## ch-live-sink

**Runbook — ClickHouse live-sink alerts**

_Source page `clickhouse.md#ch-live-sink`: status living, severity P3 (ticket); P2 page when drops sustain 1h, last verified 2026-10-06._

The indexer's real-time ClickHouse dual-sink (ADR-0034 / ADR-0041) writes each ledger twice: `clickhouse.ExtractLedger` feeds the lake's live edge and `dispatcher.CensusLedger` feeds the `ledger_ingest_log` substrate record. All alerts here read `stellarindex_ch_live_sink_*` counters, rules in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml`. Served pricing is unaffected by drops and errors (the Postgres served tier is written by an independent path). Under `read_undercount` it is not: the projector reads CH `contract_events`, so projected Soroban sources (soroswap, aquarius, phoenix), and so DEX pricing, under-read the short ledgers until replayed.

Check which `outcome`/counter fired before acting; the remedies differ:

- **drop**: the sink *accepted* the ledger and then shed it under buffer pressure. By design, healed by `ch-live-catchup`.
- **error**: the ledger was never written (extract failed, or the sink's `Add`/`Flush` failed). A systematic extract break is NOT healed by the catch-up timer.
- **read undercount**: the ledger was written short (fewer txs/events/entry changes than the chain holds) or has no substrate row. Its `stellar.ledgers` row exists, so `ch-live-catchup` (which re-fills MISSING ledgers) never revisits it.

### At a glance

- [`stellarindex_ingestion_ch_live_sink_drops`](#stellarindex_ingestion_ch_live_sink_drops) (ticket) / [`stellarindex_ingestion_ch_live_sink_drops_sustained`](#stellarindex_ingestion_ch_live_sink_drops) (page at 1h)
- [`stellarindex_ingestion_ch_live_sink_errors`](#stellarindex_ingestion_ch_live_sink_errors) (ticket)
- [`stellarindex_ingestion_ch_live_sink_read_undercount`](#stellarindex_ingestion_ch_live_sink_read_undercount) (ticket)

### stellarindex_ingestion_ch_live_sink_drops

Alerts: `stellarindex_ingestion_ch_live_sink_drops` (ticket) and `…_drops_sustained` (page at 1h sustained). Detected by `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[10m]) > 0`. Typical MTTR: minutes (CH restart / pressure passes).

Impact: the certified-lake TAIL lags live; served pricing unaffected. Healed by `ch-live-catchup`; if drops outpace the heal, the ADR-0033 substrate claim for recent ledgers degrades until caught up. The risk horizon is the completeness verdict and lake-derived surfaces (supply for unwatched tokens, explorer lake reads) for the affected ledger range until catch-up completes.

The sink is non-blocking BY DESIGN: under buffer pressure it drops the whole ledger extract (`outcome="dropped"`) instead of stalling live ingest. Drops are normal in rare bursts and are healed by the `ch-live-catchup` timer, which re-extracts missing lake ledgers from Galexie. This alert fires when dropping is *continuous*: the heal path is being exercised abnormally (ticket), or losing the race (page at 1h).

Causes:

- ClickHouse is down, wedged, or slow (merges, disk; the CS-112 no-backup lake is also the one filling the disk).
- The sink buffer is undersized for a ledger-volume burst.
- The indexer host is CPU/IO-starved so the sink's writer goroutine can't drain.

Investigate:

```sh
# Is CH alive + how far behind is the lake tail?
curl -s 'http://127.0.0.1:8123/' --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'
sudo -u postgres psql -d stellarindex -c "SELECT last_ledger FROM ingestion_cursors WHERE source='ledgerstream'"

# Drop rate + sink outcome mix
curl -s localhost:9464/metrics | grep ch_live_sink_ledgers_total

# Is the heal timer running?
systemctl status ch-live-catchup.timer ch-live-catchup.service
journalctl -u ch-live-catchup.service --since -2h | tail -50

# CH pressure
curl -s 'http://127.0.0.1:8123/' --data-binary "SELECT metric, value FROM system.metrics WHERE metric IN ('BackgroundMergesAndMutationsPoolTask','DelayedInserts')"
df -h /   # remember the CH-log root-fill incident (2026-06-11)
```

Mitigate:

1. If CH is down/wedged: restart `clickhouse-server`; watch the root filesystem (logs go to ZFS since 5dd6fcda, but verify).
2. If the sink buffer is saturated on bursts: raise the sink buffer (indexer config) and restart the indexer during a quiet window.
3. Force a heal pass once CH is healthy: `systemctl start ch-live-catchup.service`, then confirm the lake tail is contiguous: `stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <pre-gap ledger> -write` (any strict source works; substrate is global).

Escalate: sustained page + heal path cannot catch up: treat as SEV-2 (lake tail integrity), follow `docs/operations/sev-playbook.md`.

Post-mortem notes from prior firings: none yet (alert added 2026-07-02, ADR-0041).

### stellarindex_ingestion_ch_live_sink_errors

Ticket. Detected by `increase(stellarindex_ch_live_sink_ledgers_total{outcome="errored"}[30m]) > 0`. Typical MTTR: minutes (CH fault) to a deploy (decode fault).

Impact: ledgers are missing from the ClickHouse lake's live edge. Served pricing is unaffected; the projector clamps to the contiguous watermark behind the hole, so lake-derived surfaces stop advancing until it is healed.

Why it exists: `outcome="errored"` was emitted from day one and matched by no alert in either rule tree (both live-sink rules selected `outcome="dropped"` only, #371 F6). A drop and an error are different faults with different remedies, so folding them into the drops page would have told a responder the wrong thing. `errored` has two producers:

1. `clickhouse.ExtractLedger` failed in the indexer's live read loop, so the sink was never offered the ledger. A `TransactionMeta`/LCM version break fails **every** ledger in lock-step; a single bad transaction fails one.
2. The sink's own `Add`/`Flush` errored: a ClickHouse write-path fault (down, wedged, disk-full).

Symptoms:

- `stellarindex_ch_live_sink_ledgers_total{outcome="errored"}` climbing while `written` stalls (case 1 or 2), or while `written` continues normally (case 1 on isolated ledgers).
- A sampled `ch extract failed` WARN in the indexer journal naming the ledger sequence and the decode error.
- Downstream, if it persists: the projector's watermark stops advancing and `/v1/coverage` reports `complete=false` for the window.

Quick diagnosis (<= 5 min):

```sh
# Which producer? Extract failures name a ledger; sink faults do not.
journalctl -u stellarindex-indexer --since -1h | grep -i 'ch extract'

# Outcome mix — is `written` still advancing alongside the errors?
curl -s localhost:9464/metrics | grep ch_live_sink_ledgers_total

# Is ClickHouse itself healthy? (r1 native port is 9300, HTTP 8123.)
clickhouse-client --port 9300 -q 'SELECT 1'
curl -s 'http://127.0.0.1:8123/' --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'
df -h /var/lib/clickhouse

# Where is the hole?
sudo -u postgres psql -d stellarindex -tAc \
  "SELECT max(last_ledger) FROM ingestion_cursors"
```

- Errors on **every** ledger + a decode message: case 1, systematic. An upstream protocol/SDK break, not a host fault.
- Errors on **isolated** ledgers: case 1, single-transaction.
- No `ch extract` lines at all: case 2, ClickHouse write path.

Mitigation (<= 15 min):

- [ ] **Case 2 (CH fault)**: restore ClickHouse (restart, free disk), then let the heal path run: `systemctl start ch-live-catchup.service`. Watch `journalctl -u ch-live-catchup.service -f`.
- [ ] **Case 1, isolated**: run the heal pass as above. A re-read that succeeds closes the hole; if the same ledger fails again the decode fault is deterministic, so treat it as systematic.
- [ ] **Case 1, systematic**: `ch-live-catchup` re-extracts with the SAME decoder, so it cannot heal this. Do not loop on it. Identify the meta version from the journal message, then treat it as a decoder / SDK upgrade; the 2026-07-09 Galexie P27-decode SEV is the reference case. [decode-errors](ingestion.md#stellarindex_ingestion_decode_error) carries the per-source decode-regression triage matrix.
- [ ] **Verification**: the counter stops advancing, and `stellar.ledgers` is contiguous across the affected range:

```sh
clickhouse-client --port 9300 -q "
  SELECT count() FROM (
    SELECT ledger_seq, ledger_seq - lagInFrame(ledger_seq) OVER (ORDER BY ledger_seq) AS d
    FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= <from>)
  ) WHERE d > 1"
```

The alert clears on its own 30 minutes after the last error.

Root cause: the indexer journal lines around the first error (ledger sequence and raw decode error); `SELECT * FROM system.errors` on ClickHouse for case 2; whether `ch-live-catchup` healed the range and how many ranges it had to re-backfill (its output is one line per range).

False positive: a ClickHouse restart during a deploy produces a short burst of `errored` while the sink reconnects. It self-heals; the 15-minute `for` absorbs a normal restart, so a firing alert means it did not.

### stellarindex_ingestion_ch_live_sink_read_undercount

Ticket. Detected by `increase(stellarindex_ch_live_sink_read_undercount_total[30m]) > 0`. Typical MTTR: one re-derive of the affected range (isolated) to a deploy (meta-version break).

Impact: the affected ledgers are in the ClickHouse lake with fewer transactions, contract events or entry changes than the chain holds, or have no `ledger_ingest_log` substrate row. Nothing downstream can tell the lake rows are short.

Why it exists: both read paths count the transactions they could not fully read, and until this counter each count reached only a WARN log line. The two paths react differently, which the `kind` label keeps apart:

- **`tx_read_errors`, `tx_event_read_errors`, `entry_meta_unsupported`: the lake path.** The ledger is still written, because lake contiguity is the coverage proof, but its events or entry changes are short. Its `stellar.ledgers` row exists, so the projector's watermark treats it as complete and `ch-live-catchup` never revisits it. This is not the errors case above: there the ledger is absent.
- **`soroban_fee_meta_unsupported`: the lake path, fee columns.** A Soroban transaction's `TransactionMeta` version is past what the charged-fee read handles, so its `stellar.transactions` row carries 0 for the non-refundable, refundable and rent fees.
- **`evicted_keys_unreadable`: the lake path, evictions.** The ledger's evicted-keys list could not be read, so its `removed` rows are missing and every entry evicted at that ledger still reads as live in `stellar.ledger_entries_current`. The log line carries the ledger. This kind adds 1 per ledger, not a transaction count.
- **`entry_changes_unencodable`: the lake path, single changes.** An entry change could not be re-marshalled, so it wrote no row; it still took its `intra_ledger_seq` position, so later rows stay aligned with the live dispatcher. Unreachable on XDR-decoded input. This kind adds 1 per change.
- **`tx_read_errors_census`, `tx_event_read_errors_census`: the substrate path.** The indexer declines to write the `ledger_ingest_log` row, so a projection reconcile cannot pass against an undercount. That leaves a substrate gap.

The value added is the number of transactions affected, not a count of ledgers.

Symptoms:

- `stellarindex_ch_live_sink_read_undercount_total` advancing for one or more `kind` values.
- Indexer journal WARNs: `ch live-sink: ledger extracted with read undercount` (lake path) or `ledger census read errors; skipping substrate record` (substrate path), each naming the ledger.
- A meta-version break: every ledger advances the counter, and the lake looks like a run of ledgers with no Soroban events.

Quick diagnosis (<= 5 min):

```sh
# Which kinds, and how fast?
curl -s localhost:9464/metrics | grep ch_live_sink_read_undercount_total

# Which ledgers? Each WARN names the ledger and the per-kind counts.
journalctl -u stellarindex-indexer --since -1h \
  | grep -E 'read undercount|census read errors'

# Every ledger, or isolated ones? Compare with the ledgers written.
curl -s localhost:9464/metrics | grep 'ch_live_sink_ledgers_total{outcome="written"}'
```

- Counter advancing about as fast as `written`: systematic. Suspect a protocol upgrade that changed `TransactionMeta`, or an SDK bump.
- A few ledgers: a specific malformed transaction. Record the ledger sequences from the journal.

Mitigation (<= 15 min):

- [ ] **Systematic**: a decoder / SDK upgrade, not a host fault. `ch-live-catchup` cannot fix it (the ledgers exist) and a restart re-reads with the same decoder. Follow [decode-errors](ingestion.md#stellarindex_ingestion_decode_error) for the triage matrix; the 2026-07-09 Galexie P27-decode SEV is the reference case.
- [ ] **Isolated, lake path**: once the reader is fixed, re-extract the named ledgers into the lake (`stellarindex-ops ch-backfill -config PATH -from <ledger> -to <ledger>`; `ReplacingMergeTree` makes the rewrite idempotent), then replay any projected source that read them (`stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>`).
- [ ] **Isolated, substrate path**: once the reader is fixed, run `stellarindex-ops census-backfill -config PATH -from <ledger> -to <ledger> -write` over the named ledgers. It skips the same undercount and exits non-zero if any ledger in the range is still unreadable.
- [ ] **Verification**: the counter stops advancing and the WARN lines stop. For the substrate path, `ledger_ingest_log` holds a row for every ledger in the range:

```sh
sudo -u postgres psql -d stellarindex -tAc \
  "SELECT count(*) FROM ledger_ingest_log WHERE ledger_seq BETWEEN <from> AND <to>"
```

The alert clears on its own 30 minutes after the last increment.

Root cause: the first WARN line for the incident (ledger sequence and per-kind counts); the ledger's protocol version (`stellar.ledgers.protocol_version`) against the SDK's supported `TransactionMeta` versions; whether the lake path and the substrate path both advanced (they read the same meta, so one without the other points at the code that differs between `ExtractLedger` and `CensusLedger`).

False positives: none known. The counter is seeded at zero and a healthy archive does not advance it; `entry_meta_unsupported` in particular is unreachable on current production input.

## Related

- [ch-schema-restore](ch-schema-restore.md): the lake's schema+state backup, and the other ClickHouse alert family.
- [exporter-down](meta.md#stellarindex_redis_exporter_down): the same blindness pattern for exporters that run as separate processes.
- [projector-lag](projector.md#stellarindex_projector_lag_high): what an unhealed lake gap does to the CH-fed projector.
- ADR-0034 (raw lake / served tier), ADR-0048 D4 (serving-query settings profile).

**`ch-live-sink`**

- [decode-errors](ingestion.md#stellarindex_ingestion_decode_error): per-source decode-regression triage.
- [sink-undrained-rows](ingestion-sink.md#stellarindex_ingestion_sink_undrained_rows): the served-tier (Postgres) twin of the drops alert: rows the pipeline sink's bounded shutdown drain abandoned while the ledger cursor had already advanced. Different remedy: re-derive from the lake, nothing heals it by timer.
- `ingestion.md#stellarindex_ingestion_all_sources_stopped`: the severe case where live ingest itself stops (the drops alert's path leaves live ingest healthy).
- `docs/adr/0041-ingest-durability-semantics.md` (why drops are by design, the heal contract); `docs/adr/0034-tiered-clickhouse-architecture.md` (the lake the live edge feeds).
- `internal/storage/clickhouse/live_sink.go` (non-blocking buffer/drop contract; the `Add`/`Flush` error path); `internal/storage/clickhouse/sink.go` (`LedgerExtract`'s `TxReadErrors` / `TxEventReadErrors` / `EntryMetaUnsupported`).
- `cmd/stellarindex-indexer/main.go`: the `ExtractLedger` error path and its sampled WARN; `recordCHLiveSinkUndercount` and `recordLedgerIngestCensusSkip`, the two undercount emitters.
- `internal/ops/ingest/census_backfill.go`: the offline substrate writer, which fails its run on the same undercount rather than emitting this metric.
