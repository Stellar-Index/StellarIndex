---
title: Runbook — ledger-ingest-stalled
last_verified: 2026-09-03
status: current
severity: P1
---

# Runbook — `stellarindex_ingestion_ledger_stalled`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_ledger_stalled` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`; `severity: page`, `for: 5m` over a 5-minute flat window ⇒ ~10 min to page) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`. |
| Typical MTTR | 10–30 min |
| Impact | No ledger has been ingested for ≥ 10 min. Every on-chain surface — trades, protocol events, supply, coverage, the explorer — is frozen at the last committed ledger, and the freeze widens for as long as this is unfixed. Off-chain prices (CEX/FX) keep updating, so the API looks partly alive. |

## Why this exists

`stellarindex_ingestion_all_sources_stopped` is NOT the cover for a lake
outage on this deployment, though the indexer unit file claimed it was
until 2026-09-03. The CEX/FX connectors run inside the **same binary** as
the Galexie→dispatcher path, so `sum(rate(source_events_total[5m]))`
keeps moving while no ledger is being read at all. Two more changes
closed the remaining paths: #371 F3 gave the live-tail read a ~5-minute
retry budget (the process no longer exits promptly), and
`StartLimitBurst=60 / 15min` means a ~5min10s start cycle can never park
the unit, so `stellarindex_systemd_unit_failed` (a **ticket**, 15 m)
cannot fire for it either. `stellarindex_ingestion_cursor_stuck` cannot
fire for this cursor at all — it joins `on (source)
stellarindex_source_enabled`, and `ledgerstream` is a cursor namespace,
not a configured source, so no such series exists.

This alert watches the one gauge that means "a ledger was fully
processed AND its cursor row committed"
(`cmd/stellarindex-indexer/main.go`, `processAndPersistCursor`), so it
covers the whole path — lake read, dispatch, sink, Postgres commit —
rather than any single cause.

## Symptoms

- `stellarindex_cursor_last_ledger{source="ledgerstream"}` flat for
  ≥ 10 min, or the series absent entirely (a restart that never commits
  a ledger never creates it).
- `/v1/coverage` and the explorer's ledger badge stop advancing; price
  endpoints fed by CEX/FX keep updating, which is what makes this easy
  to misread as healthy.
- `stellarindex_ingestion_all_sources_stopped` is **quiet** — expected,
  not reassuring.

## Quick diagnosis (≤ 5 min)

```sh
# Is the process alive, or restart-looping on a dependency?
ssh root@136.243.90.96 'systemctl status stellarindex-indexer --no-pager | head -20'
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 --no-pager | tail -60'

# Is the lake readable, and is Galexie still writing?
ssh root@136.243.90.96 'mc ls local/galexie-live | tail'
ssh root@136.243.90.96 'systemctl is-active minio galexie'

# Where is the committed cursor vs the bucket?
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT source, sub, last_ledger, updated_at FROM ingestion_cursors WHERE source = 'ledgerstream'\""
```

Key signals:

- **MinIO/Galexie down or unreachable** → lake outage. The indexer is
  burning its retry budget; expect `SignatureDoesNotMatch` (credential
  drift) or connection errors in the journal.
- **`stellarindex_ledgerstream_live_start_retries_total` climbing** →
  same conclusion, without an ssh. That counter moves only when the live
  tail cannot OPEN the lake (datastore or schema unreadable), so a
  climbing value confirms lake reachability as the cause and a flat one
  rules it out. Nothing is being skipped while it climbs: the retry
  re-issues the identical range and the cursor is written from the
  callback, which has not run.
- **Lake healthy, cursor flat, no restarts** → the dispatch goroutine is
  wedged or the sink is blocked. Check
  `stellarindex_decoder_panics_total` (a recovered decoder panic that
  left a lock held wedges the walker) and Postgres write health
  (`stellarindex_postgres_ping_failure_streak`,
  `stellarindex_trade_insert_buffer_depth`).
- **Unit restart-looping every ~5 min** → a permanently broken
  dependency or config. The unit will NOT park in `failed` (see "Why
  this exists"), so systemd state is not the signal here.
- **Series absent + unit inactive/failed** → the process is not running;
  `systemctl status` gives the reason.

## Mitigation (≤ 15 min)

- [ ] Step 1 — fix the upstream first if it is down: MinIO, then
      Galexie. Nothing the indexer does helps while the lake is
      unreadable; it resumes from its committed cursor on its own.
      Credential drift is the usual r1 cause — see
      `docs/operations/runbooks/all-ingestion-down.md`.
- [ ] Step 2 — if the lake is healthy and the cursor is still flat,
      capture the journal (`journalctl -u stellarindex-indexer -n 1000
      --no-pager > /tmp/indexer-stall.log`) **before** restarting; a
      wedge leaves no trace after the restart.
      `ssh root@136.243.90.96 systemctl restart stellarindex-indexer`.
- [ ] Step 3 — if Postgres is the blocker (commit failures, locks), work
      that incident first; the cursor is committed in the same path.
- [ ] Verification: `stellarindex_cursor_last_ledger{source="ledgerstream"}`
      climbs again within ~1 min of the fix, and this alert clears about
      10 min after that (5m window + `for: 5m`).

## Root cause analysis

For the postmortem, gather: the indexer journal across the stall window;
`ingestion_cursors` before and after; the MinIO/Galexie unit states and
their own logs; `stellarindex_ledgerstream_tier_read_total` by outcome;
and the gap the stall left — `stellarindex-ops detect-gaps`, then the
ADR-0033 verdict once ingest has caught up.

## Known false-positive patterns

- **A deploy or host reboot longer than ~10 min.** Legitimate, and the
  page is arguably correct: ingest really is stopped. Silence it for the
  window rather than widening the rule.
- **A network with no live tail** (an indexer parked on a bounded
  backfill range that has finished). The gauge stops advancing because
  there is nothing left to ingest. This does not apply to r1, which
  always live-tails.
- **The absent branch is fleet-wide.** On a multi-host deployment
  `absent_over_time` only fires when EVERY indexer's series is gone; a
  single dead host is caught by the delta branch while its series
  survives, and by `stellarindex_systemd_unit_failed` after that.

## Related

- [all-ingestion-down.md](all-ingestion-down.md) — the sibling page for
  "no events from ANY source"; that one covers the CEX/FX side going
  quiet too, this one covers the ledger path specifically.
- [cursor-stuck.md](cursor-stuck.md) — the per-SOURCE cursor ticket. It
  cannot fire for `source="ledgerstream"` (no `source_enabled` series to
  join against), which is the gap this page fills.
- [systemd-unit-failed.md](systemd-unit-failed.md) — ticket, 15 m; will
  not fire while the unit is restart-looping inside its StartLimit
  budget.
- Implementation: `cmd/stellarindex-indexer/main.go`
  (`processAndPersistCursor`, `recordCursorMetric`),
  `internal/ledgerstream/`.

## Changelog

- 2026-09-03 — initial version, with the alert (RV2 #5: the F3 retry
  budget plus the relaxed StartLimit left a MinIO outage page-free, and
  the unit file named a cover that cannot fire for it).
- 2026-09-03 — added the
  `stellarindex_ledgerstream_live_start_retries_total` signal. The F3
  budget covered the WALK only; the datastore open + schema load ran
  before it, un-retried, so a lake outage present at process start
  failed in ~1s and the "~5min10s start cycle" above did not hold for
  the one fault it was written about. The retry now covers the open too,
  and this counter is what makes the resulting stall visible.
