---
title: Runbook — ops-job-stalled
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_ops_job_heartbeat_stale` / `stellarindex_ops_job_no_progress`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_ops_job_heartbeat_stale` (P3 / ticket) — the process is GONE.<br>`stellarindex_ops_job_no_progress` (P3 / ticket) — the process is ALIVE and HUNG. |
| Detected by | Prometheus rules in `deploy/monitoring/rules/ingestion.yml` and the R1 overlay `configs/prometheus/rules.r1/ingestion.yml`. |
| Source | node_exporter textfile written by `internal/ops/opsutil.JobHeartbeat`, at `/var/lib/node_exporter/textfile_collector/ops_job_<job>.prom`. |
| Typical MTTR | Minutes to decide; the re-run itself is as long as the window. |
| Impact | A backfill/re-derive window is NOT complete. Nothing is corrupt — the writes are idempotent — but the lake or served tier has a hole until the window is re-run, and the completeness verdict will eventually find it. |

## What these fire on

Before C6-020 (audit-2026-07-23) there was **no** backfill-progress alert
in either rule tree. Long backfills are the dominant ops activity (Phase A
recompress, movement backfills, `ch-full-backfill.sh` windows); they run
for days; and a wedged one looked exactly like a working one unless
somebody was tailing the journal.

`JobHeartbeat` publishes three things *separately* so the two genuinely
different failures stay separable:

| Series | Meaning |
| ------ | ------- |
| `stellarindex_ops_job_running{ops_job}` | 1 while the process is alive; 0 after a clean exit. |
| `stellarindex_ops_job_heartbeat_unix{ops_job}` | Rewritten every 60 s by a background ticker, **independent of whether work is happening**. |
| `stellarindex_ops_job_progress_total{ops_job}` | Units (ledgers) completed this run. |
| `stellarindex_ops_job_progress_cursor{ops_job}` | Highest ledger reached (a running max — the parallel walkers cover disjoint chunks). |
| `stellarindex_ops_job_last_exit_ok{ops_job}` | Only present after a run ends: 1 clean, 0 errored. |

Hence:

- **`heartbeat_stale`** = `running==1` but the ticker stopped → the
  process died without cleanup: OOM-kill, SIGKILL, host reboot, the SSH
  session that owned it closed.
- **`no_progress`** = `running==1`, ticker fine, counter flat for 30 min →
  the process is up and **hung**.

Both exprs are guarded by `running == 1`. That guard is what stops a
*cleanly finished* job — whose heartbeat and counter are frozen forever by
definition — from alerting eternally. Do not remove it.

## Quick diagnosis (≤ 5 min)

1. **Which job?** the `ops_job` label is the subcommand name
   (`ch-backfill`, `backfill`). It is deliberately **not** `job`: that name
   is reserved by Prometheus and would be overwritten with the scrape's own
   `job="node_exporter"` (the real value demoted to `exported_job`),
   which merged every ops job on a host into one alert group. Both alerts
   therefore join `on (ops_job, instance, pid)`.
   A `pid` label appears only on the second of two CONCURRENT runs of the
   same subcommand — the first holds the flock on the primary `.prom`, the
   second writes a `.pid<N>.prom` sibling so neither silences the other.
   `pid` is the third join key for the same reason `ops_job` is the first:
   without it the primary and its sibling cross-match, and a primary that
   finished cleanly gets rescued by the still-running sibling into a
   permanent false ticket. Dead siblings are reaped on the next contention
   (`kill(pid,0)`); one may linger if no further contention ever happens,
   which is harmless — its terminal `running=0` is a true statement about a
   run that really ended.
2. **Is the process actually there?**
   `ps aux | grep stellarindex-ops`, and
   `systemctl status ch-full-backfill` if it was launched by a unit.
3. **`heartbeat_stale` + no process** → it died. Check
   `journalctl -u <unit> --since -6h | tail -100` and
   `dmesg -T | grep -i 'killed process'` for the OOM killer. Note the
   last `progress_cursor` value — that is roughly how far it got.
4. **`no_progress` + process present** → it is hung. Before killing it,
   capture the stack: `kill -QUIT <pid>` writes a goroutine dump to the
   journal. A hang here has recurred (the 2026-07-05 galexie/captive-core
   wedge) and the dump is the only evidence of which read blocked.
   Then check the usual suspects: MinIO/S3 reachable
   (`mc ls <alias>`), ClickHouse accepting queries
   (`clickhouse-client -q 'SELECT 1'`), and the ZFS pool not full
   (`stellarindex_zfs_pool_low_space`).

## Remediation

1. Kill the hung process if it has one (`kill -QUIT` first, then
   `kill`).
2. **Re-run the same window.** Every backfill in this system is
   idempotent — ClickHouse writes land under `ReplacingMergeTree`, served
   writes use `ON CONFLICT` — so re-running an already-covered range is
   free, and leaving the hole is not. `ch-backfill` and `backfill` fail
   closed on a short walk (`backfillCoverage`), so a partial re-run will
   tell you rather than claim success.
3. If the driver was `scripts/ops/ch-full-backfill.sh`, check its resume
   state file: it appends a window only on exit 0, so an interrupted
   window is still pending and will be retried.
4. Confirm with `stellarindex-ops verify-contiguity` / `compute-completeness`
   over the affected range once the re-run finishes.

## Clearing the alert

The textfile persists after the process dies — deliberately, so a job
that died does not become invisible simply because nothing is running.
The next run of the same job overwrites it. To clear without a re-run
(e.g. you have decided to abandon the window), delete
`/var/lib/node_exporter/textfile_collector/ops_job_<job>.prom` — and
record why, because the range is then knowingly un-backfilled.

## Do NOT

- Do not treat `no_progress` as "it's just slow" without checking the
  cursor. A genuinely slow window still advances
  `progress_cursor` between scrapes; a hang does not.
- Do not "simplify" either expr's `on (ops_job, instance, pid)` join.
  Dropping `ops_job` merges every ops job on the host; dropping `instance`
  lets a healthy namesake on another host mask a dead job here; dropping
  `pid` lets a concurrent-run sibling rescue a finished primary. All three
  directions are covered by `deploy/monitoring/rule-tests/ops-job_test.yml`
  (probes a, b1/b2, d).
- Do not delete the textfile to silence `heartbeat_stale` while the
  window is still meant to be backfilled — that converts a visible hole
  into an invisible one, which is the state this alert exists to end.

## Related

- [ingest-gap-detector-silent](ingest-gap-detector-silent.md) — the
  detector that eventually finds the hole a stalled backfill left.
- [zfs-pool-full](zfs-pool-full.md) — a full pool is a common cause of a
  hung write.
- [ch-schema-restore](ch-schema-restore.md) — ADR-0043's lake-protection
  story, which the re-derive path depends on.
