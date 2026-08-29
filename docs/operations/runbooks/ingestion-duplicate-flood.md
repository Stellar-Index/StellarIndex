---
title: Runbook — ingestion-duplicate-flood
last_verified: 2026-08-29
status: draft
severity: P2
---

# Runbook — `stellarindex_ingestion_duplicate_flood`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_duplicate_flood` |
| Severity | P2 (ticket) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ingestion.yml`. Both trees carry the same expr. |
| Typical MTTR | 30–90 min |
| Impact | Cursor advances but the trades hypertable falls behind. `/v1/price` returns stale-but-flagged data; freshness SLA fails. No data loss (events were never persisted from this code path; if the source produces fresh events again, they'll land). |

## Symptoms

- `stellarindex_trade_insert_outcome_total{source=...,outcome="duplicate"}` > 0.5/sec for ≥10 min
- `stellarindex_trade_insert_outcome_total{source=...,outcome="new"}` == 0 over the same window
- `stellarindex_source_events_total{source=...}` still climbing — events ARE being decoded
- `stellarindex_cursor_last_ledger{source="ledgerstream"}` still advancing
- `psql trades` shows `max(ts) WHERE source = <X>` frozen for hours
- `/v1/markets?source=<X>` returns `last_trade_at` matching the frozen `max(ts)`

The combination is the diagnostic signature: cursor + decoder healthy, persistence apparently working (no errors), but no INSERT is landing a new row. Live r1 evidence on 2026-05-28: 157 SDEX dupes/min, `max(ts) = 14:29:17 UTC` for 11 hours.

### `outcome="duplicate"` is a CONFLATION — read it carefully

The trade upsert is **not** `ON CONFLICT DO NOTHING` any more. The
INV-3 keystone fix (migration 0109) made it
`ON CONFLICT … DO UPDATE … WHERE trades.derive_generation <= EXCLUDED.derive_generation`,
so the counter's `duplicate` label now covers THREE different
outcomes that all score "no fresh row inserted":

1. a true duplicate (the row was already there, unchanged);
2. a generation-guarded **corrective UPDATE** that rewrote an existing
   row in place — i.e. the system working exactly as intended;
3. a guard-**skipped** write (a lower generation refusing to revert a
   higher-generation correction).

**Operational consequence: a running corrective re-derive produces
this alert's exact signature** — `duplicate` climbing with `new` at
zero — because a re-derive legitimately updates rows in place rather
than inserting them. Before treating a firing as a stuck cursor,
check whether a heavy job is in flight:

```sh
ssh root@136.243.90.96 'systemctl list-units "run-heavy-job*" --all --no-pager; pgrep -a stellarindex-ops'
```

If one is, the alert is expected for the duration of the job and the
freshness SQL below (step 2) is the signal that matters, not the
counter.

## Quick diagnosis (≤ 5 min)

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
   if a `stellarindex-ops backfill` process is running.

## Remediation

For cause 1 (most common): identify the gap, run a targeted
backfill. The trades hypertable's PK is `(source, ledger, tx_hash,
op_index, ts)` so a backfill is idempotent — re-walking a range
that already has data is harmless.

```sh
# Determine the gap: lowest ledger to backfill is one past max_ts;
# upper ledger is the current cursor.
sudo -u postgres psql stellarindex -t -c "SELECT max(ledger) FROM trades WHERE source = 'sdex';"
# vs cursor_last_ledger metric.

# Run the targeted backfill. Flags are -source (singular, comma-
# separated); there is no -sources and no -parallel on this
# subcommand. Heavy one-shots on r1 ALWAYS go through the wrapper
# (CLAUDE.md "Heavy one-shot jobs on r1"), one at a time.
sudo /usr/local/sbin/run-heavy-job.sh dupflood-backfill \
  /usr/local/bin/stellarindex-ops backfill \
    -config /etc/stellarindex.toml \
    -from <max_ledger+1> -to <current_cursor> \
    -source sdex,aquarius,soroswap,phoenix,comet
```

`backfill` **refuses** any source that isn't `BackfillSafe` in
`internal/sources/external/registry.go` — for on-chain Soroban
sources that gate is the per-WASM-hash audit. Add `-dry-run` first if
you want the plan without the write. For a *projected* (Soroban-derived)
source the ADR-0032 catch-up path is
`stellarindex-ops projector-replay -source <name> -from <ledger>`
(or `projected-rebuild` beyond ~1M ledgers), not `backfill`.

For cause 2: restart the indexer to clear any goroutine leak.

```sh
systemctl restart stellarindex-indexer
```

For cause 3: identify the looping backfill via `ps`, decide whether
to stop it (`systemctl stop` for service-managed, `kill` for ad-hoc).

## Verification

After remediation, the metric should flip back:

```sh
# Wait at least 2× scrape interval (60s default) then check:
curl -sS localhost:9464/metrics | grep stellarindex_trade_insert_outcome_total
# outcome=new should be climbing again.

# And the trades table should accept fresh rows:
sudo -u postgres psql stellarindex -c "
  SELECT max(ts) AT TIME ZONE 'UTC' FROM trades WHERE source = 'sdex';"
# Should be within the last few minutes.
```

The alert will clear after `for: 10m` elapses with healthy
`outcome=new` rates.

## Related

- `internal/storage/timescale/trades.go:Store.InsertTrade` — where
  the outcome metric is emitted.
- `docs/reference/metrics/README.md#stellarindex_trade_insert_outcome_total` — metric reference.
- F-0028 audit finding (audit-2026-05-26) for the original
  observation of soroban_events ingest tip lag, similar shape.
- F-0020 audit finding for the postgres back-pressure cause.
- `trade-insert-backpressure.md` — the ADR-0041 sibling; since
  2026-07-06 infrastructure faults retry rather than drop, so a
  back-pressure episode is visible there before it can show up here.

## Changelog

- 2026-05-28 — initial draft alongside the
  `stellarindex_trade_insert_outcome_total` wiring.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). `ON CONFLICT DO NOTHING` is no longer the upsert shape
  (INV-3 / migration 0109 made it a generation-guarded `DO UPDATE`),
  so `outcome="duplicate"` conflates duplicate / corrective-update /
  guard-skipped and an in-flight corrective re-derive reproduces this
  alert's signature exactly — that check is now step 0 of triage. The
  remediation command used `-sources` and `-parallel`, neither of
  which exists on `backfill`; rewritten as `-source` under
  `run-heavy-job.sh` with the BackfillSafe refusal and the
  projector-replay alternative called out.
