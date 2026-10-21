---
title: Runbook — restore-drill
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — restore-drill alerts

## At a glance

Restore-drill alerts: failed, stale, offsite stale, textfile stale.

All `severity: ticket` (P3), `component: storage`. Rules: `configs/prometheus/rules.r1/restore-drill.yml` (also `deploy/monitoring/rules/restore-drill.yml`); `stellarindex_restore_drill_textfile_stale` is in `storage.yml`. Impact: no current proof the pgBackRest chain is restorable (ADR-0043 §3 / CS-110); the backups may be fine.

`scripts/ops/restore-drill.sh` does a non-destructive scratch restore of the stanza with WAL replay and verification. Two monthly timers, one drill at a time:

| Timer | Repo | Schedule | Log | Textfile (`/var/lib/node_exporter/textfile_collector/`) |
| ----- | ---- | -------- | --- | ------- |
| `restore-drill.timer` | repo1 (on-box) | first Saturday 04:00 UTC | `/var/log/restore-drill.log` | `restore_drill.prom` |
| `restore-drill-offsite.timer` | repo2 (encrypted S3) | the 15th 04:00 UTC | `/var/log/restore-drill-offsite.log` | `restore_drill_repo2.prom` |

- Every series carries a `repo` label. `stellarindex_restore_drill_last_success_unix` is stamped ONLY on a fully clean run (zero failed checks AND evidence written); a failed run drops it, so the series goes absent.
- Evidence log (names the repo per entry): `/var/lib/stellarindex/restore-drills/restore-drills.md`; figures in `docs/operations/drills/restore-drills.md`.
- Full drill: up to ~4 h repo1, ~1 h repo2. Force a run once the cause is cleared: `sudo systemctl start restore-drill.service` (repo1) or `restore-drill-offsite.service` (repo2); watch the log. A fresh `.prom` plus a new evidence entry clears the alert within ~5 min.
- Capacity floor: latest-backup database size x 125 % + 50 G, never below `MIN_FREE_GB`=200 G. Below it the drill exits 2 (no evidence, no metric, `failures` keeps the previous value): free space on the drill dataset, force a run.
- Evidence unwritable counts as a failure (`FATAL: could not write drill evidence`): fix ownership of `/var/lib/stellarindex/restore-drills`, force a run.
- node_exporter must use `--collector.textfile.directory /var/lib/node_exporter/textfile_collector`; a fresh `.prom` mtime with no samples in Prometheus means it does not.
- Repo1 sets `DRILL_CH_WINDOW=100000`: the CH re-derive stage writes `stellarindex_restore_drill_ch_rederive_seconds`, `..._ch_rederive_window_ledgers`, `..._ch_rederive_ledgers_per_second`. `absent()` after a drill means "unmeasured", never "fast".
- Both units also fall under the catch-all `stellarindex_systemd_unit_failed` (the only fast signal for a unit that never reaches `ExecStart`, e.g. `226/NAMESPACE`). A refusal (exit 2) leaves the unit `failed` until `sudo systemctl reset-failed restore-drill-offsite.service` (same for `restore-drill.service`); this does not touch the drill verdict.
- Fresh node bring-up: the stale tickets fire until the first drill lands; run it once by hand.

## stellarindex_restore_drill_stale

repo1 (`repo=~"1|"`): `(time() - stellarindex_restore_drill_last_success_unix{repo=~"1|"}) > 40 * 24 * 3600` for 30 min, or the series absent for 40 d. The matcher is an allow-list (repo1 or no `repo` label), deliberately not `repo!="2"`. The empty alternative is a transition allowance: until the first labelled repo1 run rewrites `restore_drill.prom` with `repo="1"`, an un-labelled series may carry the OFF-SITE verdict, so treat a green alert as unproven for repo1 and read the evidence log (force it: `sudo systemctl start restore-drill.service`).

**Check**

```sh
# 1. Is the monthly timer scheduled + when did it last fire?
sudo systemctl status restore-drill.timer
sudo systemctl list-timers restore-drill.timer

# 2. What did the last run do?
sudo tail -n 100 /var/log/restore-drill.log
sudo journalctl -u restore-drill.service --since "45 days ago" -n 200

# 3. Is the durable evidence log being written?
sudo tail -n 20 /var/lib/stellarindex/restore-drills/restore-drills.md

# 4. Is the metric present + fresh?
cat /var/lib/node_exporter/textfile_collector/restore_drill.prom
```

**Likely causes**

- Timer disabled (`inactive`): `sudo systemctl enable --now restore-drill.timer`.
- Every run refusing on free space (log ends with a `refusing` note), or evidence unwritable: see the shared notes.
- Every run failing a check (`stellarindex_restore_drill_failures > 0`, no `last_success_unix`): triage under [stellarindex_restore_drill_failed](#stellarindex_restore_drill_failed) and `infra.md#stellarindex_timescale_backup_none_24h`.
- node_exporter not scraping the textfile dir.

**Fix** Apply the matching fix above, force a run, confirm `restore_drill.prom` carries a fresh `last_success_unix` and the evidence log a new dated entry.

## stellarindex_restore_drill_textfile_stale

mtime backstop in `storage.yml`: `time() - node_textfile_mtime_seconds{file=~".*/restore_drill.*\\.prom$"} > 35d`, `for: 30m`. The generic `stellarindex_textfile_producer_stale` excludes `restore_drill*.prom` (24 h would false-fire on a monthly timer). Fires when the timer dies before writing a fresh gauge; diagnose as the repo1 or repo2 stale alert per the `file` label (`restore_drill.prom` = repo1, `restore_drill_repo2.prom` = repo2).

## stellarindex_restore_drill_offsite_stale

repo2: `(time() - stellarindex_restore_drill_last_success_unix{repo="2"}) > 35 * 24 * 3600` for 30 min, or the `repo="2"` series absent for 35 d. Repo1 lives on the database's disks and dies with the pool; repo2 is what a host- or pool-loss DR restores from (ADR-0043 §1, [dr-activation](dr-activation.md)), so a green repo1 drill says nothing about it. Fires from rule deploy until the first labelled repo2 run lands; seed with `--tags ops-jobs` and a forced run.

**Check**

```sh
# 1. Timer scheduled + last fired?
sudo systemctl status restore-drill-offsite.timer
sudo systemctl list-timers restore-drill-offsite.timer

# 2. Last run (own log, apart from repo1's)
sudo tail -n 100 /var/log/restore-drill-offsite.log
sudo journalctl -u restore-drill-offsite.service --since "40 days ago" -n 200

# 3. Evidence log; entries name the repo
sudo grep -n "restore drill (repo2)" /var/lib/stellarindex/restore-drills/restore-drills.md | tail -n 3

# 4. repo2 metric present + fresh (own file, repo="2")
cat /var/lib/node_exporter/textfile_collector/restore_drill_repo2.prom

# 5. repo2 healthy and reachable?
sudo -u postgres pgbackrest --stanza=stellarindex info --repo=2
```

**Likely causes**

- Timer not on the host (`could not be found`; units ship with `--tags ops-jobs`, gated on `pgbackrest_repo2_s3_bucket`), or disabled (`inactive`).
- S3 failure (rotated credentials, endpoint/TLS, missing cipher pass): `pgbackrest info --repo=2` errors (`403`, `AccessDenied`, `unable to resolve`, cipher); the drill records `ABORTED at pg_restore` and `stellarindex_restore_drill_failed{repo="2"}` also fires.
- Refusal on free space, `/run/lock/restore-drill.lock` held, or a postgres already on scratch port 5499 (orphan of a killed drill; the log prints the `pg_ctl -D ... stop -m immediate` for each directory under `/srv/restore-drill`).
- No repo2 on this host (no `repo2-*` lines in `/etc/pgbackrest/pgbackrest.conf`), or a pre-`repo`-label script wrote an un-labelled series into `restore_drill.prom`.

**Fix**

1. Timer missing: from `configs/ansible`, `--check --diff` first, then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --diff --tags ops-jobs` (`-e ansible_python_interpreter=/usr/bin/python3` on a tag-limited run). Disabled: `sudo systemctl enable --now restore-drill-offsite.timer`.
2. S3 failure: [backup-offsite-stale](backup-offsite.md#stellarindex_backup_offsite_stale) (its credential/endpoint/cipher table applies to restore).
3. Refusal: free space, stop the orphaned cluster with the printed command and remove its directory, or wait for the other drill. A refused cycle is NOT retried until the next 15th (a month of proof): do not wait.
4. No repo2: provision it (`docs/operations/off-site-backup-plan.md`); do NOT silence the alert.
5. Force: `sudo systemctl start restore-drill-offsite.service` (~1 h, ~$24 of S3 egress at current size). Confirm `restore_drill_repo2.prom` carries a fresh `last_success_unix{repo="2"}`.

## stellarindex_restore_drill_failed

`stellarindex_restore_drill_failures > 0` for 30 min, per `repo` label (manual runs too). The latest run ran and did NOT prove the chain restorable; it is the current run's verdict (the textfile is rewritten on every run past its preconditions, including pg_restore / pg_start aborts). A failed run never stamps `last_success_unix`, so the stale alert follows in 35-40 d if nothing changes. A capacity refusal (exit 2) writes nothing, so it surfaces only via the stale alerts.

**Check**

```sh
# 1. Which stage failed? The evidence log names it.
sudo tail -n 12 /var/lib/stellarindex/restore-drills/restore-drills.md

# 2. Full run output (pgbackrest / pg_ctl diagnostics); repo2: /var/log/restore-drill-offsite.log
sudo tail -n 200 /var/log/restore-drill.log
sudo journalctl -u restore-drill.service -n 200   # repo2: -u restore-drill-offsite.service

# 3. The metric as scraped (repo2: restore_drill_repo2.prom)
cat /var/lib/node_exporter/textfile_collector/restore_drill.prom

# 4. Is the backup chain itself healthy? (repo2: add --repo=2)
sudo -u postgres pgbackrest --stanza=stellarindex info
```

**Likely causes**

- `ABORTED at pg_restore`: pgbackrest could not restore the latest backup (repo unreadable, checksum/manifest error, ENOSPC); the partial datadir is removed. Run `pgbackrest --stanza=stellarindex check`; repo2: triage under [stellarindex_restore_drill_offsite_stale](#stellarindex_restore_drill_offsite_stale); a broken chain goes to `infra.md#stellarindex_timescale_backup_none_24h`.
- `ABORTED at pg_start`: no consistency within `PG_START_TIMEOUT` (7200 s): a WAL gap, or mirrored GUCs (`max_connections`, `max_locks_per_transaction`, ...) differ from the primary. The datadir is KEPT under `/srv/restore-drill/pgdata-*`; read `pgdata-*/log/`, then delete it.
- A check failed (`core_tables`, `wal_drain`, `tip_lag`, `hash_chain_sample`, `trades_window_match`, `ch_rederive`); the log line `FAIL <check> — <detail>` names it, datadir kept. `wal_drain`/`tip_lag` point at WAL archiving, `hash_chain_sample`/`trades_window_match` at data integrity.
- `wal_drain` after a long WAL span: the drill restored an older backup. It waits up to `BACKUP_WAIT_TIMEOUT` (6 h) for `pgbackrest-backup.service`; check `pgbackrest info` for which backup it took.
- `wal_drain` fails when replay stops advancing for `WAL_DRAIN_STALL_TIMEOUT` (30 min), or misses the 8 h `WAL_DRAIN_TIMEOUT` backstop. A slow drain is not a failure: the log's `after <n>s` says how long it took. A stall names the LSN it stopped at; `pgbackrest archive-get` of the next segment shows whether the archive has a hole.
- Evidence unwritable: fix ownership of `/var/lib/stellarindex/restore-drills`.

**Fix** Apply the matching fix and re-run (`sudo systemctl start restore-drill.service`, or `restore-drill-offsite.service` for repo2). Confirm the `.prom` carries `stellarindex_restore_drill_failures 0` and a fresh `last_success_unix`.

## Why the scheduled drill never ran until 2026-09

Three blockers stacked, each hidden by the one before. `PrivateTmp=true` hid the `/var/tmp` drill dataset inside the service namespace (`226/NAMESPACE`, before `ExecStart`). `NoNewPrivileges=true` then blocked `sudo` dropping privilege to postgres. Then pgbackrest-as-postgres could not traverse `/var/lib/stellarindex`. The dataset moved to the postgres-owned `/srv/restore-drill` and the drill runs on its own unit. Every passing record before that came from manual runs, which have no namespace. A unit test that runs the script outside systemd cannot see any of the three.

## Related

[dr-activation](dr-activation.md), [backup-offsite-stale](backup-offsite.md#stellarindex_backup_offsite_stale), `infra.md#stellarindex_timescale_backup_none_24h`, `infra.md#stellarindex_zfs_pool_low_space`, `ch-schema-restore.md`, `docs/adr/0043-backup-and-restore-strategy.md`.
