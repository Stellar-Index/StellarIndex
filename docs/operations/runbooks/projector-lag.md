---
title: Runbook — projector-lag
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_projector_lag_high` / `stellarindex_projector_error_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_projector_lag_high`, `stellarindex_projector_error_rate_high` |
| Severity | P3 (`severity: ticket` on both rules) — under Phase-3 parallel mode the dispatcher's per-source sink is still primary for most sources, so lag is visibility-only. **Exception: `sep41`.** Since F-1316 the indexer runs `SinkModeSkipSoleWriter`, i.e. the projector is the SOLE writer for the sep41 domain — lag there is real, customer-visible data lag, not a redundancy question. Re-promote the whole family to P2 once `[ingestion.persist_per_source]=false` (Phase 4, `SinkModeSkipProjected`). |
| Detected by | Prometheus rules in `deploy/monitoring/rules/projector.yml` + `configs/prometheus/rules.r1/projector.yml` |
| Expr | `max by (source) (stellarindex_projector_lag_ledgers) > 256` for `10m`; `sum by (source) (rate(stellarindex_projector_runs_total{outcome="error"}[15m])) > 0.05` for `15m` |
| Typical MTTR | 30 min |
| Impact | Per-source projection tables (`trades`, `blend_*`, `phoenix_*`, `cctp_events`, …) increasingly diverge from the authoritative event store. For the sole-writer sep41 domain that is served-data lag; for everything else in Phase 3 the dispatcher's sink still writes, so customer-facing rows are unaffected — but flipping the writer (Phase 4) is unsafe while lag is unbounded. |

## Symptoms

- `stellarindex_projector_lag_ledgers{source="<name>"}` > 256 for 10+ min.
- `stellarindex_projector_runs_total{outcome="error"}` rate > 0.05/s sustained.
- `stellarindex_projector_cycle_duration_seconds_bucket{source="<name>"}` p99 > 30 s (`projector.PerSourceTimeout` is 60 s).

**Where the projector reads from.** By default it tails the ClickHouse Tier-1
lake's `contract_events`, not Postgres `soroban_events`:
`storage.clickhouse_projector_source` defaults to **true** (ADR-0034 #10 /
ADR-0041, and it requires `clickhouse_live_sink`). Postgres `soroban_events`
is the legacy fallback, used only when that flag is turned off. Confirm which
one this host is on from the indexer's startup log:

```sh
journalctl -u stellarindex-indexer --no-pager | \
  grep -F 'projector reading from ClickHouse lake' | tail -1
# present → CH lake (default);  absent → legacy soroban_events read
```

The per-source cursor is source-agnostic, so the two read paths share the same
`ingestion_cursors` rows and the same lag gauge.

## Quick diagnosis (≤ 5 min)

```sh
# Read the per-source projector cursor — should be close to the
# ledgerstream tip.
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors \
   WHERE source = '"'"'projector'"'"' ORDER BY sub_source"'

# Compare against the live ledgerstream tip.
ssh root@136.243.90.96 'psql -U stellarindex -d stellarindex -c \
  "SELECT last_ledger FROM ingestion_cursors \
   WHERE source = '"'"'ledgerstream'"'"' AND sub_source = '"'"''"'"'"'

# Tail the projector log for the lagging source.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 | grep "projector cycle" | grep <source>'
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
- [ ] Step 3 — **catch-up after a real outage.** If the cursor is simply
      behind (it moves, just too slowly, or it was restarted from a lower
      watermark), rewind it explicitly and let the running projector tail
      forward. `-config`, `-source` and `-from` are ALL required; the source
      name is the projector's own name (`internal/projector/registry.go`), not
      the hyphenated table name `find-data-gaps` prints:

      ```sh
      # Dry run is the DEFAULT — it prints the intended rewind and writes nothing.
      stellarindex-ops projector-replay -config /etc/stellarindex.toml \
        -source <name> -from <ledger>

      # -write is REQUIRED to actually rewind the cursor.
      stellarindex-ops projector-replay -config /etc/stellarindex.toml \
        -source <name> -from <ledger> -write
      ```

      This is a one-shot cursor rewind, not a heavy job — the projector
      goroutine in `stellarindex-indexer` does the actual re-projection on its
      next cycle, and every per-source table writes `ON CONFLICT DO NOTHING`,
      so it is idempotent. An unknown `-source` fails loudly rather than
      reporting "no action". A whole-history re-derive is a different tool
      (`projected-rebuild`, run under `run-heavy-job.sh`). See
      [projector-replay](projector-replay.md) for the full procedure.
- [ ] Step 4 — if the projector is wedged on one source, check
      [projector-wedged](projector-wedged.md) FIRST (there is a dedicated
      gauge + alert for the window-floor wedge). Disabling the projector via
      `[ingestion.projector] enabled = false` in `/etc/stellarindex.toml` +
      `systemctl restart stellarindex-indexer.service` is a last resort: for
      the sole-writer sep41 domain it STOPS the only writer, so it is not a
      safe default.
- [ ] Verification: `stellarindex_projector_lag_ledgers` drops below
      256 within 30 minutes after restart (or the alert clears).

## Root cause analysis

- `journalctl -u stellarindex-indexer | grep projector` for the lag
  episode — `projector cycle` lines log per-cycle `rows_scanned`,
  `events_emitted`, `decode_errors`, `lag_ledgers`, `elapsed`.
- `sum by (source, outcome) (increase(stellarindex_projector_runs_total[6h]))`
  in Prometheus for the failure-class breakdown.
- `sum by (source, outcome) (increase(stellarindex_projector_events_decoded_total[6h]))`
  to separate decode failures from sink failures.

## Known false-positive patterns

- During a fresh deploy the projector starts at `(projector, <source>)`
  cursor = 0 and catches up from the soroban-era genesis ledger. The
  alert's 10-minute `for:` window absorbs the cold-start; if it fires,
  raise to 30 min temporarily.
- After a soroban-events landing-zone backfill, projector lag spikes
  while it catches up to the newly-written rows. Same as cold-start —
  let it drain.
- An OPERATOR-INITIATED `projector-replay` rewind no longer reaches this
  alert at all (issue #325): the replay tool records the rewind window,
  the projector publishes
  `stellarindex_projector_replay_window_active{source}=1` while the
  cursor is climbing back through it, and the rule carries `unless … ==
  1`. So if this alert IS firing, no recorded `projector-replay` rewind
  explains it — treat the lag as real. (A pending `projected-rebuild`
  window does NOT excuse lag: the flag is gated on the recorded window's
  `reason`, precisely so a source held at a rebuild's range stays
  alertable.) The mirror-image signal for a replay that has stopped
  climbing is `stellarindex_projector_replay_stalled`
  ([projector-replay](projector-replay.md)).

## Related

- ADR-0029 — soroban_events landing zone (the legacy raw store).
- ADR-0032 — per-source tables as projections (this runbook's parent).
- ADR-0034 #10 / ADR-0041 — the CH `contract_events` feed-switch that is now
  the default read source.
- `internal/projector/` — the projector implementation.
- The three sibling projector alerts, each with its own runbook:
  [projector-wedged](projector-wedged.md) (cursor stuck at the window floor),
  [projector-row-quarantined](projector-row-quarantined.md) (a row the sink
  refused), [projector-decode-error-rate](projector-decode-error-rate.md).
- [projector-replay](projector-replay.md) — the catch-up procedure referenced
  in step 3.
- `docs/operations/runbooks/source-stopped.md` — adjacent surface:
  per-source ingest cadence alerts (about live-ingest writes, not
  projection).

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): the read
  source is the ClickHouse `contract_events` lake by DEFAULT
  (`storage.clickhouse_projector_source = true`, ADR-0034 #10 / ADR-0041) —
  `soroban_events` is the legacy fallback; added the missing catch-up step
  (`projector-replay -config … -source … -from …`, all three required, run via
  `run-heavy-job.sh`); recorded the F-1316 sep41 sole-writer exception, which
  makes "just disable the projector" unsafe for that domain; listed the three
  sibling projector alerts and their runbooks; replaced the non-SQL
  `SELECT … FROM stellarindex_projector_runs_total` and the pprof capture
  (no pprof endpoint exists) with real PromQL.
- 2026-08-29 — issue #325: the lag rule gained an `unless
  stellarindex_projector_replay_window_active == 1` arm, so an
  operator-initiated replay is no longer a ~4h ticket that also masks a
  genuine lag on the same source.
- 2026-06-12 — F-1330: fix diagnosis SQL (`ingestion_cursors` not
  `source_cursors`; `last_updated` not `updated_at`); config file is
  `/etc/stellarindex.toml` not `r1.toml`.
- 2026-05-29 — initial draft (ADR-0032 Phase 3 rc.95).
