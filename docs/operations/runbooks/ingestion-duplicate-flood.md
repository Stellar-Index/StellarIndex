---
title: Runbook — ingestion-duplicate-flood
last_verified: 2026-05-28
status: draft
severity: P2
---

# Runbook — `stellaratlas_ingestion_duplicate_flood`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellaratlas_ingestion_duplicate_flood` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` + `configs/prometheus/rules.r1/ingestion.yml` |
| Typical MTTR | 30–90 min |
| Impact | Cursor advances but the trades hypertable falls behind. `/v1/price` returns stale-but-flagged data; freshness SLA fails. No data loss (events were never persisted from this code path; if the source produces fresh events again, they'll land). |

## Symptoms

- `stellaratlas_trade_insert_outcome_total{source=...,outcome="duplicate"}` > 0.5/sec for ≥10 min
- `stellaratlas_trade_insert_outcome_total{source=...,outcome="new"}` == 0 over the same window
- `stellaratlas_source_events_total{source=...}` still climbing — events ARE being decoded
- `stellaratlas_cursor_last_ledger{source="ledgerstream"}` still advancing
- `psql trades` shows `max(ts) WHERE source = <X>` frozen for hours
- `/v1/markets?source=<X>` returns `last_trade_at` matching the frozen `max(ts)`

The combination is the diagnostic signature: cursor + decoder healthy, persistence apparently working (no errors), but every INSERT is a no-op via `ON CONFLICT DO NOTHING`. Live r1 evidence on 2026-05-28: 157 SDEX dupes/min, `max(ts) = 14:29:17 UTC` for 11 hours.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@<host>

# 1. Confirm the duplicate vs new split.
curl -sS localhost:9464/metrics | grep stellaratlas_trade_insert_outcome_total

# 2. Confirm the trades hypertable is actually stale.
sudo -u postgres psql stellaratlas -c "
  SELECT source, max(ts) AT TIME ZONE 'UTC' AS max_ts,
         count(*) FILTER (WHERE ts > NOW() - INTERVAL '1 hour') AS rows_last_hour
    FROM trades
   WHERE ts > NOW() - INTERVAL '24 hours'
   GROUP BY source
   ORDER BY max_ts DESC;"

# 3. Confirm the indexer cursor IS advancing.
curl -sS localhost:9464/metrics | grep cursor_last_ledger

# 4. Look at the ingestion_cursors table for stuck backfills shadowing live ingest.
sudo -u postgres psql stellaratlas -c "
  SELECT source, sub_source, last_ledger, last_updated
    FROM ingestion_cursors
   ORDER BY last_updated DESC
   LIMIT 20;"
```

## Likely causes

1. **Cursor jumped past data without persisting events.** A
   back-pressure event (postgres outage, slow sink) caused
   ProcessLedger to return cleanly because the channel buffer
   absorbed the events, but the events were never drained before
   shutdown — they got dropped. The cursor was upserted regardless.
   Subsequent live walking starts past the gap; the events for
   that gap range have to be backfilled.
2. **Live indexer running with a stale event channel from a prior
   process.** Extremely unlikely under the current architecture
   (single sink goroutine) but possible if a refactor introduces a
   leak.
3. **A backfill process replaying the same range repeatedly.**
   Inspect `ingestion_cursors` for a `backfill` row whose
   `last_ledger` is below its sub_source's upper bound and check
   if a `stellaratlas-ops backfill` process is running.

## Remediation

For cause 1 (most common): identify the gap, run a targeted
backfill. The trades hypertable's PK is `(source, ledger, tx_hash,
op_index, ts)` so a backfill is idempotent — re-walking a range
that already has data is harmless.

```sh
# Determine the gap: lowest ledger to backfill is one past max_ts;
# upper ledger is the current cursor.
sudo -u postgres psql stellaratlas -t -c "SELECT max(ledger) FROM trades WHERE source = 'sdex';"
# vs cursor_last_ledger metric.

# Run the targeted backfill.
stellaratlas-ops backfill \
  -from <max_ledger+1> -to <current_cursor> \
  -sources sdex,aquarius,soroswap,phoenix,comet \
  -parallel 4 \
  -config /etc/stellaratlas.toml
```

For cause 2: restart the indexer to clear any goroutine leak.

```sh
systemctl restart stellaratlas-indexer
```

For cause 3: identify the looping backfill via `ps`, decide whether
to stop it (`systemctl stop` for service-managed, `kill` for ad-hoc).

## Verification

After remediation, the metric should flip back:

```sh
# Wait at least 2× scrape interval (60s default) then check:
curl -sS localhost:9464/metrics | grep stellaratlas_trade_insert_outcome_total
# outcome=new should be climbing again.

# And the trades table should accept fresh rows:
sudo -u postgres psql stellaratlas -c "
  SELECT max(ts) AT TIME ZONE 'UTC' FROM trades WHERE source = 'sdex';"
# Should be within the last few minutes.
```

The alert will clear after `for: 10m` elapses with healthy
`outcome=new` rates.

## Related

- `internal/storage/timescale/trades.go:Store.InsertTrade` — where
  the outcome metric is emitted.
- `docs/reference/metrics/README.md#stellaratlas_trade_insert_outcome_total` — metric reference.
- F-0028 audit finding (audit-2026-05-26) for the original
  observation of soroban_events ingest tip lag, similar shape.
- F-0020 audit finding for the postgres back-pressure cause.
