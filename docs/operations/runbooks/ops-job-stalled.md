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

1. Kill the hung process if it has one: `kill -QUIT` first for the
   goroutine dump, then stop it — **by scope** when it runs under the
   heavy-job wrapper, see [Stopping a wrapped job](#stopping-a-wrapped-job).
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

## Stopping a wrapped job

Every heavy one-shot on r1 runs under `/usr/local/sbin/run-heavy-job.sh`,
which for a root caller puts the payload in a transient scope named
`heavy-<job name>-<wrapper pid>.scope`. Stop it by scope:

```sh
systemctl list-units 'heavy-*.scope' --no-pager   # find the unit
systemctl stop heavy-<name>-<pid>.scope           # blocks until the scope is gone
```

Not by pattern: the wrapper's own command line carries the same tokens
as the payload's, so a `pkill -f` on the subcommand can take the wrapper
and leave the binary — and its Postgres backend — running. The
2026-09-03 restamp was stopped by scope for that reason.

`stop` SIGTERMs every process in the scope at once, then SIGKILLs when
the scope's `TimeoutStopSec` expires. The wrapper sets that from
`HEAVY_JOB_STOP_TIMEOUT`, **default `5min`**, in place of systemd's 90 s
default. The default is short on purpose: almost every payload this
wrapper starts installs no signal handler at all and is gone the instant
SIGTERM lands, and the few that handle it run their cleanup on an
already-cancelled context and finish in seconds. One payload is not like
that — `usd-volume-restamp -chunks` (#372) re-compresses the chunk it
has open, on a context that survives cancellation, and the largest
`trades` chunk in that window is 160 GB uncompressed — so **that job
names its own bound on its launch line**
(`HEAVY_JOB_STOP_TIMEOUT=2h`, in
[../usd-volume-rederive-2026-08.md](../usd-volume-rederive-2026-08.md)
Step 6) rather than every other job inheriting a multi-hour stop it will
never use. The 2 h there is a budget, not a measurement: 160 GB at
~22 MB/s, and no `compress_chunk` rate has been measured on r1. SIGTERM
is still immediate — a job that exits on it promptly is not slowed, and
the bound only decides how long a job still cleaning up may take.

The value is validated before the scope exists. **A bare integer is
SECONDS** under `systemd.time`, so `HEAVY_JOB_STOP_TIMEOUT=2` meaning
two hours would be a two-second grace — systemd accepts it, and the job
is then killed harder than with no setting at all. The wrapper takes a
bare integer, `Ns`, `Nmin`, `Nh` and `infinity`, and refuses every other
spelling and anything below a **90 s floor** (the systemd default the
bound replaces), exiting 2 before the payload starts. It also prints the
bound it will use to stderr on every run, so the job log says what a
stop of that job will do.

### What the bound disarms while it is in force

None of this is visible from outside the wrapper, so it is stated here:

- **The disk watchdog's own low-space stop now waits up to the bound
  before SIGKILL.** `stop_job` is the same `systemctl stop` an operator
  runs; a root-FS or data-pool trip therefore reaches SIGKILL 2 h later
  on a restamp run, not 90 s later.
- **The `|| systemctl kill` fallback in `stop_job` cannot fire inside
  that wait.** `systemctl stop` blocks synchronously until the scope is
  gone and then returns success, so the fallback runs only if the `stop`
  itself fails — never as a shorter escalation path. There is no second
  timer behind the bound.
- **This is affordable only because of what the long window is spent
  on.** The restamp's cleanup is a `compress_chunk`: it writes the
  compressed copy — ~1/15 of the chunk at this window's ratio (379 GB
  uncompressed / 25 GB compressed across the 90 chunks) — beside the
  heap and then truncates the heap, so it peaks well inside the 300 GiB
  the pool guard preserves and **ends by freeing space**. A payload
  whose SIGTERM cleanup instead consumed space or pool connections for
  hours must not be given a long bound; the wrapper's own default stays
  at `5min` for exactly that reason.

### Before applying it: prove systemd takes the property

`TimeoutStopSec` on a transient scope is a systemd-version question, and
a scope that fails to start takes the job with it. Run this on the host
first — it costs nothing and needs no job:

```sh
systemd-run --scope -p TimeoutStopSec=2h --quiet /bin/true; echo "rc=$?"
systemd-run --scope -p MemoryMax=20G -p MemorySwapMax=0 \
  -p CPUWeight=50 -p IOWeight=50 -p TimeoutStopSec=2h --quiet /bin/true; echo "rc=$?"
grep -c TimeoutStopSec /usr/local/sbin/run-heavy-job.sh
```

Both `rc=0` means the running systemd accepts the property, alone and in
the wrapper's full property set (verified on r1, systemd 255,
2026-09-04). The `grep` is the applied/unapplied answer: `0` — r1's
reading on 2026-09-04 — means the host is still on systemd's 90 s
default and every warning below about the long bound does not yet
describe it; `1` means the wrapper carries it.

**If a stop outlives the bound** — the scope is still listed after it,
or `systemctl status heavy-<name>-<pid>.scope` reports `result: timeout`
— systemd has already sent SIGKILL and the SIGTERM cleanup did not
finish. Check, in order:

1. **A surviving backend.** A Postgres backend outlives its killed
   client until it notices the closed socket:
   `sudo -u postgres psql -d stellarindex -c "SELECT pid, state, now() - xact_start AS age, left(query, 80) FROM pg_stat_activity WHERE state <> 'idle' ORDER BY xact_start;"`.
   `pg_terminate_backend(pid)` it. A terminated `compress_chunk` rolls
   back and leaves its chunk decompressed — the next check.
2. **Chunks left decompressed:**
   `sudo -u postgres psql -d stellarindex -c "SELECT chunk_schema, chunk_name, range_start, range_end FROM timescaledb_information.chunks WHERE hypertable_name = 'trades' AND NOT is_compressed AND range_end < now() - interval '7 days';"`
3. **A policy left paused** — a job that paused compression for its run
   is killed before it re-enables it:
   `sudo -u postgres psql -d stellarindex -c "SELECT job_id, scheduled FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades';"`
   `scheduled` must be `true`. `usd-volume-restamp -chunks` prints the
   exact repair (the `compress_chunk` and the `alter_job(…, scheduled =>
   true)`) to stderr **before** each statement that can outlive it — run
   what the job log shows.
4. **The singleton lock** `/run/lock/stellarindex-heavy-<name>.lock` is
   released with the cgroup, so a relaunch is not blocked by the dead
   run.
5. **Whether the bound was wrong for that job.** A job launched without
   `HEAVY_JOB_STOP_TIMEOUT` took the `5min` default; one whose measured
   cleanup exceeds what it was launched with needs a larger value on the
   next launch line — and the measurement written down beside it, not
   left as an estimate.

The bound reaches r1 only when the wrapper is re-rendered — it is
codified in
`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`
under the `heavy-job-wrapper` tag (from `configs/ansible/`), and the
pre-apply smoke check above is what proves the host's systemd will take
it:

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags heavy-job-wrapper --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags heavy-job-wrapper
```

Re-run `grep -c TimeoutStopSec /usr/local/sbin/run-heavy-job.sh`
afterwards — `1`, or the host is still on the 90 s default. A scope
keeps the properties it was created with, so a job already running at
apply time stays on 90 s until it is relaunched.

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
- [../usd-volume-rederive-2026-08.md](../usd-volume-rederive-2026-08.md)
  Step 6, chunk mode — the job whose SIGTERM cleanup sized the stop
  bound, and the state to repair when a run is killed through it.

## Follow-ups (measured, not fixed here)

Two things an enumeration of the wrapper's payloads turned up on
2026-09-04 while sizing the stop bound. Neither is caused by the bound;
both bear on what a stop actually achieves.

- **`internal/ops/archive/verify_archive.go:400-402` documents a signal
  path that does not exist.** The comment on the `maxRuntime == 0`
  branch says the binary "still honours external SIGTERM via the SDK's
  signal hooks". That file imports no `os/signal`, and no package it
  reaches registers a handler for the archive-verify path — the
  uncancellable-parent branch builds its context from
  `context.Background()` with nothing wired to a signal. A SIGTERM to
  that job is therefore the default disposition (immediate death), not
  a cancellation the walk observes. The behaviour is safe for a
  read-only walk; the comment is what is wrong, and it describes exactly
  the semantics the stop bound is documented against, so it will
  mislead the next reader who checks how a stop lands.
- **A ClickHouse `OPTIMIZE … FINAL` cannot be stopped by killing its
  client.** `scripts/ops/recompress-lec.sh:23` issues `OPTIMIZE TABLE
  stellar.ledger_entry_changes PARTITION ID '<p>' FINAL` over the HTTP
  interface (`curl` on `:8123`), and
  [phase-a-capacity-relief-2026-07-18.md](phase-a-capacity-relief-2026-07-18.md)
  Step 3 issues the same statement through `clickhouse-client
  --receive_timeout 36000`. The merge runs server-side; dropping the
  client connection does not cancel it (only `KILL MUTATION` /
  `system.merges` intervention does). That runbook launches the script
  under `run-heavy-job.sh` and tells an operator to abort with
  `systemctl stop heavy-recompress-lec.scope`, so the stop returns while
  the partition rewrite keeps running — and its transient is up to one
  partition, ≤ 424 GiB, above the 300 GiB the wrapper's pool guard
  preserves. A pre-existing hole in the pool guard, independent of the
  stop bound: no value of `TimeoutStopSec` closes it, because the
  process being timed is not the one holding the space.

## Changelog

- 2026-09-04 — added "Stopping a wrapped job": stop by scope not by
  pattern, the wrapper's `TimeoutStopSec` bound (default `5min`,
  override `HEAVY_JOB_STOP_TIMEOUT`, validated with a 90 s floor) and
  what it disarms in the disk watchdog, the pre-apply `systemd-run
  --scope` smoke check, what to check when a stop outlives the bound,
  and the `heavy-job-wrapper` apply that puts it on r1. Recorded two
  measured follow-ups under "Follow-ups".
