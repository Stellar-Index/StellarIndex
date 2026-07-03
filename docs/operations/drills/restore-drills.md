# Restore-drill evidence log

One entry per drill (ADR-0043 §3; CS-110: "a backup that has never
been restored is a hope, not a backup"). Appended by
`scripts/ops/restore-drill.sh` when run from a checkout; runs from
`/usr/local/bin` on r1 are reconstructed here from
`/var/log/restore-drill-*.log`.

## 2026-07-03 restore drill (repo1) — first drill series

The first-ever drill took **five runs**, each failing one layer
deeper — every failure mode is now encoded in the script, which is
exactly the value CS-110 promised:

1. **Production-sized config**: the restored `postgresql.auto.conf`
   carries live sizing (tens-of-GB `shared_buffers`); a second
   instance beside the live DB can't allocate it. → scratch overrides
   (memory downsized).
2. **Debian config layout**: the cluster's `postgresql.conf` /
   `pg_hba.conf` live under `/etc/postgresql`, NOT in PGDATA — the
   restored datadir has neither and `pg_ctl` dies pre-recovery. →
   synthesized minimal config + loopback-trust hba.
3. **WAL replay needs real time**: `pg_ctl -w -t 600` timed out while
   recovery was healthily replaying ~21h of WAL through
   `archive-get` (daily-diff schedule × busy ingest DB). →
   `PG_START_TIMEOUT` default 2h.
4. **Replay-enforced GUCs**: with `hot_standby=on`, recovery ABORTS
   ("insufficient parameter settings") unless
   `max_connections`/`max_worker_processes`/`max_wal_senders`/
   `max_prepared_transactions`/`max_locks_per_transaction` are ≥ the
   primary's — the downsizing pass had cut `max_connections` 200→20.
   → the five GUCs are now read from the live primary and mirrored.

Runs 1–4: `pg_restore` OK every time (848–888s for the ~273GB set,
repo1); `pg_start` failed per the modes above. Run 5 (fifth, with all
four fixes): verdict appended below when complete.

