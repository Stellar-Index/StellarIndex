---
title: Runbook — restore-drill
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — restore-drill alerts

All alerts are `severity: ticket` (P3), `component: storage`. Rules: `configs/prometheus/rules.r1/restore-drill.yml` (also `deploy/monitoring/rules/restore-drill.yml`); `stellarindex_restore_drill_textfile_stale` lives in `storage.yml`. Impact of all of them: no *current* proof the pgBackRest backup chain is restorable (ADR-0043 §3 / CS-110). The backups may be fine; the alerts say we cannot presently prove it.

The drill (`scripts/ops/restore-drill.sh`) is a non-destructive scratch restore of the pgBackRest stanza with WAL replay and verification. Two monthly timers run the same script, one drill at a time:

| Timer | Repo | Schedule | Log | Textfile (`/var/lib/node_exporter/textfile_collector/`) |
| ----- | ---- | -------- | --- | ------- |
| `restore-drill.timer` | repo1 (on-box) | first Saturday 04:00 UTC | `/var/log/restore-drill.log` | `restore_drill.prom` |
| `restore-drill-offsite.timer` | repo2 (encrypted S3) | the 15th 04:00 UTC | `/var/log/restore-drill-offsite.log` | `restore_drill_repo2.prom` |

- Every series carries a `repo` label. The drill takes any positive-integer `DRILL_REPO` and writes `restore_drill_repo<N>.prom`.
- `stellarindex_restore_drill_last_success_unix` is stamped ONLY on a fully clean run (zero failed checks AND the evidence log written). A failed run rewrites the textfile without it, so the series goes absent, not old; the `absent_over_time` branches of the stale alerts cover that.
- Durable evidence log (names the repo per entry): `/var/lib/stellarindex/restore-drills/restore-drills.md`; figures in `docs/operations/drills/restore-drills.md`.
- Full drill: up to ~4 h for repo1 (scratch restore + WAL replay), ~1 h for repo2. Force a run once the cause is cleared: `sudo systemctl start restore-drill.service` (repo1) or `restore-drill-offsite.service` (repo2). Watch the log.
- Capacity floor: database size from the latest backup x 125 % + 50 G WAL headroom, never below `MIN_FREE_GB`=200 G. Below it the drill exits 2 (uncounted precondition: no evidence, no metric, `failures` keeps the previous run's value). Free space on the drill dataset (Phase-A capacity runbook), then force a run.
- Evidence unwritable counts as a failure by design (the evidence is the deliverable): log ends `FATAL: could not write drill evidence`; fix ownership/permissions on `/var/lib/stellarindex/restore-drills`, then force a run.
- node_exporter must point `--collector.textfile.directory` at `/var/lib/node_exporter/textfile_collector`; a fresh `.prom` mtime with no samples in Prometheus means it does not.
- Lake RTO is a measured number (#343): `restore-drill.service` (repo1 only; the off-site unit deliberately leaves it unset) sets `DRILL_CH_WINDOW=100000`, so every scheduled repo1 drill also fetch+decodes a 100k-ledger window from the archive in dry-run (ADR-0043 §2.2) and writes `stellarindex_restore_drill_ch_rederive_seconds`, `..._ch_rederive_window_ledgers`, `..._ch_rederive_ledgers_per_second`. Full-lake re-derive time ~ live tip / `ledgers_per_second` / parallelism. The series is absent when the CH stage did not run or failed: treat `absent()` after a drill as "unmeasured", never "fast".
- Both drill units stay under the catch-all `stellarindex_systemd_unit_failed` (decided 2026-09-04; deliberately NOT in `scripts/ci/unit-failed-dedicated.baseline`). The failure class that hid this drill for months (BDR-04: `226/NAMESPACE`, a unit that never reaches `ExecStart`) writes no evidence and no metric, so none of the drill alerts can see it; the catch-all's 15 min is the only fast signal, against 35-40 days from the staleness tickets. A refusal (exit 2) leaves the unit `failed` until cleared; on the monthly off-site unit an uncleared ticket stands for a month (`sudo systemctl reset-failed restore-drill-offsite.service`; same for `restore-drill.service`), which does not touch the drill's verdict. Noise is reduced at the source: `restore-drill-offsite.service` is `After=restore-drill.service` (ordering only) so missed slots run one after the other instead of racing for the lock, and the archival-node role clears stale failed state on both units at apply time (`--tags ops-jobs`).
- Fresh node bring-up: a new archival node has never run the drill, so the stale tickets fire until the first drill lands. That is why they are P3 tickets, not pages: run the drill once by hand to seed the evidence and clear them.

## At a glance

- [`stellarindex_restore_drill_stale`](#stellarindex_restore_drill_stale): repo1 (`repo=~"1|"`) no success in 40 d.
- [`stellarindex_restore_drill_textfile_stale`](#stellarindex_restore_drill_textfile_stale): mtime backstop, 35 d.
- [`stellarindex_restore_drill_offsite_stale`](#stellarindex_restore_drill_offsite_stale): repo2 no success in 35 d.
- [`stellarindex_restore_drill_failed`](#stellarindex_restore_drill_failed): the last run recorded failures > 0 (per repo).

## stellarindex_restore_drill_stale

Typical MTTR: 30 min to confirm cause + up to 4 h (a full drill run).

Fires when `(time() - stellarindex_restore_drill_last_success_unix{repo=~"1|"}) > 40 * 24 * 3600` for 30 min, or the series is absent for 40 d (`absent_over_time(...{repo=~"1|"}[40d])`): no fully-successful monthly drill in over 40 days (one missed cycle plus slack), or none ever.

- `repo=~"1|"` is the on-box repo1 ticket; repo2 has its own ([below](#stellarindex_restore_drill_offsite_stale)). It is an ALLOW-LIST (repo1, or no `repo` label), deliberately not `repo!="2"`: a negative matcher lets any other repo satisfy the absent branch, and one `repo="3"` sample held this ticket silent through a never-drilled repo1.
- **The empty alternative is a transition allowance, and on r1 it currently tolerates repo2's verdict, not repo1's.** The pre-label `restore_drill.prom` is rewritten whole by whichever drill ran LAST, and that was the 2026-09-03 repo2 hand-run, so the un-labelled series most likely carries the OFF-SITE result and this ticket reads it as repo1's proof of health. The window closes at the first labelled repo1 run (`restore-drill.timer`, first Saturday), which rewrites that file with `repo="1"` on every series; force it sooner with `sudo systemctl start restore-drill.service`. Until then treat a green `stellarindex_restore_drill_stale` as unproven for repo1 and read the evidence log. (Dropping the empty alternative would ticket from rule deploy until that run, for a drill that had not stopped.)

Quick diagnosis (5 min):

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

Typical root causes:

1. **Timer disabled** (stopped during maintenance, not re-enabled). Signal: `systemctl status restore-drill.timer` shows `inactive`. Fix: `sudo systemctl enable --now restore-drill.timer`.
2. **Every run refusing on free space.** Signal: log ends with a `refusing` note. Fix: free space (see shared notes), force a run.
3. **Every run failing a verification check** (`fail_count > 0`, last_success deliberately not stamped, series goes absent). `stellarindex_restore_drill_failed` fires within 30 min of that run; this ticket is the 40-day backstop. Signal: `restore_drill.prom` has `stellarindex_restore_drill_failures > 0` and no `..._last_success_unix` line; the log names the failing check. Fix: triage under [stellarindex_restore_drill_failed](#stellarindex_restore_drill_failed) and `infra.md#stellarindex_timescale_backup_none_24h`.
4. **Evidence unwritable.** Signal: `FATAL: could not write drill evidence`. Fix: see shared notes.
5. **node_exporter not scraping the textfile dir.** Signal: file has a fresh mtime but the gauge has no samples in Prometheus. Fix: see shared notes.

Mitigation: walk the diagnostics, apply the matching fix, force a run (`sudo systemctl start restore-drill.service`, up to ~4 h, watch `/var/log/restore-drill.log`). Verification: `restore_drill.prom` carries a fresh `stellarindex_restore_drill_last_success_unix`, the evidence log has a new dated entry, and the alert clears within ~5 min.

## stellarindex_restore_drill_textfile_stale

Companion to the above, in `storage.yml`. The generic `stellarindex_textfile_producer_stale` catch-all excludes `restore_drill*.prom` (its 24 h threshold false-fires against a monthly timer); this rule carries the right threshold: `time() - node_textfile_mtime_seconds{file=~".*/restore_drill.*\\.prom$"} > 35d`, `for: 30m`, `severity: ticket`. It is a pure mtime backstop (same OBS-2 pattern as `stellarindex_patroni_textfile_stale`) for the case the timer dies before ever writing a fresh gauge; `stellarindex_restore_drill_stale` (the content signal) is the primary alert and fires first in every other scenario. Diagnose and fix as for the repo1 or repo2 stale alert, according to the `file` label (`restore_drill.prom` = repo1, `restore_drill_repo2.prom` = repo2).

## stellarindex_restore_drill_offsite_stale

Typical MTTR: 30 min to confirm cause + ~1 h (a full off-site drill: ~47 min restore + WAL drain).

Same ticket as [stellarindex_restore_drill_stale](#stellarindex_restore_drill_stale) for the OFF-SITE copy: `(time() - stellarindex_restore_drill_last_success_unix{repo="2"}) > 35 * 24 * 3600` for 30 min, or the `repo="2"` series absent for 35 d (one monthly cycle on a fixed day plus >= 4 days of slack, under two cycles). The alert carries `repo="2"` on both branches.

Impact: repo1 lives on the same disks as the database and dies with the pool; repo2 is the backup a host- or pool-loss DR actually restores from (ADR-0043 §1, [dr-activation](dr-activation.md)). A green repo1 drill says nothing about it.

Why it exists: before 2026-09-04 repo2 had been restored once, by hand (2026-09-03: restore 2813 s, tip lag 17 ledgers, hash-chain breaks 0, trades window match 2,726,303 = 2,726,303, failures 0; RTO ~47 min against ~9 min from repo1; ~269 GB of S3 egress at ~$24). `restore-drill.timer` drilled repo1 only and wrote one un-labelled textfile that whichever run came last overwrote, so nothing scheduled the next off-site drill. Now `restore-drill-offsite.timer` runs the script with `DRILL_REPO=2` on the 15th and each repo has its own labelled textfile.

Quick diagnosis (5 min):

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

Typical root causes:

1. **Timer not on the host.** Units are codified in the archival-node role under `--tags ops-jobs`, gated on `pgbackrest_repo2_s3_bucket`; an ansible commit does not reach the box on its own. Signal: `systemctl status restore-drill-offsite.timer` reports `could not be found`. Fix: from `configs/ansible`, `--check --diff` first, then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --diff --tags ops-jobs` (`-e ansible_python_interpreter=/usr/bin/python3` on a tag-limited run). Then force a run rather than waiting for the 15th.
2. **Timer disabled.** Signal: `inactive`. Fix: `sudo systemctl enable --now restore-drill-offsite.timer`.
3. **Every run failing against S3** (expired/rotated repo2 credentials, endpoint/TLS problem, missing cipher pass). The drill aborts at `pg_restore`, records `ABORTED at pg_restore` with `failures: 1`, and `stellarindex_restore_drill_failed{repo="2"}` fires within 30 min; this ticket is the 35-day backstop. Signal: `pgbackrest info --repo=2` errors (`403`, `AccessDenied`, `unable to resolve`, cipher); the log shows pgbackrest output in full. Fix: [backup-offsite-stale](backup-offsite.md#stellarindex_backup_offsite_stale), whose credential/endpoint/cipher table applies to restore as to backup.
4. **Every run refusing on free space, the lock, or the scratch port.** Besides the capacity floor (sized from the latest repo2 backup), the drill exits 2 when another drill holds `/run/lock/restore-drill.lock`, when that lock file cannot be opened, and when a postgres already answers on scratch port 5499 (leftovers of a drill killed after `pg_start`; the lock does NOT cover this because it dies with the shell that held it). Signal: log ends with a `refusing` note naming free space, lock or port; a port refusal prints the exact `pg_ctl -D ... stop -m immediate` for each orphaned data directory under `/srv/restore-drill`. Fix: free space; or stop the orphaned cluster with the printed command and remove its directory; or wait for the other drill. Then force a run. **A refused cycle is NOT retried:** `Persistent=true` fires the missed run once, that run is the one refused, and the timer waits for the next 15th, so a refusal costs a month of proof (what the 35-day threshold is sized to catch). Do not wait for the next tick.
5. **No repo2 configured on this host.** The role removes the off-site timer where `pgbackrest_repo2_s3_bucket` is unset, so the series is absent by construction and the ticket fires permanently. Signal: no `repo2-*` lines in `/etc/pgbackrest/pgbackrest.conf`. Fix: provision repo2 (`docs/operations/off-site-backup-plan.md`). Do NOT silence the alert: it prices the missing survival copy, as `stellarindex_backup_offsite_stale` does.
6. **node_exporter not scraping, or a script from before the `repo` label.** The run succeeded but wrote an un-labelled series into `restore_drill.prom`, which `repo="2"` cannot select. Signal: `restore_drill_repo2.prom` missing while `restore_drill.prom` carries a fresh un-labelled `last_success_unix` and the evidence log says `(repo2)`. Fix: re-apply `--tags ops-jobs` so `/usr/local/bin/restore-drill.sh` is the labelled version, then force a run.

Mitigation: walk the diagnostics, apply the matching fix, force an off-site drill: `sudo systemctl start restore-drill-offsite.service`. Budget ~1 h and ~$24 of S3 egress (~269 GB at the 2026-09-03 size; scales with the database). Verification: `restore_drill_repo2.prom` carries a fresh `stellarindex_restore_drill_last_success_unix{repo="2"}`, the evidence log has a new `restore drill (repo2)` entry, and the alert clears within ~5 min.

Known false positives: **first deploy of the rule.** It fires from rule deploy (via `deploy.yml`; the units and labelled script do not deploy with it) until the first labelled repo2 run lands; the 2026-09-03 hand-run wrote an un-labelled series and does not count. Seed it: apply `--tags ops-jobs`, force a run. Fresh node bring-up: same remedy.

## stellarindex_restore_drill_failed

Typical MTTR: 30 min to read the evidence + up to 4 h (a clean re-run).

Fires on `stellarindex_restore_drill_failures > 0` for 30 min, per `repo` label (`repo="1"` on-box, `repo="2"` off-site; manual runs too). The latest run ran and did NOT prove the chain restorable. Unlike the stale alerts, this is the current run's verdict: the script rewrites the textfile on every run past its preconditions, including the pg_restore / pg_start abort paths (`restore-drill-run-test.sh` pins that in CI). Before 2026-08-28 those aborts exited before the evidence phase and the previous month's clean textfile kept being scraped.

Symptoms: the tail of the evidence log names an aborted stage (`ABORTED at pg_restore` / `ABORTED at pg_start`) or lists `failures: N` after a full verification pass; `..._last_success_unix` is absent (a failed run never stamps it), so the stale alert follows in <= 35-40 d if nothing changes.

Quick diagnosis (5 min):

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

Typical root causes:

1. **`ABORTED at pg_restore`**: pgbackrest could not restore the latest backup (repo unreadable, checksum/manifest error, or ENOSPC). The partial datadir is removed automatically; the pgbackrest output in the log is the diagnostic. Fix: `pgbackrest --stanza=stellarindex check`; for repo2 confirm credentials and reachability and triage S3 credential/endpoint/cipher causes under [stellarindex_restore_drill_offsite_stale](#stellarindex_restore_drill_offsite_stale); route to `infra.md#stellarindex_timescale_backup_none_24h` if the chain itself is broken.
2. **`ABORTED at pg_start`**: the restored cluster never reached consistency within `PG_START_TIMEOUT` (7200 s): a missing WAL segment in the archive, or the mirrored GUCs (`max_connections`, `max_locks_per_transaction`, ...) no longer match the primary. The datadir is KEPT under `/srv/restore-drill/pgdata-*`; delete it once read. Fix: read `pgdata-*/log/` or the unit log for the recovery error; `pgbackrest --stanza=stellarindex info` plus the archive check in `infra.md#stellarindex_timescale_backup_none_24h` for a WAL gap.
3. **A verification check failed** (`core_tables`, `wal_drain`, `tip_lag`, `hash_chain_sample`, `trades_window_match`, `ch_rederive`): restore worked but the copy disagrees with the live DB or the archive stream did not drain. The log line `FAIL <check> — <detail>` says which; the datadir is kept. `wal_drain`/`tip_lag` point at WAL archiving, `hash_chain_sample`/`trades_window_match` at data integrity.
4. **Evidence unwritable**: all checks passed but the evidence log could not be written; counted as a failure. Fix ownership of `/var/lib/stellarindex/restore-drills`.

Mitigation: read the evidence entry and log, apply the matching fix, re-run (`sudo systemctl start restore-drill.service`, or `restore-drill-offsite.service` for repo2). Verification: the `.prom` carries `stellarindex_restore_drill_failures 0` and a fresh `last_success_unix`; the alert clears within ~5 min of the scrape.

Known false positive (inverse): a capacity refusal (exit 2) writes neither evidence nor metric, so `failures` keeps the previous run's value; that case surfaces only through the stale alerts. If this alert fires, the drill ran.

## Related

- [dr-activation](dr-activation.md): the procedure that restores from repo2 for real; the drill is its monthly rehearsal.
- [backup-offsite-stale](backup-offsite.md#stellarindex_backup_offsite_stale): the repo2 backup stream; the offsite drill proves its copy restores.
- `infra.md#stellarindex_timescale_backup_none_24h`: the backup chain itself; `infra.md#stellarindex_zfs_pool_low_space`: the drill restores onto the shared pool, hence the backup-derived capacity floor.
- `ch-schema-restore.md`: the ClickHouse half of a restore; the optional CH re-derive stage exercises the same path.
- `docs/adr/0043-backup-and-restore-strategy.md`: §1 (repo2, drilled monthly), §3 (drill logging is append-only evidence).

## Why the scheduled drill never ran until 2026-09

Three blockers stacked, each hidden by the one before. `PrivateTmp=true` hid the `/var/tmp` drill dataset inside the service namespace (`226/NAMESPACE`, before `ExecStart`). `NoNewPrivileges=true` then blocked `sudo` dropping privilege to postgres. Then pgbackrest-as-postgres could not traverse `/var/lib/stellarindex`. The dataset moved to the postgres-owned `/srv/restore-drill` and the drill runs on its own unit. Every passing record before that came from manual runs, which have no namespace. A unit test that runs the script outside systemd cannot see any of the three. Source: `git show 52aacb972:docs/operations/v1-launch-plan.md`, lines 1270-1280.
