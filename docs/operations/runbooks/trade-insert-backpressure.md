---
title: Runbook — trade sink retrying under backpressure
last_verified: 2026-07-06
status: ratified
severity: P3 (ticket)
---

# Runbook — `stellarindex_ingestion_trade_insert_backpressure`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_trade_insert_backpressure` |
| Severity | P3 (ticket) |
| Detected by | `sum(rate(stellarindex_trade_insert_retries_total{outcome="retry"}[5m])) > 0` for 10 min (`deploy/monitoring/rules/ingestion.yml` + `configs/prometheus/rules.r1/ingestion.yml`) |
| Typical MTTR | minutes (Postgres restart / pressure passes) |
| Impact | The served tier is FROZEN: the on-chain ledger cursor is held and no new trades/prices land while this fires. **No data is lost** — on-chain trades block-and-retry (cursor gating), external CEX/FX trades buffer in memory. External price freshness degrades if it persists. |

## Why this exists

2026-07-06 incident: during a 17-minute Postgres outage the trade sink
DROPPED writes (`insert trade failed` / `connection refused`) while the
ledger cursor kept advancing — a ~205-ledger sdex hole (healed from the
lake) plus unrecoverable CEX drops. The sink now classifies the failure
([`timescale.IsInfraError`](../../../internal/storage/timescale/errors.go))
and, on an infrastructure fault, RETRIES with backpressure instead of
dropping. This alert is the visible signal that the retry path is active
— i.e. Postgres is unreachable and ingest is intentionally stalled
rather than losing data (ADR-0041).

## Symptoms

- `stellarindex_trade_insert_retries_total{outcome="retry"}` climbing;
  `outcome="recovered"` flat (hasn't recovered yet).
- The on-chain ledger cursor (`stellarindex_cursor_last_ledger`) is not
  advancing — `stellarindex_ingestion_cursor_stuck` may also fire.
- `stellarindex_trade_insert_buffer_depth` climbing (external CEX/FX
  trades queuing in the bounded retry buffer).
- Indexer journal: repeated `infrastructure fault on trade insert —
  retrying with backpressure`.

## Quick diagnosis (≤ 5 min)

```sh
# Is Postgres actually up? (the near-universal cause)
systemctl status postgresql
sudo -u postgres psql -d stellarindex -c 'SELECT 1'

# Retry vs recovery mix + external buffer depth
curl -s localhost:9464/metrics | grep -E 'trade_insert_retries_total|trade_insert_buffer_depth'

# Indexer's own view
journalctl -u stellarindex-indexer --since -15m | grep -iE 'backpressure|connection refused|abandoned'
```

If `SELECT 1` fails → it's a Postgres outage (expected trigger); go to
Mitigation. If Postgres is healthy but retries persist → suspect
connection-pool exhaustion (`too_many_connections`, SQLSTATE 53300) or
a network partition to the DB host.

## Mitigation (≤ 15 min)

- [ ] Bring Postgres back (restart the service / clear disk / restore
      the network path). This is the only real fix — the sink recovers
      on its own the instant writes succeed.
- [ ] If the cause is pool exhaustion, find and kill the hog:
      `SELECT pid, state, query_start, left(query,80) FROM pg_stat_activity ORDER BY query_start;`
- [ ] Verification: within ~1 retry interval of Postgres recovering,
      `stellarindex_trade_insert_retries_total{outcome="recovered"}`
      increments, the cursor resumes advancing, and
      `stellarindex_trade_insert_buffer_depth` drains to 0. The alert
      clears within its 10 min window.

## Root cause analysis

- **Nothing to re-derive for on-chain trades** — they were held in
  memory and landed on recovery. If the indexer was HARD-restarted while
  this fired (not a graceful shutdown), the in-flight buffer is lost; but
  those ledgers are re-derivable from the CH lake and the ADR-0033
  completeness verdict will flag any residue. A graceful shutdown logs
  the exact abandoned ledger range at ERROR (`abandoned on shutdown …
  re-derive this ledger range`).
- **External (CEX/FX) trades** dropped only if the outage outlasted the
  bounded buffer (`stellarindex_source_insert_errors_total{kind="dropped"}`
  bumped). Those are vendor-refillable via the connector backfill path.

## Related

- [`insert-errors`](insert-errors.md) — fires on GENUINE loss
  (`kind=trade` data faults, `kind=dropped` external overflow), the
  complement of this alert.
- [`cursor-stuck`](cursor-stuck.md) — the cursor-not-advancing symptom
  this backpressure produces on purpose.
- [`all-ingestion-down`](all-ingestion-down.md) — if the outage is total
  and prolonged, the SEV-1 page.
- [ADR-0041](../../adr/0041-ingest-durability-semantics.md) — the
  durability semantics this path implements.
