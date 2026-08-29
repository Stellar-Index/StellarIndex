---
title: Runbook — supply-snapshot-stale
last_verified: 2026-08-29
status: ratified
severity: P3
---

# Runbook — `stellarindex_supply_snapshot_stale` / `_critical_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_supply_snapshot_stale` (P3, > 36 h), `stellarindex_supply_snapshot_critical_stale` (P2, > 72 h) |
| Detected by | `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`; `_stale`: `severity: ticket`, `for: 5m`; `_critical_stale`: `severity: page`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`. |
| Typical MTTR | 15 min |
| Impact | `/v1/assets/{id}` F2 fields visibly old. After ≥ 36 h the displayed `observed_at` is more than a day behind chain state. |

## Two refresh paths — r1 runs BOTH; this alert is real

Per [supply-pipeline.md](../../architecture/supply-pipeline.md),
`asset_supply_history` snapshots are produced by **both** of:

1. **systemd timer** (`supply-snapshot.timer` →
   `stellarindex-ops supply snapshot`, native XLM only; writes
   `last_success_timestamp` into
   `/var/lib/node_exporter/textfile_collector/supply_snapshot.prom`).
   This alert tracks **only** that gauge.
2. **Aggregator-resident goroutine** (`runSupplyRefresh` in
   `cmd/stellarindex-aggregator`, gated by
   `[supply] aggregator_refresh_enabled = true`, emits
   `stellarindex_aggregator_supply_refresh_total{outcome=…}` —
   tracked by `supply-refresh-stalled.md` /
   `-error-dominant.md`).

**On r1 both paths are live**: the ansible template renders
`aggregator_refresh_enabled = true` with a 5-minute cadence for the
watched classic assets (`stellarindex.toml.j2` `[supply]` block),
AND the timer path runs for native XLM with `TEXTFILE_OUTPUT` set
via the ansible-managed `/etc/default/supply-snapshot`
(`10-observability.yml`). They are complementary, not
alternatives — do NOT silence this alert on the theory that "the
goroutine path covers it": the goroutine path's metrics are a
different alert family, and a firing `_stale` here means the
timer path (native XLM's canonical CLI surface) genuinely stopped
succeeding. Follow the diagnosis below.

## Symptoms

- `(time() - stellarindex_supply_snapshot_last_success_timestamp{asset_key=…}) > 36*3600`
  for ≥ 5 min.
- Two-tier: 36 h is the standard heartbeat budget (24 h cron + 12 h
  cushion); 72 h is the page-the-on-call line.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Is the timer scheduled?
systemctl status supply-snapshot.timer
systemctl list-timers supply-snapshot.timer

# 2. When did the unit last run?
journalctl -u supply-snapshot.service --since "3 days ago" -n 50

# 3. Is the textfile being written?
ls -la /var/lib/node_exporter/textfile_collector/supply_snapshot.prom

# 4. Force a one-off run.
systemctl start supply-snapshot.service
```

## Typical root causes

1. **Timer disabled.** Operator ran `systemctl stop supply-snapshot.timer`
   for maintenance and forgot to re-enable.
   - Mitigation: `systemctl enable --now supply-snapshot.timer`.

2. **Service unit failing every run.** Fires alongside the
   `_unit_failed_alert`.
   - Mitigation: route to `supply-snapshot-unit-failed.md`.

3. **`TEXTFILE_OUTPUT` unset? Then this alert CANNOT be what
   fired.** With no textfile the gauge series is ABSENT, and
   `time() - <missing>` evaluates to no data — that state is owned
   by `stellarindex_supply_snapshot_never_initialized`
   ([supply-snapshot-never-initialized.md](supply-snapshot-never-initialized.md)).
   On r1, `/etc/default/supply-snapshot` is ansible-managed with
   `TEXTFILE_OUTPUT` set (`10-observability.yml`) — finding it
   unset is config drift to codify + fix, not an operator option.

4. **Clock skew.** If the host clock jumped backwards, a recently-
   passed run has a timestamp that looks ancient relative to `time()`.
   - Signal: `node_time_seconds` deviates from real time.
   - Mitigation: investigate ntp; clear by running the writer once
     with a fresh clock.

## Mitigation

- [ ] Step 1 — Identify which stage is silent (timer / unit / textfile).
- [ ] Step 2 — Apply the matching root-cause fix.
- [ ] Step 3 — Force a run: `systemctl start supply-snapshot.service`.
- [ ] Verification: `last_success_timestamp` updates within 60 s
      after a successful run lands in node_exporter.

## Known false-positive patterns

- **A never-initialized gauge cannot trip this alert.** An absent
  series makes the expr evaluate to no data — r1 sat exactly there,
  silently, for 24+ days before the 2026-05-08 audit (documented in
  `rules.r1/supply-snapshot.yml`'s `_never_initialized` comment
  block). That state belongs to
  [supply-snapshot-never-initialized.md](supply-snapshot-never-initialized.md);
  if `_stale` fired, the gauge existed and a successful run HAS
  happened at some point.

## Related

- `supply-snapshot-unit-failed.md` — when runs are failing.
- `supply-snapshot-never-initialized.md` — the absent-series owner
  (gauge never emitted; this alert's structural blind spot).
- `supply-refresh-stalled.md` — the aggregator-resident-path counterpart (this alert covers the systemd-timer path; that one covers the goroutine path).
- `supply-refresh-error-dominant.md` — sibling for the goroutine-path failure mode.
- `archive-completeness-stale.md` — same shape on the archive side.
- `docs/architecture/supply-pipeline.md` — the two-path overview both runbooks live under.

## Changelog

- 2026-08-29 — re-verified against HEAD. False-positive entry
  "fresh deploy — gauge never set" replaced: an absent series means
  the expr evaluates to no data, so a never-initialized gauge
  CANNOT trip this alert — that state is owned by
  `_never_initialized` (r1's 24+-day silent pre-2026-05-08 gap is
  the founding case). Root cause 3 (`TEXTFILE_OUTPUT` unset)
  rerouted for the same absent-series logic, and noted that on r1
  the env file is ansible-managed with the path set — unset =
  drift. Two-path framing rewritten: r1 runs BOTH paths (timer for
  native XLM + `aggregator_refresh_enabled=true`/5m for watched
  classic assets), so the pick-one/silence advice was wrong — the
  alert is real on r1. Rule citation → `rules.r1/supply-snapshot.yml`
  with per-alert severities; commands use r1 shapes;
  `_never_initialized` added to Related.
- 2026-04-30 — initial draft alongside #295 (textfile + alerts).
- 2026-04-30 — added two-refresh-paths callout. PR #318's
  supply-pipeline architecture documents two producers of
  `asset_supply_history` (systemd timer + aggregator goroutine);
  this alert is timer-path-only, so deployments using the
  goroutine path were silently false-positiving without
  cross-reference to the alternative path.
