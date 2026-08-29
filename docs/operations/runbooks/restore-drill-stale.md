---
title: Runbook — restore-drill-stale
last_verified: 2026-08-28
status: ratified
severity: P3
---

# Runbook — `stellarindex_restore_drill_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_restore_drill_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/restore-drill.yml` |
| Typical MTTR | 30 min (to confirm cause) + up to 4 h (a full drill run) |
| Impact | We have no *current* proof the pgBackRest backup chain is restorable (ADR-0043 §3 / CS-110). The backups may be fine — this alert says we can't presently prove it. |

## Symptoms

- `(time() - stellarindex_restore_drill_last_success_unix) > 40 * 24 * 3600`
  for ≥ 30 min, **or** the series is absent for ≥ 40 d
  (`absent_over_time(...[40d])`).
- No fully-successful monthly restore-drill has been recorded in over
  40 days (one missed monthly cycle plus slack), or none has ever been
  recorded.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is the monthly timer scheduled + when did it last fire?
sudo systemctl status restore-drill.timer
sudo systemctl list-timers restore-drill.timer

# 2. What did the last run do? (belt-and-suspenders log off journald)
sudo tail -n 100 /var/log/restore-drill.log
sudo journalctl -u restore-drill.service --since "45 days ago" -n 200

# 3. Is the durable evidence log being written?
sudo tail -n 20 /var/lib/stellarindex/restore-drills/restore-drills.md

# 4. Is the metric present + fresh?
cat /var/lib/node_exporter/textfile_collector/restore_drill.prom
```

## Typical root causes

1. **Timer disabled** — operator ran `systemctl stop restore-drill.timer`
   during capacity/maintenance work and did not re-enable it.
   - Signal: `systemctl status restore-drill.timer` shows `inactive`.
   - Mitigation: `sudo systemctl enable --now restore-drill.timer`.

2. **Every run refusing on free space** — the drill sizes its floor from
   the latest backup (`pgbackrest info` database size × 125 % + 50 G WAL
   headroom, never below `MIN_FREE_GB`=200 G) and exits 2 (uncounted
   precondition) below that, so it never stamps a success.
   - Signal: `/var/log/restore-drill.log` ends with a `refusing` note.
   - Mitigation: free space on the drill dataset (Phase-A capacity
     runbook), then force a run (below).

3. **Every run failing a verification check** — the drill runs but a
   check fails (or aborts at restore / recovery), so `fail_count > 0` and
   last_success is deliberately not stamped; the series drops from the
   textfile and goes absent. `stellarindex_restore_drill_failed` fires
   within 30 min of that run — this ticket is the 40-day backstop.
   - Signal: `restore_drill.prom` has `stellarindex_restore_drill_failures > 0`
     and no `..._last_success_unix` line; `/var/log/restore-drill.log`
     names the failing check.
   - Mitigation: route to `backup-failed.md` for the specific check.

4. **Evidence unwritable** — the drill passed all checks but could not
   write `/var/lib/stellarindex/restore-drills/restore-drills.md`, which
   the script now treats as a FAILURE (the evidence is the deliverable).
   - Signal: log ends with `FATAL: could not write drill evidence`.
   - Mitigation: fix ownership/permissions on
     `/var/lib/stellarindex/restore-drills`, then force a run.

5. **node_exporter not scraping the textfile dir** — the drill succeeds
   and writes the `.prom`, but Prometheus never sees the metric.
   - Signal: the file exists with a fresh mtime but the gauge has no
     samples in Prometheus.
   - Mitigation: confirm node_exporter's
     `--collector.textfile.directory` points at
     `/var/lib/node_exporter/textfile_collector`.

## Mitigation

- [ ] Step 1 — Walk the diagnostic commands; identify which stage is
      silent.
- [ ] Step 2 — Apply the matching fix from "Typical root causes."
- [ ] Step 3 — Force a drill run: `sudo systemctl start restore-drill.service`
      (up to ~4 h — it is a full scratch restore + WAL replay). Watch
      `/var/log/restore-drill.log`.
- [ ] Verification: after a clean run, `restore_drill.prom` carries a
      fresh `stellarindex_restore_drill_last_success_unix`, the evidence
      log has a new dated entry, and the alert clears within ~5 min.

## Known false-positive patterns

- **Fresh node bring-up** — a newly deployed archival node has never run
  the drill, so the series is absent and this ticket fires until the
  first monthly drill (or a manual run) lands. It is intentionally a P3
  ticket, not a page, for exactly this reason: on a new node, run the
  drill once by hand to seed the evidence and clear it.

## Related

- `restore-drill-failed.md` — `stellarindex_restore_drill_failed`; the
  immediate signal when the most recent run recorded failures > 0.
- `backup-failed.md` — when a drill's individual restore/verify checks
  fail.
- `ch-schema-restore.md` — the ClickHouse half of a restore; the
  restore-drill's optional CH re-derive stage exercises the same path.

## Changelog

- 2026-08-14 — initial draft alongside the BDR-03 remediation (durable
  evidence + `stellarindex_restore_drill_last_success_unix` metric +
  this staleness alert).
- 2026-08-28 — backup-derived capacity floor; companion
  `stellarindex_restore_drill_failed` ticket (audit backup-restore-3/-4).
