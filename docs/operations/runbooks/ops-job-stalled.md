---
title: Runbook — ops-job-stalled
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_ops_job_heartbeat_stale` / `stellarindex_ops_job_no_progress` / `stellarindex_ops_job_run_failed` / `stellarindex_entry_index_lagging`

## At a glance

| Alert | Severity | Meaning |
| ----- | -------- | ------- |
| `stellarindex_ops_job_heartbeat_stale` | P3 ticket | the process is GONE |
| `stellarindex_ops_job_no_progress` | P3 ticket | the process is ALIVE and HUNG |
| `stellarindex_ops_job_run_failed` | P3 ticket | the last run FINISHED with a non-zero exit |

Rules: `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml`. Source: node_exporter textfile written by `internal/ops/opsutil.JobHeartbeat` at `/var/lib/node_exporter/textfile_collector/ops_job_<job>.prom`. Impact: a backfill/re-derive window is NOT complete. Writes are idempotent so nothing is corrupt, but the lake or served tier has a hole until the window is re-run.

## What these fire on

| Series | Meaning |
| ------ | ------- |
| `stellarindex_ops_job_running{ops_job}` | 1 while the process is alive; 0 after a clean exit |
| `stellarindex_ops_job_heartbeat_unix{ops_job}` | rewritten every 60 s by a ticker, independent of work |
| `stellarindex_ops_job_progress_total{ops_job}` | units (ledgers) completed this run |
| `stellarindex_ops_job_progress_cursor{ops_job}` | highest ledger reached (running max) |
| `stellarindex_ops_job_progress_bytes_total{ops_job}` | bytes of storage observed moving during a phase that completes no units |
| `stellarindex_ops_job_last_exit_ok{ops_job}` | only after a run ends: 1 clean, 0 errored |

- `heartbeat_stale`: `running==1` but the ticker stopped; the process died without cleanup (OOM-kill, SIGKILL, reboot, owning SSH session closed).
- `no_progress`: `running==1`, ticker fine, BOTH progress counters flat for 30 min.
- `run_failed`: `last_exit_ok==0`. No `running==1` guard, so it watches the FINISHED state.

The other two exprs ARE guarded by `running == 1` so a cleanly finished job (frozen counters) does not alert forever; do not remove it. Bytes count too because some phases complete no units (`usd-volume-restamp -chunks` decompressing a chunk). A wedged decompress extends no files, so the alert still fires. The re-compress phase (`compress_chunk`) shows no moving signal and can outlast the 45 min threshold on the largest chunk; a `no_progress` ticket while the job log's last line is `re-compressing` is expected there. Confirm by hand: `SELECT state, wait_event_type, wait_event FROM pg_stat_activity WHERE query LIKE '%compress_chunk%'`; `Lock` is a real wedge, anything else is work in progress.

## Quick diagnosis (≤ 5 min)

1. Which job: the `ops_job` label is the subcommand name (`ch-backfill`, `backfill`). Not `job`, which Prometheus overwrites. Rules join `on (ops_job, instance, pid)`; a `pid` label appears only on the second of two CONCURRENT runs of the same subcommand (it writes a `.pid<N>.prom` sibling). Lingering dead siblings are harmless.
2. Is the process there: `ps aux | grep stellarindex-ops`, and `systemctl status ch-full-backfill` if launched by a unit.
3. `heartbeat_stale` and no process: it died. `journalctl -u <unit> --since -6h | tail -100` and `dmesg -T | grep -i 'killed process'` for the OOM killer. The last `progress_cursor` is roughly how far it got.
4. `no_progress` with a process: hung. Check `progress_bytes_total` and the re-compress note first. Before killing, capture the stack: `kill -QUIT <pid>` writes a goroutine dump to the journal. Then check MinIO/S3 (`mc ls <alias>`), ClickHouse (`clickhouse-client -q 'SELECT 1'`), and `stellarindex_zfs_pool_low_space`.
5. `run_failed`: no process; read the log (`journalctl -u <unit> --since -6h | tail -200`, or terminal output) for the error before the non-zero exit.

## Remediation

1. Kill a hung process: `kill -QUIT` first, then stop it, BY SCOPE under the heavy-job wrapper ([Stopping a wrapped job](#stopping-a-wrapped-job)).
2. Re-run the same window. Backfills are idempotent (`ReplacingMergeTree`, `ON CONFLICT`); `ch-backfill` and `backfill` fail closed on a short walk (`backfillCoverage`).
3. If the driver was `scripts/ops/ch-full-backfill.sh`, its resume state file appends a window only on exit 0, so an interrupted window is retried.
4. Confirm with `stellarindex-ops verify-contiguity` / `compute-completeness` over the range.

## Stopping a wrapped job

Heavy one-shots on r1 run under `/usr/local/sbin/run-heavy-job.sh`, which puts the payload in a transient scope `heavy-<job name>-<wrapper pid>.scope`. Stop it by scope, not by `pkill -f` pattern (the wrapper's command line carries the same tokens, so a pattern can kill the wrapper and leave the binary and its Postgres backend running):

```sh
systemctl list-units 'heavy-*.scope' --no-pager   # find the unit
systemctl stop heavy-<name>-<pid>.scope           # blocks until the scope is gone
```

`stop` SIGTERMs the whole scope, then SIGKILLs at the scope's `TimeoutStopSec`, which the wrapper sets from `HEAVY_JOB_STOP_TIMEOUT` (default `5min`, replacing systemd's 90 s). `usd-volume-restamp -chunks` re-compresses the chunk it has open (the largest `trades` chunk is 160 GB), so it names its own bound on its launch line (`HEAVY_JOB_STOP_TIMEOUT=2h`; see [../usd-volume-rederive.md](../usd-volume-rederive.md) "Chunk mode"). A bare integer is SECONDS under `systemd.time`; the wrapper accepts a bare integer, `Ns`, `Nmin`, `Nh` and `infinity`, refuses other spellings and anything below a 90 s floor (exit 2 before the payload starts), and prints the bound on every run.

While a long bound is in force: the disk watchdog's low-space stop (`stop_job`, the same `systemctl stop`) waits up to the bound before SIGKILL, and the `|| systemctl kill` fallback cannot fire inside that wait. Do not give a long bound to a payload whose SIGTERM cleanup consumes space or pool connections.

### A ClickHouse merge outlives its client

`scripts/ops/recompress-lec.sh` and `scripts/ops/recompress-others.sh` issue `OPTIMIZE TABLE ... PARTITION ... FINAL` from a client. The merge runs in the server, so stopping the scope kills only the client and the partition rewrite (up to one partition, <= 424 GiB on `ledger_entry_changes`, above the 300 GiB the pool guard preserves) carries on. After the scope stop, cancel the merge itself (substitute the table):

```sh
clickhouse-client --port 9300 -q "SELECT partition_id, elapsed, progress FROM system.merges WHERE database='stellar' AND table='ledger_entry_changes'"
clickhouse-client --port 9300 -q "SYSTEM STOP MERGES stellar.ledger_entry_changes"   # cancels running merges on the table
# re-run the system.merges SELECT until it returns no rows, then:
clickhouse-client --port 9300 -q "SYSTEM START MERGES stellar.ledger_entry_changes"
```

`KILL QUERY` is not a substitute. Never leave merges stopped: part count climbs until inserts fail with `TOO_MANY_PARTS`. A cancelled merge keeps the originals, so the script redoes that partition from scratch.

### Before applying the long bound: prove systemd takes the property

```sh
systemd-run --scope -p TimeoutStopSec=2h --quiet /bin/true; echo "rc=$?"
systemd-run --scope -p MemoryMax=20G -p MemorySwapMax=0 \
  -p CPUWeight=50 -p IOWeight=50 -p TimeoutStopSec=2h --quiet /bin/true; echo "rc=$?"
grep -c TimeoutStopSec /usr/local/sbin/run-heavy-job.sh
```

Both `rc=0` means systemd accepts it. The `grep` answers applied (`1`) or not (`0`, still the 90 s default). A running scope keeps the properties it was created with.

If a stop outlives the bound (scope still listed, or `systemctl status heavy-<name>-<pid>.scope` shows `result: timeout`), systemd already sent SIGKILL. Check in order:

1. A surviving Postgres backend, then `pg_terminate_backend(pid)` (a terminated `compress_chunk` rolls back and leaves its chunk decompressed):
   `sudo -u postgres psql -d stellarindex -c "SELECT pid, state, now() - xact_start AS age, left(query, 80) FROM pg_stat_activity WHERE state <> 'idle' ORDER BY xact_start;"`
2. Chunks left decompressed:
   `sudo -u postgres psql -d stellarindex -c "SELECT chunk_schema, chunk_name, range_start, range_end FROM timescaledb_information.chunks WHERE hypertable_name = 'trades' AND NOT is_compressed AND range_end < now() - interval '15 days';"`
3. A compression policy left paused (`scheduled` must be `true`):
   `sudo -u postgres psql -d stellarindex -c "SELECT job_id, scheduled FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy';"`
   `usd-volume-restamp -chunks` prints the exact repair (`compress_chunk` and `alter_job(..., scheduled => true)`) to stderr before each statement that can outlive it; run what the job log shows.
4. The locks `/run/lock/stellarindex-heavy-<name>.lock` and `/run/lock/stellarindex-heavy.lock` are released with the cgroup; a relaunch is not blocked.
5. If the job's cleanup exceeded its bound, raise `HEAVY_JOB_STOP_TIMEOUT` on the next launch line and write the measurement down.

The bound reaches r1 only when the wrapper is re-rendered (`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`, tag `heavy-job-wrapper`, from `configs/ansible/`):

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags heavy-job-wrapper --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags heavy-job-wrapper
```

Re-run `grep -c TimeoutStopSec /usr/local/sbin/run-heavy-job.sh` afterwards; expect `1`.

## Applying pending service restarts (maintenance step)

Unattended upgrades install patches but restart nothing: needrestart is list-only (`/etc/needrestart/conf.d/50-stellarindex-list-only.conf`, `12-hardening.yml`), because restarting every daemon after `apt-daily-upgrade` bounced Postgres and killed heavy jobs. A patched library is not in effect until its daemon restarts, so apply restarts in a maintenance window:

```sh
needrestart -b -r l                             # NEEDRESTART-SVC lines = pending restarts; KSTA 2/3 = reboot pending
systemctl list-units 'heavy-*.scope' --no-pager # must be empty: wait, or stop by scope (above)
systemctl restart <unit> ...                    # the listed units; or `needrestart -r a` for all of them
```

A pending kernel (`NEEDRESTART-KSTA` 2 or 3) needs a reboot in the same window; `Automatic-Reboot` is off.

## Clearing the alert

The textfile persists after the process dies, deliberately. The next run overwrites it. To clear without a re-run (abandoning the window), delete `/var/lib/node_exporter/textfile_collector/ops_job_<job>.prom` and record why, since the range is then knowingly un-backfilled.

## Do NOT

- Treat `no_progress` as "just slow" without checking the cursor: a slow window still advances `progress_cursor` between scrapes.
- Simplify the `on (ops_job, instance, pid)` join: dropping `ops_job` merges jobs, `instance` lets a namesake on another host mask a dead job, `pid` lets a sibling rescue a finished primary (`deploy/monitoring/rule-tests/ops-job_test.yml`).
- Delete the textfile to silence `heartbeat_stale` while the window should still be backfilled.

## Entry index lagging

`stellarindex_entry_index_lagging` fires when the ledger `ch-entry-index` has built through (its heartbeat's `progress_cursor`) trails the ledgerstream cursor by more than 20,000 ledgers for 30 minutes, or the job never reported. The index backs per-account and per-asset ledger walks; a stall hides recent activity there while the lake stays complete.

Check and fix:

1. Timer alive: `systemctl list-timers entry-index.timer` and `journalctl -u entry-index.service -n 50`. A deploy installs the units but never enables the timer; `systemctl enable --now entry-index.timer` is the last step of the runbook in `deploy/clickhouse/entry_index.sql`, after the tables exist and `/etc/default/entry-index` carries the ec-floor.
2. A refused window names its reason: `is not contiguous` (lake hole; wait for `ch-backfill`), `skips ledgers below it` (manual `-from` above the watermark; re-run without `-from`), or `lacks ledger_entry_changes coverage` (set `-floor-ledger <ec-floor>` through `EXTRA_FLAGS` in `/etc/default/entry-index`).
3. The initial build fires this alert until it catches up; that is progress while `progress_cursor` keeps rising.
4. Re-derive one window by hand after a lake repair (rows collapse on merge; the watermark never moves down):
   `stellarindex-ops ch-entry-index -ch-addr 127.0.0.1:9300 -from <l> -to <l> -write`.

## stellarindex_catchup_lane_failed

The catch-up lanes (`si-catchup-a`, `si-catchup-b`) run the steps in `/var/lib/stellarindex/catchup/<lane>.queue` in order. A step that exits 75 defers and retries; any other failure writes `<lane>.failed` and the lane sleeps until the file is gone. `catchup-lanes.timer` publishes the lane state every 5 min.

1. `tail -20 /var/lib/stellarindex/catchup/<lane>.log` names the failed step; its own log sits beside it.
2. Fix the cause, then `rm /var/lib/stellarindex/catchup/<lane>.failed`. To skip the step instead, comment its queue line with `#`.

`stellarindex_catchup_lane_stalled` fires when a lane has queued steps, is not failed or stopped, and has logged nothing for 24 h: a step is hung, or the driver is down (`systemctl is-active si-catchup-<lane>`). A long step is normal; check its own progress before stopping it.

## Related

- [ingest-gap-detector-silent](ingest-gap.md#stellarindex_ingest_gap_detector_silent), [zfs-pool-full](infra.md#stellarindex_zfs_pool_low_space), [ch-schema-restore](ch-schema-restore.md).
- [../usd-volume-rederive.md](../usd-volume-rederive.md) "Chunk mode": the job whose cleanup sized the stop bound.
