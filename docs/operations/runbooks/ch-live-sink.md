---
title: Runbook — ClickHouse live-sink
last_verified: 2026-10-06
status: living
severity: P3 (ticket); P2 page when drops sustain 1h
---

# Runbook — ClickHouse live-sink alerts

The indexer's real-time ClickHouse dual-sink (ADR-0034 / ADR-0041) writes each ledger twice: `clickhouse.ExtractLedger` feeds the lake's live edge and `dispatcher.CensusLedger` feeds the `ledger_ingest_log` substrate record. All alerts here read `stellarindex_ch_live_sink_*` counters, rules in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml`. Served pricing is unaffected by drops and errors (the Postgres served tier is written by an independent path). Under `read_undercount` it is not: the projector reads CH `contract_events`, so projected Soroban sources (soroswap, aquarius, phoenix), and so DEX pricing, under-read the short ledgers until replayed.

Check which `outcome`/counter fired before acting; the remedies differ:

- **drop**: the sink *accepted* the ledger and then shed it under buffer pressure. By design, healed by `ch-live-catchup`.
- **error**: the ledger was never written (extract failed, or the sink's `Add`/`Flush` failed). A systematic extract break is NOT healed by the catch-up timer.
- **read undercount**: the ledger was written short (fewer txs/events/entry changes than the chain holds) or has no substrate row. Its `stellar.ledgers` row exists, so `ch-live-catchup` (which re-fills MISSING ledgers) never revisits it.

## At a glance

- [`stellarindex_ingestion_ch_live_sink_drops`](#stellarindex_ingestion_ch_live_sink_drops) (ticket) / [`stellarindex_ingestion_ch_live_sink_drops_sustained`](#stellarindex_ingestion_ch_live_sink_drops) (page at 1h)
- [`stellarindex_ingestion_ch_live_sink_errors`](#stellarindex_ingestion_ch_live_sink_errors) (ticket)
- [`stellarindex_ingestion_ch_live_sink_read_undercount`](#stellarindex_ingestion_ch_live_sink_read_undercount) (ticket)

## stellarindex_ingestion_ch_live_sink_drops

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
3. Force a heal pass once CH is healthy: `systemctl start ch-live-catchup.service`, then confirm the lake tail is contiguous: `stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <pre-gap ledger>` (any strict source works; substrate is global).

Escalate: sustained page + heal path cannot catch up: treat as SEV-2 (lake tail integrity), follow `docs/operations/sev-playbook.md`.

Post-mortem notes from prior firings: none yet (alert added 2026-07-02, ADR-0041).

## stellarindex_ingestion_ch_live_sink_errors

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
- [ ] **Case 1, systematic**: `ch-live-catchup` re-extracts with the SAME decoder, so it cannot heal this. Do not loop on it. Identify the meta version from the journal message, then treat it as a decoder / SDK upgrade; the 2026-07-09 Galexie P27-decode SEV is the reference case. [decode-errors](decode-errors.md) carries the per-source decode-regression triage matrix.
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

## stellarindex_ingestion_ch_live_sink_read_undercount

Ticket. Detected by `increase(stellarindex_ch_live_sink_read_undercount_total[30m]) > 0`. Typical MTTR: one re-derive of the affected range (isolated) to a deploy (meta-version break).

Impact: the affected ledgers are in the ClickHouse lake with fewer transactions, contract events or entry changes than the chain holds, or have no `ledger_ingest_log` substrate row. Nothing downstream can tell the lake rows are short.

Why it exists: both read paths count the transactions they could not fully read, and until this counter each count reached only a WARN log line. The two paths react differently, which the `kind` label keeps apart:

- **`tx_read_errors`, `tx_event_read_errors`, `entry_meta_unsupported`: the lake path.** The ledger is still written, because lake contiguity is the coverage proof, but its events or entry changes are short. Its `stellar.ledgers` row exists, so the projector's watermark treats it as complete and `ch-live-catchup` never revisits it. This is not the errors case above: there the ledger is absent.
- **`soroban_fee_meta_unsupported`: the lake path, fee columns.** A Soroban transaction's `TransactionMeta` version is past what the charged-fee read handles, so its `stellar.transactions` row carries 0 for the non-refundable, refundable and rent fees.
- **`evicted_keys_unreadable`: the lake path, evictions.** The ledger's evicted-keys list could not be read, so its `removed` rows are missing and every entry evicted at that ledger still reads as live in `stellar.ledger_entries_current`. The log line carries the ledger. This kind adds 1 per ledger, not a transaction count.
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

- [ ] **Systematic**: a decoder / SDK upgrade, not a host fault. `ch-live-catchup` cannot fix it (the ledgers exist) and a restart re-reads with the same decoder. Follow [decode-errors](decode-errors.md) for the triage matrix; the 2026-07-09 Galexie P27-decode SEV is the reference case.
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

- [decode-errors](decode-errors.md): per-source decode-regression triage.
- [sink-undrained-rows](ingestion-sink.md#stellarindex_ingestion_sink_undrained_rows): the served-tier (Postgres) twin of the drops alert: rows the pipeline sink's bounded shutdown drain abandoned while the ledger cursor had already advanced. Different remedy: re-derive from the lake, nothing heals it by timer.
- `all-ingestion-down.md`: the severe case where live ingest itself stops (the drops alert's path leaves live ingest healthy).
- `docs/adr/0041-ingest-durability-semantics.md` (why drops are by design, the heal contract); `docs/adr/0034-tiered-clickhouse-architecture.md` (the lake the live edge feeds).
- `internal/storage/clickhouse/live_sink.go` (non-blocking buffer/drop contract; the `Add`/`Flush` error path); `internal/storage/clickhouse/sink.go` (`LedgerExtract`'s `TxReadErrors` / `TxEventReadErrors` / `EntryMetaUnsupported`).
- `cmd/stellarindex-indexer/main.go`: the `ExtractLedger` error path and its sampled WARN; `recordCHLiveSinkUndercount` and `recordLedgerIngestCensusSkip`, the two undercount emitters.
- `internal/ops/ingest/census_backfill.go`: the offline substrate writer, which fails its run on the same undercount rather than emitting this metric.
