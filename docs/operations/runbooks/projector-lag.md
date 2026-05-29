---
title: Runbook — projector-lag
last_verified: 2026-05-29
status: ratified
severity: P3
---

# Runbook — `ratesengine_projector_lag_high` / `ratesengine_projector_error_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `ratesengine_projector_lag_high`, `ratesengine_projector_error_rate_high` |
| Severity | P3 (ticket) — Phase 3 parallel-mode means the dispatcher's per-source sink is still primary; lag is visibility-only until Phase 4. Re-promote to P2 once `[ingestion.persist_per_source]=false` (Phase 4). |
| Detected by | Prometheus rules in `deploy/monitoring/rules/projector.yml` + `configs/prometheus/rules.r1/projector.yml` |
| Typical MTTR | 30 min |
| Impact | Per-source projection tables (`trades`, `blend_*`, `phoenix_*`, `cctp_events`, …) increasingly diverge from the raw `soroban_events` store. In Phase 3 the dispatcher's per-source sink is still primary, so customer-facing rows are unaffected — but flipping the writer (Phase 4) is unsafe while lag is unbounded. |

## Symptoms

- `ratesengine_projector_lag_ledgers{source="<name>"}` > 256 for 10+ min.
- `ratesengine_projector_runs_total{outcome="error"}` rate > 0.05/s sustained.
- `ratesengine_projector_cycle_duration_seconds_bucket{source="<name>"}` p99 > 30 s (the per-cycle timeout is 60 s).

## Quick diagnosis (≤ 5 min)

```sh
# Read the per-source projector cursor — should be close to the
# ledgerstream tip.
ssh root@136.243.90.96 'psql -U ratesengine -d ratesengine -c \
  "SELECT source, sub_source, last_ledger, updated_at FROM source_cursors \
   WHERE source = '"'"'projector'"'"' ORDER BY sub_source"'

# Compare against the live ledgerstream tip.
ssh root@136.243.90.96 'psql -U ratesengine -d ratesengine -c \
  "SELECT last_ledger FROM source_cursors \
   WHERE source = '"'"'ledgerstream'"'"' AND sub_source = '"'"''"'"'"'

# Tail the projector log for the lagging source.
ssh root@136.243.90.96 'journalctl -u ratesengine-indexer -n 200 | grep "projector cycle" | grep <source>'
```

If the projector cursor isn't moving at all, jump to `Mitigation`. If
it's moving but slower than the live tip, this is honest catch-up after
an outage — let it run unless lag exceeds a few hours.

## Mitigation (≤ 15 min)

- [ ] Step 1 — check that the dispatcher's per-source sink is still
      writing rows (Phase 3 parallel-mode safety net). If yes,
      customer impact is bounded; this alert is operational only.
- [ ] Step 2 — if `outcome="error"` rate is high, inspect log lines
      tagged `component=projector` for the failing source. Common
      causes: postgres connection saturation, downstream PK
      constraint failure on a malformed event, decoder panic.
- [ ] Step 3 — if the projector is wedged on one source, disable
      it via `[ingestion.projector] enabled = false` in `r1.toml` +
      `systemctl restart ratesengine-indexer.service`. Lag will
      reset on next start.
- [ ] Verification: `ratesengine_projector_lag_ledgers` drops below
      256 within 30 minutes after restart (or the alert clears).

## Root cause analysis

- `journalctl -u ratesengine-indexer | grep projector` for the lag
  episode — `projector cycle` lines log per-cycle `rows_scanned`,
  `events_emitted`, `decode_errors`, `lag_ledgers`, `elapsed`.
- `SELECT outcome, count(*) FROM ratesengine_projector_runs_total`
  in Prometheus for the failure-class breakdown.
- Capture a `pgrep -af ratesengine-indexer` + `pprof` heap if cycle
  durations are pathological.

## Known false-positive patterns

- During a fresh deploy the projector starts at `(projector, <source>)`
  cursor = 0 and catches up from the soroban-era genesis ledger. The
  alert's 10-minute `for:` window absorbs the cold-start; if it fires,
  raise to 30 min temporarily.
- After a soroban-events landing-zone backfill, projector lag spikes
  while it catches up to the newly-written rows. Same as cold-start —
  let it drain.

## Related

- ADR-0029 — soroban_events landing zone (the upstream raw store).
- ADR-0032 — per-source tables as projections (this runbook's parent).
- `internal/projector/` — the projector implementation.
- `docs/operations/runbooks/source-stopped.md` — adjacent surface:
  per-source ingest cadence alerts (about live-ingest writes, not
  projection).

## Changelog

- 2026-05-29 — initial draft (ADR-0032 Phase 3 rc.95).
