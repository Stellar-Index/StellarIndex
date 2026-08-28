---
title: Runbook — supply-refresh-stalled
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_aggregator_supply_refresh_stalled`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_supply_refresh_stalled` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/supply-refresh.yml` (group `stellarindex.supply_refresh`, `severity: page`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-refresh.yml`. |
| Typical MTTR | 15–30 min |
| Impact | `/v1/assets/{id}` F2 fields go increasingly stale across all watched assets. Customer-visible after the first stale snapshot lands a few minutes after the alert. |

## Symptoms

- `sum(changes(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[30m])) == 0`
  for ≥ 5 min. (An earlier `time() - max(timestamp(...))` form was
  abandoned — `timestamp()` returns the scrape time ≈ now, so it
  never fired.)
- Journal signal: the `supply refresh ok` line is Debug-level and
  invisible at the default log level — its absence proves nothing.
  The VISIBLE signals are the Warn/Error lines
  (`supply refresh: no ledger` / `compute failed` / `insert failed`).

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Aggregator process up?
systemctl status stellarindex-aggregator

# 2. Are any goroutines progressing? (Other counters should
#    be incrementing if the orchestrator is alive.)
curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_ticks_total

# 3. What's the most-recent supply refresh outcome label?
#    The metric is keyed by (asset_key, outcome) — if every asset_key
#    has stopped incrementing the page is fleet-wide; if only one
#    asset_key has stalled while others tick, the failure is
#    per-asset and `error_dominant` should ALSO be firing.
curl -s http://localhost:9465/metrics | \
  awk '/^stellarindex_aggregator_supply_refresh_total\{/' | sort

# 4. Recent supply-refresh logs. NB: the ok line is Debug-level —
#    look for the Warn/Error lines ("supply refresh: no ledger",
#    "compute failed", "insert failed").
journalctl -u stellarindex-aggregator --since "1 hour ago" -n 200 | \
  grep -E "supply refresh|supply-refresh"
```

## Typical root causes

1. **Aggregator process down.** `systemctl status` shows
   inactive / failed.
   - Mitigation: investigate the crash via journald; restart.

2. **Orchestrator wedged.** Process is running but no goroutine
   is making progress. The orchestrator's own tick counter
   (`stellarindex_aggregator_ticks_total`) is also stalled.
   - Mitigation: restart the binary. File a P2 bug for the wedge.

3. **Every tick failing.** Goroutine is alive but every
   per-asset Tick produces a non-ok outcome.
   `_error_dominant` should ALSO be firing in this case — route
   to that runbook.

4. **Refresher disabled? Then this alert CANNOT be what fired.**
   With the `changes()` expr, a disabled refresher means the series
   is ABSENT — that state belongs to
   `stellarindex_aggregator_supply_refresh_never_initialized`
   (`absent_over_time` 36h →
   [`supply-snapshot-never-initialized.md`](supply-snapshot-never-initialized.md)).
   On r1 the goroutine path is default-ON anyway (the ansible
   template renders `aggregator_refresh_enabled = true`; the code
   default is false). If `_stalled` fired, the refresher was
   recently alive.

## Mitigation

- [ ] Step 1 — Check process health (Quick diagnosis #1).
- [ ] Step 2 — If process is up but stalled: check
      `stellarindex_aggregator_ticks_total` — if THAT also stalled,
      the orchestrator is wedged; restart.
- [ ] Step 3 — If process is up + orchestrator is ticking but
      supply isn't: confirm `aggregator_refresh_enabled = true` in
      the config; check logs for repeated outcome labels.
- [ ] Step 4 — Force a restart as the safe mitigation;
      investigate the underlying cause from journald +
      pprof goroutine dump if available.
- [ ] Verification: `outcome="ok"` increments resume within
      `aggregator_refresh_cadence` (default 5 min). The alert
      clears once any `outcome="ok"` increment lands inside the
      trailing 30 m `changes()` window.

## Known false-positive patterns

- **Aggregator restart in progress.** The first few minutes
  after a restart have no observations yet. `for: 5m` absorbs
  ~one cadence; longer restarts still trip it.
- **Refresher disabled by config.** Under the `changes()` expr this
  no longer fires here (absent series) — it surfaces as
  `_never_initialized` instead; see root cause 4.

## Related

- `supply-refresh-error-dominant.md` — when the refresher is
  alive but every tick fails.
- `supply-snapshot-never-initialized.md` — the
  `_never_initialized` alert (`absent_over_time` 36h): where a
  disabled / never-started refresher surfaces.
- `supply-snapshot-stale.md` — the systemd-timer-path equivalent
  (different metric, different expectation).
- `ch-supply-gapfill-failed.md` — the ClickHouse supply gap-fill
  sibling.
- `aggregator-silent.md` — when the orchestrator's tick counter
  itself is stalled.

## Changelog

- 2026-08-28 — re-verified against HEAD. Symptom expr replaced: the
  documented `time() - max(timestamp(...))` form was abandoned and
  NEVER FIRED (`timestamp()` returns scrape time ≈ now); the HEAD
  rule is `sum(changes(..._supply_refresh_total{outcome="ok"}[30m])) == 0`,
  `for: 5m` — verification wording updated to match. Root cause 4
  rewritten: with `changes()` a disabled refresher means an ABSENT
  series (→ `_never_initialized` / supply-snapshot-never-initialized.md),
  and r1 renders `aggregator_refresh_enabled=true` anyway — if
  `_stalled` fired the refresher was recently alive. Journal signal
  corrected: the ok line is Debug-level and invisible at the default
  level; the Warn/Error lines are the visible signals. Rule citation
  → `rules.r1/supply-refresh.yml`; commands use r1 shapes;
  `_never_initialized` + `ch-supply-gapfill-failed.md` added to
  Related.
- 2026-04-30 — initial draft alongside #313 (supply-refresh
  alerts).
- 2026-04-30 — quick-diagnosis #3 now references the
  `asset_key` label (added in #314) so operators can confirm
  whether the stall is fleet-wide or scoped to one asset.
