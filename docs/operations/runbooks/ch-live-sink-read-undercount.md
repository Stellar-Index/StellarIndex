---
title: Runbook — indexer read path undercounting ledgers
last_verified: 2026-09-23
status: ratified
severity: P3 (ticket)
---

# Runbook — `stellarindex_ingestion_ch_live_sink_read_undercount`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_ch_live_sink_read_undercount` (ticket) |
| Severity | ticket |
| Detected by | `increase(stellarindex_ch_live_sink_read_undercount_total[30m]) > 0` in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml` |
| Typical MTTR | one re-derive of the affected range (isolated) to a deploy (meta-version break) |
| Impact | The affected ledgers are in the ClickHouse lake with fewer transactions, contract events or entry changes than the chain holds, or have no `ledger_ingest_log` substrate row. Nothing downstream can tell the lake rows are short. |

## Why this exists

The indexer reads every ledger twice: `clickhouse.ExtractLedger` for the
lake's live edge and `dispatcher.CensusLedger` for the
`ledger_ingest_log` substrate record. Both count the transactions they
could not fully read, and until this counter each count reached only a
WARN log line. The two paths react differently, which the `kind`
label keeps apart:

- **`tx_read_errors`, `tx_event_read_errors`, `entry_meta_unsupported`:
  the lake path.** The ledger is still written, because lake
  contiguity is the coverage proof, but its events or entry changes
  are short. Its `stellar.ledgers` row exists, so the projector's
  watermark treats it as complete and `ch-live-catchup`, which re-fills
  MISSING ledgers, never revisits it. This is not the
  [ch-live-sink-errors](ch-live-sink-errors.md) case: there the ledger
  is absent.
- **`soroban_fee_meta_unsupported`: the lake path, fee columns.** A
  Soroban transaction's `TransactionMeta` version is past what the
  charged-fee read handles, so its `stellar.transactions` row carries
  0 for the non-refundable, refundable and rent fees.
- **`tx_read_errors_census`, `tx_event_read_errors_census`: the
  substrate path.** The indexer declines to write the
  `ledger_ingest_log` row, so a projection reconcile cannot pass
  against an undercount. That leaves a substrate gap.

The value added is the number of transactions affected, not a count
of ledgers.

## Symptoms

- `stellarindex_ch_live_sink_read_undercount_total` advancing for one
  or more `kind` values.
- Indexer journal WARNs: `ch live-sink: ledger extracted with read
  undercount` (lake path) or `ledger census read errors; skipping
  substrate record` (substrate path), each naming the ledger.
- A meta-version break: every ledger advances the counter, and the
  lake looks like a run of ledgers with no Soroban events.

## Quick diagnosis (≤ 5 min)

```sh
# Which kinds, and how fast?
curl -s localhost:9464/metrics | grep ch_live_sink_read_undercount_total

# Which ledgers? Each WARN names the ledger and the per-kind counts.
journalctl -u stellarindex-indexer --since -1h \
  | grep -E 'read undercount|census read errors'

# Every ledger, or isolated ones? Compare with the ledgers written.
curl -s localhost:9464/metrics | grep 'ch_live_sink_ledgers_total{outcome="written"}'
```

- Counter advancing about as fast as `written` → systematic. Suspect a
  protocol upgrade that changed `TransactionMeta`, or an SDK bump.
- A few ledgers → a specific malformed transaction. Record the ledger
  sequences from the journal.

## Mitigation (≤ 15 min)

- [ ] **Systematic** — this is a decoder / SDK upgrade, not a host
      fault. `ch-live-catchup` cannot fix it (the ledgers exist) and a
      restart re-reads with the same decoder. Follow
      [decode-errors](decode-errors.md) for the triage matrix; the
      2026-07-09 Galexie P27-decode SEV is the reference case.
- [ ] **Isolated, lake path** — once the reader is fixed, re-extract
      the named ledgers into the lake
      (`stellarindex-ops ch-backfill -config PATH -from <ledger> -to <ledger>`;
      `ReplacingMergeTree` makes the rewrite idempotent), then replay
      any projected source that read them
      (`stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>`).
- [ ] **Isolated, substrate path** — once the reader is fixed, run
      `stellarindex-ops census-backfill -config PATH -from <ledger> -to <ledger> -write`
      over the named ledgers. It skips the same undercount and exits
      non-zero if any ledger in the range is still unreadable.
- [ ] **Verification** — the counter stops advancing and the WARN lines
      stop. For the substrate path, `ledger_ingest_log` holds a row for
      every ledger in the range:

```sh
sudo -u postgres psql -d stellarindex -tAc \
  "SELECT count(*) FROM ledger_ingest_log WHERE ledger_seq BETWEEN <from> AND <to>"
```

The alert clears on its own 30 minutes after the last increment.

## Root cause analysis

- The first WARN line for the incident: it carries the ledger sequence
  and the per-kind counts.
- The ledger's protocol version (`stellar.ledgers.protocol_version`)
  against the SDK's supported `TransactionMeta` versions.
- Whether the lake path and the substrate path both advanced: they read
  the same meta, so one without the other points at the code that
  differs between `ExtractLedger` and `CensusLedger`.

## Known false-positive patterns

- None known. The counter is seeded at zero and a healthy archive does
  not advance it; `entry_meta_unsupported` in particular is unreachable
  on current production input.

## Related

- [ch-live-sink-errors](ch-live-sink-errors.md) — ledgers the lake
  never received, rather than ledgers it received short.
- [decode-errors](decode-errors.md) — per-source decode-regression
  triage.
- `internal/storage/clickhouse/sink.go` — `LedgerExtract`'s
  `TxReadErrors` / `TxEventReadErrors` / `EntryMetaUnsupported`.
- `cmd/stellarindex-indexer/main.go` — `recordCHLiveSinkUndercount` and
  `recordLedgerIngestCensusSkip`, the two emitters.
- `internal/ops/ingest/census_backfill.go` — the offline substrate
  writer, which fails its run on the same undercount rather than
  emitting this metric.

## Changelog

- 2026-09-23 — initial version, added with the alert.
