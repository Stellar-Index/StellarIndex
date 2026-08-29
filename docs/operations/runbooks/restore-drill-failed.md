---
title: Runbook — restore-drill-failed
last_verified: 2026-08-28
status: ratified
severity: P3
---

# Runbook — `stellarindex_restore_drill_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_restore_drill_failed` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/restore-drill.yml` (mirrored in `configs/prometheus/rules.r1/restore-drill.yml`) |
| Typical MTTR | 30 min (to read the evidence) + up to 4 h (a clean re-run) |
| Impact | The most recent restore-drill ran and did NOT prove the pgBackRest backup chain restorable (ADR-0043 §3 / CS-110). The backups may still be fine — this alert says the last attempt to prove it failed. |

## Why this exists

Before 2026-08-28 the two most likely drill failures — `pgbackrest
restore` failing and the scratch instance never reaching consistency —
exited before the evidence phase. The drill log got no entry and the
previous month's textfile (`failures 0`, a fresh `last_success`) kept
being scraped as if the backup had just been proven restorable; the only
alert was the 40-day staleness ticket. The script now records every run
that gets past its preconditions, and this alert reads that record.

## Symptoms

- `stellarindex_restore_drill_failures > 0` for ≥ 30 min.
- The tail of `/var/lib/stellarindex/restore-drills/restore-drills.md`
  either names an aborted stage (`ABORTED at pg_restore` /
  `ABORTED at pg_start`) or lists `failures: N` after a full
  verification pass.
- `stellarindex_restore_drill_last_success_unix` is absent from the
  textfile (a failed run never stamps it), so `restore-drill-stale`
  will follow in ≤ 40 d if nothing changes.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Which stage failed? The evidence log names it.
sudo tail -n 12 /var/lib/stellarindex/restore-drills/restore-drills.md

# 2. The full run output (pgbackrest / pg_ctl diagnostics live here).
sudo tail -n 200 /var/log/restore-drill.log
sudo journalctl -u restore-drill.service -n 200

# 3. The metric as scraped.
cat /var/lib/node_exporter/textfile_collector/restore_drill.prom

# 4. Is the backup chain itself healthy?
sudo -u postgres pgbackrest --stanza=stellarindex info
```

## Typical root causes

1. **`ABORTED at pg_restore`** — pgbackrest could not restore the latest
   backup (repo unreadable, checksum/manifest error, or ENOSPC). The
   partial datadir is removed automatically; the pgbackrest output in
   `/var/log/restore-drill.log` is the diagnostic.
   - Mitigation: `pgbackrest --stanza=stellarindex check`; for a repo2
     (offsite) drill confirm credentials + reachability; route to
     `backup-failed.md` if the chain itself is broken.

2. **`ABORTED at pg_start`** — the restored cluster never reached
   consistency within `PG_START_TIMEOUT` (7200 s): a missing WAL
   segment in the archive, or the mirrored GUCs (`max_connections`,
   `max_locks_per_transaction`, …) no longer match the primary. The
   datadir is KEPT under `/srv/restore-drill/pgdata-*` for diagnosis;
   delete it once read.
   - Mitigation: read `pgdata-*/log/` or the unit log for the recovery
     error; `pgbackrest --stanza=stellarindex info` + the archive check
     in `backup-failed.md` for a WAL gap.

3. **A verification check failed** (`core_tables`, `wal_drain`,
   `tip_lag`, `hash_chain_sample`, `trades_window_match`,
   `ch_rederive`) — the restore worked but the copy disagrees with the
   live DB or the archive stream did not drain.
   - Mitigation: the log line `FAIL <check> — <detail>` says which; the
     datadir is kept. `wal_drain`/`tip_lag` point at WAL archiving,
     `hash_chain_sample`/`trades_window_match` at data integrity.

4. **Evidence unwritable** — all checks passed but the evidence log
   could not be written; counted as a failure by design.
   - Mitigation: fix ownership of `/var/lib/stellarindex/restore-drills`.

## Mitigation

- [ ] Step 1 — Read the evidence entry + log; identify the stage.
- [ ] Step 2 — Apply the matching fix above.
- [ ] Step 3 — Re-run: `sudo systemctl start restore-drill.service`
      (up to ~4 h). Watch `/var/log/restore-drill.log`.
- [ ] Verification: `restore_drill.prom` carries
      `stellarindex_restore_drill_failures 0` and a fresh
      `stellarindex_restore_drill_last_success_unix`; the alert clears
      within ~5 min of the scrape.

## Known false-positive patterns

- **Capacity refusal is NOT this alert.** A run that refuses on free
  space (exit 2) writes neither evidence nor metric, so `failures`
  keeps the previous run's value; that case surfaces only through
  `restore-drill-stale.md`. If this alert fires, the drill ran.

## Related

- `scripts/ops/restore-drill.sh` — the drill; `restore-drill-run-test.sh`
  pins the abort-path evidence + metric behaviour in CI.
- `restore-drill-stale.md` — `stellarindex_restore_drill_stale`; the
  40-day backstop when no clean run lands.
- `backup-failed.md` — the backup chain itself.
- `zfs-pool-full.md` — the drill restores onto the shared pool; its
  capacity floor is sized from the backup for exactly this reason.

## Changelog

- 2026-08-28 — initial draft alongside the abort-path evidence fix
  (audit backup-restore-4).
