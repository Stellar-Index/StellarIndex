---
title: Runbook — root filesystem low or full
last_verified: 2026-09-16
status: current
---

# `stellarindex_root_filesystem_low_space` / `_critically_low_space`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_root_filesystem_low_space` (P3, ticket) · `stellarindex_root_filesystem_critically_low_space` (P1, page) |
| Severity | P1 (critical) / P3 (low) |
| Detected by | Prometheus rule in `configs/prometheus/rules.r1/infra.yml` (R1 overlay) |
| Typical MTTR | 5–20 min (reclaim), longer if Postgres already failed |
| Impact | Root carries `/usr`, `/var/log`, the swapfile and — via a symlink out of the data directory — Postgres' `pg_wal`. At zero bytes Postgres crashes **and cannot complete crash recovery**, taking every Postgres-dependent unit with it. |

## What fired

Free space on a non-pool filesystem (`ext4`/`xfs`/`btrfs`) fell below
**8 GB** (ticket) or **3 GB** (page). On this fleet that is the ~49 GB root
volume — not the ZFS data pool, which has its own alerts with capacity-aware
thresholds.

## Why it pages

Root is small and shared: `/usr`, `/var/log`, a swapfile, and — via a symlink
out of the Postgres data directory — **`pg_wal`**. At zero bytes Postgres does
not merely stop accepting writes: it crashes AND cannot complete crash
recovery, because recovery has to create new WAL segments. It then shuts down
rather than come up inconsistent.

That is not hypothetical. See
[the 2026-09-16 post-mortem](../postmortems/2026-09-16-r1-pg-wal-fills-root.md):
three hours of API 503s, eighteen units failed behind it, and the first signal
was an unrelated deploy step failing on `connection refused`.

## Triage

```bash
df -h /                                    # how bad
du -x -h -d1 / 2>/dev/null | sort -rh | head -12
readlink -f /var/lib/postgresql/15/main/pg_wal   # is WAL on this volume?
du -sh "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
```

`du -x` stays on one filesystem, which matters here: without it the pool's
terabytes drown out the volume actually filling.

If the children do not sum to the used total, look for a large file directly
under `/` (a swapfile) or a deleted-but-open file:

```bash
find / -xdev -type f -size +500M -printf '%s %p\n' | sort -rn | head
lsof -nP +L1 | awk '$7>1073741824'
```

## Reclaim, in order of safety

1. **Rotated logs older than `.1`** — compressed and weeks old:
   `rm -f /var/log/postgresql/postgresql-*-main.log.[5-9].gz` and the
   `*.1` files under `/var/log/pgbackrest/`.
2. **Journal:** `journalctl --vacuum-size=200M`.
3. **Crash dumps:** `/var/crash/*.crash` — check the date first; a recent one
   may still be wanted.

Do **not** delete anything inside `pg_wal` by hand. Postgres owns that
directory and removing a segment it still needs is unrecoverable.

## If `pg_wal` is the growth

Check `max_wal_size` against **this volume**, not against the pool the data
directory is on:

```bash
grep max_wal_size /etc/postgresql/15/main/postgresql.conf
df -h "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
```

Lowering it is a `SIGHUP` (`systemctl reload postgresql@15-main`) and Postgres
trims the directory at the next checkpoints — on 2026-09-16 that returned
7.7 GB within minutes. Then fix it in ansible, or the next apply pushes the
bad value straight back; `roles/archival-node/tasks/05-postgres.yml` now
refuses an apply that does not fit, so a repo change is the durable route.

## If Postgres is already down

```bash
systemctl status postgresql@15-main
tail -40 /var/log/postgresql/postgresql-15-main.log
```

`could not write to file "pg_wal/xlogtemp.N": No space left on device` during
startup is this failure. Free space first, then `systemctl start
postgresql@15-main` and watch recovery complete. Clear the units that failed
behind it with `systemctl reset-failed` once the database is up.

Check the archive backlog before assuming loss:

```bash
ls "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"/archive_status | grep -c ready
```

Zero `.ready` files means every segment reached the repository and nothing is
waiting to be shipped.

## The durable fix

`pg_wal` should not be on root. The data pool has terabytes free; moving it
there removes the whole class, and is tracked in the post-mortem's open items.

## Related

- Post-mortem: [`2026-09-16-r1-pg-wal-fills-root.md`](../postmortems/2026-09-16-r1-pg-wal-fills-root.md)
  — the outage this alert exists because nobody had.
- Companion runbook: [`zfs-pool-full.md`](zfs-pool-full.md) — the same question
  for the DATA POOL, which is large, capacity-aware and alerted as a
  percentage. This one is small and alerted in absolute bytes, and the two are
  deliberately not one rule.
- Rule: `configs/prometheus/rules.r1/infra.yml` (`stellarindex.infra` group),
  mirrored in `deploy/monitoring/rules/infra.yml`.
- Guard: `configs/ansible/roles/archival-node/tasks/05-postgres.yml` refuses an
  apply whose `max_wal_size` does not fit the volume `pg_wal` resolves to.

## Changelog

| Date | Change |
| ---- | ------ |
| 2026-09-16 | Created, with the alerts, after `pg_wal` filled root and took the database down. |
