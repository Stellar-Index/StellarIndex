---
title: Runbook — backup-failed
last_verified: 2026-08-28
status: living
severity: P1
---

# Runbook — `stellarindex_timescale_backup_failed` / `_backup_none_24h` / `stellarindex_pgbackrest_backup_metrics_absent` / `_backup_unit_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_timescale_backup_none_24h` (no completed backup in > 24 h, `severity: page`, **SEV-1**) / `stellarindex_timescale_backup_failed` (> 25 h, `severity: ticket`). Both `for: 5m`. **The page fires first**; the ticket follows one hour later. `stellarindex_pgbackrest_backup_metrics_absent` (`severity: page`, `for: 15m`): the exporter is up but reports NO real stanza, so the two freshness alerts are structurally blind — treat as "backup state unknown", same urgency as `_none_24h`. `stellarindex_pgbackrest_backup_unit_failed` (`severity: ticket`, `for: 5m`): `pgbackrest-backup.service` exited non-zero — the early, causal signal. |
| Severity | P1 (`_backup_none_24h`) / ticket (`_backup_failed`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (the file r1 actually loads — `prometheus.r1.yml` `rule_files: /etc/prometheus/rules.r1/*.yml`; `deploy/monitoring/rules/storage.yml` is the multi-host source copy, identical at HEAD). Metric: `pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}` from woblerr `pgbackrest_exporter`, job `pgbackrest_exporter`, `localhost:9854`, unit `pgbackrest_exporter.service`, `--collect.interval=600` (up to 10 min metric lag). |
| Scheduler | `pgbackrest-backup.timer` → `pgbackrest-backup.service` → `/usr/local/bin/pgbackrest-backup.sh` (`User=postgres`), daily `02:00 UTC` + ≤ 15 min jitter, `Persistent=true`; full Sunday / diff Mon–Sat. Installed by `configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml`. |
| Repos | `repo1` = `/var/lib/pgbackrest` — local ZFS dataset `data/pgbackrest`, **same pool as the DB**, postgres-owned 0750, `repo1-retention-full=2`. `repo2` (offsite S3) is **NOT provisioned** on r1 (`pgbackrest_offsite_ack: true`; see `docs/operations/off-site-backup-plan.md`). pgBackRest does not use MinIO. |
| Typical MTTR | 30 min – 4 h |
| Impact | Increasing RPO. Our declared RPO is 5 min (held by continuous WAL archiving to repo1); at 24 h without a backup we are 288× over. If the primary dies in this window, we lose everything after the last good backup — and repo1 shares the primary's pool, so a pool loss loses both. |

## Symptoms

- `_backup_none_24h`: `min by (stanza)(pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}) > 24 * 3600` for 5 m; pages directly.
- `_backup_failed`: same expression `> 25 * 3600` for 5 m; ticket.
- `_metrics_absent`: `up{job="pgbackrest_exporter"} == 1 unless on (instance)
  pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}`
  for 15 m; page. Both `min by (stanza)(...)` alerts above evaluate to an
  EMPTY vector when that series is missing and can never fire, so an
  exporter that is up with zero stanzas would otherwise let backups stop
  silently for weeks (audit 2026-08-28, backup-restore-1).
- `_unit_failed`: `node_systemd_unit_state{name="pgbackrest-backup.service",state="failed"} == 1`
  for 5 m; ticket. Covers exit 127 (pgbackrest binary missing) and any
  backup error, ~24 h before the freshness alerts would.
- Cadence is **one backup per day** (02:00 UTC), so ONE failed or skipped
  nightly run pages ~24 h after the previous backup's stop time. There is
  no "missed one of many cycles" tier in practice — the two alerts are one
  hour apart.
- `journalctl -u pgbackrest-backup.service`: `ERROR: ...` — specific cause varies.

## Quick diagnosis (≤ 5 min)

```sh
# Single-host r1 today: run directly on r1 (Patroni is not deployed;
# when multi-region lands, run on the Patroni leader).
ssh root@136.243.90.96

# Most recent backup status — ALWAYS as postgres, never as root (see Mitigation).
sudo -u postgres pgbackrest --stanza=stellarindex info
#   Look at: backup timeline, last backup stop time, repository size.

# Did the scheduler fire, and how did the last run end?
systemctl list-timers pgbackrest-backup.timer --all
systemctl status pgbackrest-backup.service --no-pager
journalctl -u pgbackrest-backup.service --since '48 hours ago' --no-pager | tail -50

# Is repo1 (local disk, same pool as the DB) healthy / not full?
ls -ld /var/lib/pgbackrest
zfs list data/pgbackrest data/postgres
df -h /var/lib/pgbackrest

# Is continuous WAL archiving still working? (This is what holds the 5-min RPO.)
sudo -u postgres psql -Atc "SELECT archived_count, failed_count, last_archived_time, last_failed_time FROM pg_stat_archiver;"

# Is primary Postgres healthy? (Backup requires primary access.)
sudo -u postgres psql -Atc "SELECT now(), pg_is_in_recovery();"

# Is the exporter emitting the metric the alert reads? (If not, see exporter-down.md.)
curl -s http://127.0.0.1:9854/metrics | grep pgbackrest_backup_since_last_completion_seconds
```

## Typical root causes

1. **repo1 dataset / pool full, or permissions on `/var/lib/pgbackrest`.**
   repo1 is postgres-owned 0750 on the `data` pool. Root-owned files left
   behind by a manual `pgbackrest backup` run as root break the next
   timer-driven run as postgres. Check
   `find /var/lib/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest -not -user postgres`.
   - Mitigation: free space (see `zfs-pool-full.md` / `db-disk-full.md`);
     `chown -R postgres:postgres` the stray files.
   - Forward note: S3 credential / bucket-policy failures only apply to
     `repo2`, which is not provisioned on r1 yet.

2. **Primary resource pressure** — backup can't get a backup
   slot / buffer. Rare but has happened during heavy write
   bursts.

3. **WAL archive fallen behind** and pgBackRest's
   `archive-async` spool (`/var/spool/pgbackrest`) filled up.
   - Signal: `pg_stat_archiver` shows rising `failed_count`.
   - Mitigation: find why archive-push is failing — usually the
     same repo1 space/permission issue as above.

4. **pgBackRest version mismatch** between the pgBackRest binary
   and the repository format after a major upgrade. A repo cipher
   mismatch after a repo re-create (`docs/operations/pgbackrest-encryption.md`)
   presents identically.

5. **Scheduler didn't run.** `pgbackrest-backup.timer` disabled or absent
   (`18-pgbackrest-backup.yml` removes it on hosts with
   `pgbackrest_backup_enabled=false`); host down at 02:00 UTC
   (`Persistent=true` catches up on boot); or the unit exited 127
   (pgbackrest not installed — the testnet/futurenet shape). A missing
   timer is a documented past root cause of `_backup_none_24h`.

## Mitigation — `stellarindex_pgbackrest_backup_metrics_absent`

The exporter answers scrapes but `pgbackrest info` gives it nothing to
report. Find out which of the three shapes it is, then fall through to
the mitigation below once a backup series exists again:

```sh
curl -s http://127.0.0.1:9854/metrics | grep -E '^pgbackrest_(exporter_status|stanza_status|backup_since_last_completion_seconds)'
journalctl -u pgbackrest_exporter.service -n 50 --no-pager
# Exactly what the exporter sees — run AS THE EXPORTER USER, not postgres/root:
sudo -u pgbackrest_exporter pgbackrest --stanza=stellarindex info --output=json
stat -c '%U:%G %a' /etc/pgbackrest/pgbackrest.conf /var/lib/pgbackrest
command -v pgbackrest || echo "pgbackrest NOT installed"
```

| Shape | Fix |
| ----- | --- |
| `pgbackrest` not installed (unit exits 127; testnet/futurenet shape) | Install pgbackrest and create the stanza, or set `pgbackrest_backup_enabled=false` for the host so the exporter and timer are removed together |
| Permission denied on `pgbackrest.conf` / repo (managed conf is `postgres:postgres 0640`, repo `0750`; the exporter runs as its own `pgbackrest_exporter` user) | Make the conf group-readable by the exporter user (add it to the `postgres` group) — verify with the `sudo -u pgbackrest_exporter … info` line above |
| Stanza error in `pgbackrest info` | Fix the stanza (`stanza-create` / repo cipher / lock files) as for `_backup_failed` below |

While this alert is firing the freshness alerts say nothing; do not read
their silence as "backups are fine". Verification: the `curl` line shows
`pgbackrest_backup_since_last_completion_seconds{stanza="stellarindex",…}`
and the alert resolves within ~15 min.

## Mitigation — `stellarindex_timescale_backup_*`

- [ ] Step 1 — immediate: run a manual backup to verify the
      system works. **NEVER run pgbackrest as root** — it leaves
      root-owned lock/log/manifest files in the postgres-owned repo and
      breaks subsequent timer runs:
      ```sh
      sudo -u postgres pgbackrest --stanza=stellarindex --type=diff backup
      ```
- [ ] Step 2 — if manual works: investigate scheduler.
      ```sh
      systemctl list-timers pgbackrest-backup.timer --all
      systemctl enable --now pgbackrest-backup.timer
      journalctl -u pgbackrest-backup.service -n 100 --no-pager
      ```
- [ ] Step 3 — if manual fails: fix the specific error
      (space, permissions, version/cipher, primary access).
- [ ] Step 4 — for `_backup_none_24h`: **declare SEV-1**. This is
      an RPO breach. r1 has **no replica**; the only other safety net is
      continuous WAL archiving into the same repo1. Confirm
      `pg_stat_archiver.failed_count` is not rising and
      `last_archived_time` is recent, and confirm the pool is not near
      full (`zfs-pool-full.md`). An RPO breach here has no offsite
      fallback (`off-site-backup-plan.md`, status proposed).
- [ ] Step 5 — once healthy, take a full backup (not differential)
      to establish a known-good restore point (the timer's next full is
      Sunday):
      ```sh
      sudo -u postgres pgbackrest --stanza=stellarindex --type=full backup
      ```
      Note `repo1-retention-full=2`: an extra full expires the oldest
      chain. Make sure the restore drill (`restore-drill-stale.md`) has
      validated the newest chain before relying on it.
- [ ] Verification: `sudo -u postgres pgbackrest --stanza=stellarindex info`
      shows a backup with stop time < 24 h AND
      `pgbackrest_backup_since_last_completion_seconds` has dropped in
      Prometheus (allow ≤ 10 min exporter lag) AND both alerts resolve in
      Alertmanager.

## Root cause analysis

- Backup log from the last successful through the first failure.
- ZFS / kernel logs for the `data` pool in the window.
- Any secret / config / binary upgrade around the failure time?
- RPO math: what data would have been lost if primary failed?

## Known false-positive patterns

- **Exporter down** — the alerts go `no_data`-blind rather than
  false-firing; that gap is covered by
  `stellarindex_pgbackrest_exporter_down` (`rules.r1/meta.yml`,
  `exporter-down.md`), and the exporter-UP-but-no-stanza gap by
  `stellarindex_pgbackrest_backup_metrics_absent` (this runbook). If the
  exporter was down across 02:00, the metric can read stale for up to
  10 min after it returns. `_metrics_absent` deliberately does not fire
  while `up == 0`, so the two never double-page.
- **First backup after a repo re-create** (`pgbackrest-encryption.md`)
  legitimately resets the age series; the alerts resolve on the first
  completed backup.

## Related

- `restore-drill-stale.md` — `stellarindex_restore_drill_stale`; monthly
  first-Saturday 04:00 UTC drill proves repo1 restores.
- `exporter-down.md` — `pgbackrest_exporter` down makes these alerts blind.
- `zfs-pool-full.md` / `db-disk-full.md` — repo1 shares the DB's pool; if
  the pool fills, WAL archive stops first, then backups fail.
- `timescale-primary-down.md` — Patroni failover procedure for a topology
  NOT deployed on single-host r1; primary must be reachable to take backups.
- `docs/operations/pgbackrest-encryption.md` — repo cipher / re-create procedure.
- `docs/operations/off-site-backup-plan.md` — repo2 (offsite) plan, status proposed.
- `docs/adr/0043-backup-and-restore-strategy.md`.
- HA plan §3.3 "Backup" reality check + §8: `docs/architecture/ha-plan.md`.

> TODO(ash): the archival-node role templates no `archive_mode` /
> `archive_command` (WAL archiving is configured out-of-band on r1 per
> `18-pgbackrest-backup.yml`); the live `pgbackrest.conf` is hand-managed
> (`pgbackrest_manage_conf` defaults false). Confirm on r1 that
> `SHOW archive_command` is `pgbackrest ... archive-push %p` and that
> repo1 is still `/var/lib/pgbackrest`, then delete this note.

## Changelog

- 2026-08-28 — added `stellarindex_pgbackrest_backup_metrics_absent`
  (page) + `stellarindex_pgbackrest_backup_unit_failed` (ticket) and their
  mitigation section (audit finding backup-restore-1: the `min by
  (stanza)` freshness alerts evaluate over an empty vector — never fire —
  when the exporter is up with zero stanzas, and `exporter_down` checks
  `up` alone). Promtool coverage: `deploy/monitoring/rule-tests/storage-backup_test.yml`.
- 2026-08-28 — re-verified against HEAD. Alert tiering corrected (page at
  24 h fires BEFORE the ticket at 25 h; one daily cycle, not hourly);
  rule path → `rules.r1/storage.yml`; MinIO/S3 checks removed (repo1 is
  local ZFS); all pgbackrest/psql commands run as postgres; k8s/CronJob
  and "check replication" references removed (no replica on r1).
- 2026-04-23 — initial draft. Emphasises the RPO math —
  "missed one backup" and "no backup 24h" are different severity
  levels because the cost curve is non-linear.
