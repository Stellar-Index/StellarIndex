---
title: Runbook — ClickHouse live-sink dropping ledger extracts
last_verified: 2026-07-02
status: ratified
severity: P3 (ticket) / P2 when sustained 1h (page)
---

# Runbook — `stellarindex_ingestion_ch_live_sink_drops`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_ch_live_sink_drops` (ticket) / `…_drops_sustained` (page) |
| Severity | ticket; page at 1h sustained |
| Detected by | `increase(stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}[10m]) > 0` |
| Typical MTTR | minutes (CH restart / pressure passes) |
| Impact | The certified-lake TAIL lags live; served pricing unaffected. Healed by `ch-live-catchup`; if drops outpace the heal, the ADR-0033 substrate claim for recent ledgers degrades until caught up. |

## Alert description

The indexer's real-time ClickHouse dual-sink (ADR-0034 / ADR-0041) is
non-blocking BY DESIGN: under buffer pressure it drops the whole
ledger extract (`outcome="dropped"`) instead of stalling live ingest.
Drops are normal in rare bursts and are healed by the
`ch-live-catchup` timer, which re-extracts missing lake ledgers from
Galexie. This alert fires when dropping is *continuous* — the heal
path is being exercised abnormally (ticket), or losing the race
(page at 1h).

## When this fires

- ClickHouse is down, wedged, or slow (merges, disk, the CS-112
  no-backup lake is also the one filling the disk).
- The sink buffer is undersized for a ledger-volume burst.
- The indexer host is CPU/IO-starved so the sink's writer goroutine
  can't drain.

## What it means for users

Nothing immediately — the served tier (Postgres) is written by an
independent path. The risk horizon is the completeness verdict and
lake-derived surfaces (supply for unwatched tokens, explorer lake
reads) for the affected ledger range until catch-up completes.

## How to investigate

```sh
# Is CH alive + how far behind is the lake tail?
curl -s 'http://127.0.0.1:8123/' --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'
sudo -u postgres psql -d stellarindex -c "SELECT last_ledger FROM ingest_cursors WHERE name='ledgerstream'"

# Drop rate + sink outcome mix
curl -s localhost:9464/metrics | grep ch_live_sink_ledgers_total

# Is the heal timer running?
systemctl status ch-live-catchup.timer ch-live-catchup.service
journalctl -u ch-live-catchup.service --since -2h | tail -50

# CH pressure
curl -s 'http://127.0.0.1:8123/' --data-binary "SELECT metric, value FROM system.metrics WHERE metric IN ('BackgroundMergesAndMutationsPoolTask','DelayedInserts')"
df -h /   # remember the CH-log root-fill incident (2026-06-11)
```

## How to mitigate

1. If CH is down/wedged: restart `clickhouse-server`; watch the root
   filesystem (logs go to ZFS since 5dd6fcda, but verify).
2. If the sink buffer is saturated on bursts: raise the sink buffer
   (indexer config) and restart the indexer during a quiet window.
3. Force a heal pass once CH is healthy:
   `systemctl start ch-live-catchup.service`, then confirm the lake
   tail is contiguous:
   `stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <pre-gap ledger>`
   (any strict source works; substrate is global).

## How to escalate

Sustained page + heal path cannot catch up → treat as SEV-2 (lake
tail integrity), follow `docs/operations/sev-playbook.md`.

## Post-mortem notes from prior firings

- (none yet — alert added 2026-07-02, ADR-0041.)

## Related

- `docs/adr/0041-ingest-durability-semantics.md` — why drops are by
  design and what the heal contract is.
- `all-ingestion-down.md` — the severe case where live ingest itself
  stops (this alert's path leaves live ingest healthy).
- `internal/storage/clickhouse/live_sink.go` — the non-blocking
  buffer/drop contract.
