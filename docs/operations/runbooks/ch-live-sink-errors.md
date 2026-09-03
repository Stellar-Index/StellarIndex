---
title: Runbook — ClickHouse live-sink erroring on ledger extracts
last_verified: 2026-09-03
status: ratified
severity: P3 (ticket)
---

# Runbook — `stellarindex_ingestion_ch_live_sink_errors`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_ch_live_sink_errors` (ticket) |
| Severity | ticket |
| Detected by | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="errored"}[30m]) > 0` in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml` |
| Typical MTTR | minutes (CH fault) to a deploy (decode fault) |
| Impact | Ledgers are missing from the ClickHouse lake's live edge. Served pricing is unaffected; the projector clamps to the contiguous watermark behind the hole, so lake-derived surfaces stop advancing until it is healed. |

## Why this exists

`outcome="errored"` was emitted from day one and matched by no alert
in either rule tree — both live-sink rules selected
`outcome="dropped"` only (#371 F6). A drop and an error are different
faults with different remedies, so folding them into the existing
page would have told a responder the wrong thing:

- **`dropped`** — the sink *accepted* the ledger and then shed it
  under buffer pressure. By design (ADR-0041), healed by
  `ch-live-catchup`. Owned by
  [ch-live-sink-drops](ch-live-sink-drops.md).
- **`errored`** — the ledger was never written at all. Two producers:
  1. `clickhouse.ExtractLedger` failed in the indexer's live read
     loop, so the sink was never offered the ledger. A
     `TransactionMeta`/LCM version break fails **every** ledger in
     lock-step; a single bad transaction fails one.
  2. The sink's own `Add`/`Flush` errored — a ClickHouse write-path
     fault (down, wedged, disk-full).

## Symptoms

- `stellarindex_ch_live_sink_ledgers_total{outcome="errored"}` climbing
  while `written` stalls (case 1 or 2), or while `written` continues
  normally (case 1 on isolated ledgers).
- A sampled `ch extract failed` WARN in the indexer journal naming the
  ledger sequence and the decode error.
- Downstream, if it persists: the projector's watermark stops
  advancing and `/v1/coverage` reports `complete=false` for the window.

## Quick diagnosis (≤ 5 min)

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

- Errors on **every** ledger + a decode message → case 1, systematic.
  This is an upstream protocol/SDK break, not a host fault.
- Errors on **isolated** ledgers → case 1, single-transaction.
- No `ch extract` lines at all → case 2, ClickHouse write path.

## Mitigation (≤ 15 min)

- [ ] **Case 2 (CH fault)** — restore ClickHouse (restart, free disk),
      then let the heal path run: `systemctl start ch-live-catchup.service`.
      Watch `journalctl -u ch-live-catchup.service -f`.
- [ ] **Case 1, isolated** — run the heal pass as above. A re-read that
      succeeds closes the hole; if the same ledger fails again the
      decode fault is deterministic, so treat it as systematic.
- [ ] **Case 1, systematic** — `ch-live-catchup` re-extracts with the
      SAME decoder, so it cannot heal this. Do not loop on it. Identify
      the meta version from the journal message, then treat it as a
      decoder / SDK upgrade — the 2026-07-09 Galexie P27-decode SEV is
      the reference case. [decode-errors](decode-errors.md) carries the
      per-source decode-regression triage matrix.
- [ ] **Verification** — the counter stops advancing, and
      `stellar.ledgers` is contiguous across the affected range:

```sh
clickhouse-client --port 9300 -q "
  SELECT count() FROM (
    SELECT ledger_seq, ledger_seq - lagInFrame(ledger_seq) OVER (ORDER BY ledger_seq) AS d
    FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= <from>)
  ) WHERE d > 1"
```

The alert clears on its own 30 minutes after the last error.

## Root cause analysis

- The indexer journal lines around the first error (they carry the
  ledger sequence and the raw decode error).
- `SELECT * FROM system.errors` on ClickHouse for case 2.
- Whether `ch-live-catchup` healed the range, and how many ranges it
  had to re-backfill — its output is one line per range.

## Known false-positive patterns

- A ClickHouse restart during a deploy produces a short burst of
  `errored` while the sink reconnects. It self-heals; the 15-minute
  `for` absorbs a normal restart, so a firing alert means it did not.

## Related

- [ch-live-sink-drops](ch-live-sink-drops.md) — the sibling outcome on
  the same counter: load shed by a sink that *did* see the ledger.
  Different remedy; check which outcome fired before acting.
- `docs/adr/0034-clickhouse-raw-lake.md` — the lake the live edge feeds.
- `docs/adr/0041-ingest-durability-semantics.md` — the drop/heal
  contract this alert sits beside.
- `internal/storage/clickhouse/live_sink.go` — the sink's `Add`/`Flush`
  error path (producer 2).
- `cmd/stellarindex-indexer/main.go` — the `ExtractLedger` error path
  and its sampled WARN (producer 1).

## Changelog

- 2026-09-03 — initial version, added with the alert (#371 F6).
