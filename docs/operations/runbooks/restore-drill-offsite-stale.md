---
title: Runbook — restore-drill-offsite-stale
last_verified: 2026-09-04
status: ratified
severity: P3
---

# Runbook — `stellarindex_restore_drill_offsite_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_restore_drill_offsite_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/restore-drill.yml` (mirrored in `configs/prometheus/rules.r1/restore-drill.yml`) |
| Typical MTTR | 30 min (to confirm cause) + ~1 h (a full off-site drill: ~47 min restore + WAL drain) |
| Impact | We have no *current* proof that the OFF-SITE pgBackRest copy (repo2, encrypted S3) is restorable. repo1 lives on the same disks as the database and dies with the pool; repo2 is the backup a host- or pool-loss DR actually restores from (ADR-0043 §1, [dr-activation](dr-activation.md)). A green `restore-drill-stale` says nothing about it. |

## Why this exists

Before 2026-09-04 the off-site copy had been restored exactly once, by
hand (2026-09-03: restore 2813 s, tip lag 17 ledgers, hash-chain breaks
0, trades window match 2,726,303 = 2,726,303, failures 0 — an RTO of
~47 min against ~9 min from repo1, for ~269 GB of S3 egress at ~$24).
`restore-drill.timer` drills repo1 only, and the drill wrote one
un-labelled textfile that whichever run came last overwrote, so nothing
scheduled the next off-site drill and nothing could tell if one never
ran. `restore-drill-offsite.timer` now runs the same script with
`DRILL_REPO=2` on the 15th of each month; the drill writes one textfile
per repo with a `repo` label on every series; this ticket reads
`repo="2"`.

## Symptoms

- `(time() - stellarindex_restore_drill_last_success_unix{repo="2"}) > 35 * 24 * 3600`
  for ≥ 30 min, **or** the `repo="2"` series is absent for ≥ 35 d
  (`absent_over_time(...{repo="2"}[35d])`).
- No fully-successful off-site drill has been recorded in over 35 days
  (one monthly cycle on a fixed day of month plus ≥ 4 days of slack), or
  none has ever been recorded with the label.
- The alert carries `repo="2"` on both branches.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is the monthly timer scheduled + when did it last fire?
sudo systemctl status restore-drill-offsite.timer
sudo systemctl list-timers restore-drill-offsite.timer

# 2. What did the last run do? (own log file, kept apart from repo1's)
sudo tail -n 100 /var/log/restore-drill-offsite.log
sudo journalctl -u restore-drill-offsite.service --since "40 days ago" -n 200

# 3. Is the durable evidence log being written? Entries name the repo.
sudo grep -n "restore drill (repo2)" /var/lib/stellarindex/restore-drills/restore-drills.md | tail -n 3

# 4. Is the repo2 metric present + fresh? (its own file, repo="2")
cat /var/lib/node_exporter/textfile_collector/restore_drill_repo2.prom

# 5. Is repo2 itself healthy and reachable?
sudo -u postgres pgbackrest --stanza=stellarindex info --repo=2
```

## Typical root causes

1. **Timer not on the host** — the units are codified in the
   archival-node role under `--tags ops-jobs` and gated on
   `pgbackrest_repo2_s3_bucket`; an ansible commit does not reach the box
   on its own.
   - Signal: `systemctl status restore-drill-offsite.timer` reports
     `could not be found`.
   - Mitigation: from `configs/ansible`, `--check --diff` first, then
     `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --diff --tags ops-jobs`
     (`-e ansible_python_interpreter=/usr/bin/python3` on a tag-limited
     run). Then force a run (below) rather than waiting for the 15th.

2. **Timer disabled** — stopped during capacity or maintenance work and
   not re-enabled.
   - Signal: `systemctl status restore-drill-offsite.timer` shows `inactive`.
   - Mitigation: `sudo systemctl enable --now restore-drill-offsite.timer`.

3. **Every run failing against S3** — expired or rotated repo2
   credentials, an endpoint/TLS problem, or a missing cipher pass. The
   drill aborts at `pg_restore`, records `ABORTED at pg_restore` with
   `failures: 1`, and `stellarindex_restore_drill_failed{repo="2"}` fires
   within 30 min of that run; this ticket is the 35-day backstop.
   - Signal: `pgbackrest info --repo=2` errors (`403`, `AccessDenied`,
     `unable to resolve`, cipher); `/var/log/restore-drill-offsite.log`
     shows the pgbackrest output in full.
   - Mitigation: route to [backup-offsite-stale](backup-offsite-stale.md)
     — the credential / endpoint / cipher table there is the same for a
     restore as for a backup.

4. **Every run refusing on free space, the lock, or the scratch port** —
   the drill sizes its floor from the latest repo2 backup (database size
   × 125 % + 50 G WAL headroom, never below 200 G) and exits 2 (uncounted
   precondition) below that; it also refuses (exit 2) when another drill
   holds `/run/lock/restore-drill.lock`, when that lock file cannot be
   opened at all, and when a postgres is already answering on the scratch
   port 5499 — the leftovers of a drill killed after `pg_start`, which
   the lock does NOT cover because the lock dies with the shell that held
   it.
   - Signal: `/var/log/restore-drill-offsite.log` ends with a `refusing`
     note naming the free space, the lock or the port. A port refusal
     also prints the exact `pg_ctl -D … stop -m immediate` for each
     orphaned data directory it found under `/srv/restore-drill`.
   - Mitigation: free space on the drill dataset (Phase-A capacity
     runbook); or stop the orphaned cluster with the printed command and
     remove its directory; or wait for the other drill to finish. Then
     force a run.
   - **A refused cycle is NOT retried.** `Persistent=true` fires the
     missed run once and that run is the one that gets refused; the timer
     then waits for the next 15th. Nothing re-queues it, so a refusal
     costs a full month of proof — which is exactly what the 35-day
     threshold on this ticket is sized to catch. Do not wait for the next
     tick: force the run once the cause is cleared.

5. **No repo2 configured on this host** — the role removes the off-site
   timer where `pgbackrest_repo2_s3_bucket` is unset, so the series is
   absent by construction and this ticket fires permanently.
   - Signal: no `repo2-*` lines in `/etc/pgbackrest/pgbackrest.conf`.
   - Mitigation: provision repo2 (`docs/operations/off-site-backup-plan.md`).
     Do NOT silence the alert: it is pricing the missing survival copy,
     exactly as `stellarindex_backup_offsite_stale` does.

6. **node_exporter not scraping the textfile dir**, or a script from
   before the `repo` label — the run succeeded but wrote an un-labelled
   series into `restore_drill.prom`, which `repo="2"` cannot select.
   - Signal: `restore_drill_repo2.prom` is missing while `restore_drill.prom`
     carries a fresh un-labelled `last_success_unix` and the evidence log
     says `(repo2)`.
   - Mitigation: re-apply `--tags ops-jobs` so `/usr/local/bin/restore-drill.sh`
     is the labelled version, then force a run.

## Mitigation

- [ ] Step 1 — Walk the diagnostic commands; identify which stage is
      silent.
- [ ] Step 2 — Apply the matching fix from "Typical root causes."
- [ ] Step 3 — Force an off-site drill:
      `sudo systemctl start restore-drill-offsite.service`. Budget ~1 h
      and ~$24 of S3 egress (~269 GB at the 2026-09-03 size; the price
      scales with the database). Watch `/var/log/restore-drill-offsite.log`.
- [ ] Verification: after a clean run, `restore_drill_repo2.prom` carries
      a fresh `stellarindex_restore_drill_last_success_unix{repo="2"}`,
      the evidence log has a new `restore drill (repo2)` entry, and the
      alert clears within ~5 min.

## The unit is left `failed` by a refusal — deliberately

A refusal (exit 2) leaves `restore-drill-offsite.service` in `failed`,
and systemd keeps that state until something clears it. The catch-all
`stellarindex_systemd_unit_failed` tickets it after 15 min, and on a
monthly unit the ticket then stands for a month.

**Decided 2026-09-04: both drill units stay under the catch-all** — they
are deliberately NOT in `scripts/ci/unit-failed-dedicated.baseline`. The
failure class that hid this drill for months (BDR-04: `226/NAMESPACE`, a
unit that never reaches `ExecStart`) writes no evidence and no metric, so
none of the three drill alerts can see it; the catch-all's 15 min is the
only fast signal for it, against 35–40 days from the staleness tickets.
The noise is the acceptable half of that trade, and it is reduced at the
source rather than suppressed:

- `restore-drill-offsite.service` is ordered `After=restore-drill.service`
  (ordering only, no dependency), so a host that missed both monthly slots
  runs the two catch-ups one after the other instead of racing for the
  lock — the collision that produced the refusal in the first place.
- The archival-node role clears a stale failed state on both drill units
  at apply time (`--tags ops-jobs`), matching what the remove block has
  always done.

Clearing it by hand is one line, and does not touch the drill's verdict
(which lives in the evidence log and the textfile metric):

```sh
sudo systemctl reset-failed restore-drill-offsite.service
```

## Known false-positive patterns

- **First deploy of the rule** — the ticket fires from the moment the
  rules ship (they deploy with `deploy.yml`; the units and the labelled
  script do not) until the first labelled repo2 run lands. The
  2026-09-03 hand-run wrote an un-labelled series and does not count.
  Seed it: apply `--tags ops-jobs`, then force a run as above. It is
  deliberately a P3 ticket, not a page, for exactly this reason.
- **Fresh node bring-up** — a newly deployed archival node has never run
  the off-site drill; same remedy.

## Related

- [restore-drill-stale](restore-drill-stale.md) — the on-box repo1
  drill's staleness ticket (`repo=~"1|"`, 40 d); this runbook is its
  off-site twin. Both timers run `scripts/ops/restore-drill.sh`.
- [restore-drill-failed](restore-drill-failed.md) —
  `stellarindex_restore_drill_failed`, per repo; the immediate signal when
  the most recent run of either drill recorded failures > 0.
- [backup-offsite-stale](backup-offsite-stale.md) — the repo2 *backup*
  stream; this ticket proves the copy that stream writes actually
  restores.
- [dr-activation](dr-activation.md) — the procedure that restores from
  repo2 for real; the drill is its monthly rehearsal.
- `docs/operations/drills/restore-drills.md` — the evidence log, and the
  2026-09-03 off-site drill's figures.
- `docs/adr/0043-backup-and-restore-strategy.md` — §1 (repo2, drilled
  monthly) and §3 (drill logging is append-only evidence).

## Changelog

- 2026-09-04 — initial draft alongside `restore-drill-offsite.{service,timer}`,
  the per-repo `repo` label on the drill's textfile metric, and this alert.
  Records the catch-all decision above, the scratch-port pre-flight, and
  that a refused cycle waits a month.
