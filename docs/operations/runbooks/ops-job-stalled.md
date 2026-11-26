---
title: Runbook — ops-job-stalled
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_ops_job_heartbeat_stale` / `stellarindex_ops_job_no_progress` / `stellarindex_ops_job_run_failed` / `stellarindex_entry_index_lagging` / `stellarindex_lending_state_lagging` / `stellarindex_lending_interest_lagging` / `stellarindex_lending_interest_redo_pending` / `stellarindex_lending_interest_proof_failed` / `stellarindex_lending_interest_proof_stale` / `stellarindex_lending_interest_unexplained_versions`

## At a glance

| Alert | Severity | Meaning |
| ----- | -------- | ------- |
| `stellarindex_ops_job_heartbeat_stale` | P3 ticket | the process is GONE |
| `stellarindex_ops_job_no_progress` | P3 ticket | the process is ALIVE and HUNG |
| `stellarindex_ops_job_run_failed` | P3 ticket | the last run FINISHED with a non-zero exit |
| `stellarindex_unit_running_replaced_binary` | P3 ticket | a unit still runs a release binary replaced on disk ([Replaced binary](#replaced-binary)) |

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
| `stellarindex_ops_job_skipped_total{ops_job}` | records dropped as undecodable this run, by `reason` (see [Records skipped](#records-skipped)) |

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
3. If the driver was `scripts/ops/ch-full-backfill.sh`, its resume state file appends a window, with its end, only on exit 0, so an interrupted window is retried. A recorded window is skipped only while the lake still holds every ledger in it.
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

`scripts/ops/recompress-others.sh` issues `OPTIMIZE TABLE ... PARTITION ... FINAL` from a client. The merge runs in the server, so stopping the scope kills only the client and the partition rewrite carries on. After the scope stop, cancel the merge itself (substitute the table):

```sh
clickhouse-client --port 9300 -q "SELECT partition_id, elapsed, progress FROM system.merges WHERE database='stellar' AND table='ledger_entry_changes'"
clickhouse-client --port 9300 -q "SYSTEM STOP MERGES stellar.ledger_entry_changes"   # cancels running merges on the table
# re-run the system.merges SELECT until it returns no rows, then:
clickhouse-client --port 9300 -q "SYSTEM START MERGES stellar.ledger_entry_changes"
```

`KILL QUERY` is not a substitute. Never leave merges stopped: part count climbs until inserts fail with `TOO_MANY_PARTS`. A cancelled merge keeps the originals, so the script redoes that partition from scratch.

### A ClickHouse rewrite outlives its client

`scripts/ops/recompress-lec.sh` runs `ALTER TABLE stellar.ledger_entry_changes REWRITE PARTS IN PARTITION <n>` under the heavy-job scope `recompress-lec-p<n>`. It queries through `RECOMPRESS_CH` (default root's `/usr/bin/clickhouse-client --port 9300`, the admin identity; the bare `clickhouse-client` inside the heavy-job scope is a low-priority user that cannot read `system.mutations`) and aborts at startup if it cannot. The rewrite is a mutation: stopping the script or the scope does not stop it. `SYSTEM STOP MERGES` also stalls it, so never use that here. See it, then stop it:

```sh
clickhouse-client --port 9300 -q "SELECT mutation_id, parts_to_do, latest_fail_reason FROM system.mutations WHERE database='stellar' AND table='ledger_entry_changes' AND NOT is_done"
clickhouse-client --port 9300 -q "KILL MUTATION WHERE database='stellar' AND table='ledger_entry_changes' AND mutation_id='<id>'"
```

Before each rewrite the script freezes the partition to `shadow/lec_codec_<n>/` and releases the freeze only after count, per-part rows and an all-column hash match the `p<n> START` line. A `p<n> MISMATCH` line, or any `ABORT` after `START`, keeps the freeze. A killed rewrite leaves the rows unchanged: check count and hash (step 3), release the freeze (step 4), then re-run the script to redo the partition. To restore partition `<n>` from the freeze, as root (`<path>` = `data_paths` from `system.tables` for the table):

1. `ALTER TABLE stellar.ledger_entry_changes DETACH PARTITION <n>`, then move `<path>detached/<n>_*` to a holding directory.
2. `cp -al /var/lib/clickhouse/shadow/lec_codec_<n>/store/*/*/<n>_* <path>detached/` and `chown -R clickhouse:clickhouse <path>detached`.
3. `ALTER TABLE stellar.ledger_entry_changes ATTACH PARTITION <n>`; check `count()` and `sum(cityHash64(*))` `WHERE _partition_id='<n>'` against the `p<n> START` line.
4. Only when they match: `ALTER TABLE stellar.ledger_entry_changes UNFREEZE PARTITION <n> WITH NAME 'lec_codec_<n>'`.

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
4. The locks `/run/lock/stellarindex/stellarindex-heavy-<name>.lock` and `/run/lock/stellarindex-heavy.lock` are released with the cgroup; a relaunch is not blocked.
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

## Replaced binary

`stellarindex_unit_running_replaced_binary` (ticket, `for: 30m`) fires when a process in `{unit}` runs a `/usr/local/bin/stellarindex-*` or `galexie` binary that was replaced on disk after it started. Source: `memory-mappings.sh` (5-minute timer, `memory_mappings.prom`); `1` = stale, `0` = current, no series = no release binary in that unit. The process is executing the OLD build, so a deployed fix is not in effect for whatever it writes. Usual causes: a deploy that skipped the restart (`cli_daemon_units` in `configs/ansible/playbooks/deploy-binary.yml` does not list the unit), or a binary copied by hand. A oneshot caught mid-run on a replaced binary is reported too; it clears on its next start.

Check:

```sh
systemctl show -p MainPID,ActiveEnterTimestamp <unit>   # when the stale process started
cat /sys/fs/cgroup/system.slice/<unit>/cgroup.procs     # every pid in the unit
readlink /proc/<pid>/exe   # "…/stellarindex-ops (deleted)" or "…stellarindex-ops.prev-<tag>" = stale
```

Fix:

1. `systemctl try-restart <unit>`; re-read the exe links. The alert clears on the next producer run (≤ 5 min).
2. Add a long-running unit to `cli_daemon_units` so the next deploy restarts it.
3. Decide whether to re-derive what the stale writer wrote between `ActiveEnterTimestamp` and the restart: only if a release in that span changed that writer's code (`git log <old-tag>..<new-tag> -- <writer package>`). Re-derive with the domain's own idempotent command, never a bespoke backfill: `cap67-movements` → `stellarindex-ops ch-cap67-movements -write -from <lo> -to <hi>`; a projected domain → `stellarindex-ops projector-replay`; other non-projected domains → `ch-rebuild` ([replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule)).

Do NOT `kill` the process to clear the alert (a unit with `Restart=` comes back, one without stays down; use `try-restart`), delete `memory_mappings.prom`, or delete the `.prev-<tag>` file (it is the rollback target, and deleting it leaves the process on a ` (deleted)` exe, still stale).

## Clearing the alert

The textfile persists after the process dies, deliberately. The next run overwrites it. To clear without a re-run (abandoning the window), delete `/var/lib/node_exporter/textfile_collector/ops_job_<job>.prom` and record why, since the range is then knowingly un-backfilled.

## Do NOT

- Treat `no_progress` as "just slow" without checking the cursor: a slow window still advances `progress_cursor` between scrapes.
- Simplify the `on (ops_job, instance, pid)` join: dropping `ops_job` merges jobs, `instance` lets a namesake on another host mask a dead job, `pid` lets a sibling rescue a finished primary (`deploy/monitoring/rule-tests/ops-job_test.yml`).
- Delete the textfile to silence `heartbeat_stale` while the window should still be backfilled.

## Records skipped

`stellarindex_ops_job_records_skipped` (P3 ticket) fires when `stellarindex_ops_job_skipped_total{reason!~"not_token_shaped|judged_non_token"}` has risen over the last 3 hours (one skip fires it; it keeps firing 1 h). `ch-cap67-movements` and `ch-backfill` report it; the `ch-cap67-movements` `-follow` daemon drops a transfer/mint/burn/clawback event it cannot decode and advances the watermark, so `stellar.account_movements` lacks that event's rows. The raw event stays in `stellar.contract_events`.

Reasons: `not_token_shaped` (a foreign contract's event whose body is neither a bare i128 nor a map with `amount`, e.g. an NFT's u32 id) is expected traffic, is metered and never alerts; so is `judged_non_token`, a contract recorded in `judgedNonToken` (`internal/storage/clickhouse/supply_judged.go`) for that one reason. `amount_not_i128` and `body_unparseable` are token-shaped bodies whose amount did not decode; `decode_failed` is a token-shaped body the decoder still refused (topics, close time). The one-shot run's `-max-decode-errors` budget counts only these three.

1. `journalctl -u <unit> | grep 'events skipped (decode)'` names each window `[lo,hi]`, its counts by reason, and the first 8 alerting `contract:reason` pairs.
2. Read those contracts' events in the window from `stellar.contract_events` and find why the decoder refused them. A real token whose body shape changed is a decoder bug. A contract that is not a token: record the ledger, tx and body as evidence and open the PR adding it to `judgedNonToken` with that reason.
3. After a decoder fix, re-derive the windows: `stellarindex-ops ch-cap67-movements -write -from <lo> -to <hi>` (idempotent; a `<lo>` above watermark+1, or above the floor before any watermark exists, is refused). The one-shot run exits non-zero if anything is still skipped.

## Entry index lagging

`stellarindex_entry_index_lagging` fires when the ledger `ch-entry-index` has built through (its heartbeat's `progress_cursor`) trails the ledgerstream cursor by more than 20,000 ledgers for 30 minutes, or the job never reported. The index backs per-account and per-asset ledger walks; a stall hides recent activity there while the lake stays complete.

Check and fix:

1. Timer alive: `systemctl list-timers entry-index.timer` and `journalctl -u entry-index.service -n 50`. A deploy installs the units but never enables the timer; `systemctl enable --now entry-index.timer` is the last step of the runbook in `deploy/clickhouse/entry_index.sql`, after the tables exist and `/etc/default/entry-index` carries the ec-floor.
2. A refused window names its reason: `is not contiguous` (lake hole; wait for `ch-backfill`), `skips ledgers below it` (manual `-from` above the watermark; re-run without `-from`), or `lacks ledger_entry_changes coverage` (set `-floor-ledger <ec-floor>` through `EXTRA_FLAGS` in `/etc/default/entry-index`).
3. The initial build fires this alert until it catches up; that is progress while `progress_cursor` keeps rising.
4. Re-derive one window by hand after a lake repair (rows collapse on merge; the watermark never moves down):
   `stellarindex-ops ch-entry-index -ch-addr 127.0.0.1:9300 -from <l> -to <l> -write`.

## Lending state lagging

`stellarindex_lending_state_lagging` fires when the ledger `ch-entry-history-lending` has derived through (its heartbeat's `progress_cursor`) trails the ledgerstream cursor by more than 20,000 ledgers for 30 minutes. A host where the job never reported stays quiet. The table backs lending supplied, borrowed, APR and backstop history; a stall ends those series early while the lake stays complete.

Check and fix:

1. Timer alive: `systemctl list-timers lending-state.timer` and `journalctl -u lending-state.service -n 50`. The timer is enabled by hand as the last step of the runbook in `deploy/clickhouse/lending_state.sql`.
2. A refused window names its reason: `is not contiguous` (lake hole; wait for `ch-backfill`), `lacks known pools` (the lake is missing a factory `deploy` event), or `without intra-ledger order` (the lake range's `intra_ledger_seq` must be re-derived first, then the range deleted and replayed per the replay decision rule in `docs/architecture/ingest-pipeline.md`).
3. The initial catch-up fires this alert until it reaches the tip; that is progress while `progress_cursor` keeps rising.

## Lending interest lagging

`stellarindex_lending_interest_lagging` fires when `ch-entry-history-lending-interest`'s `progress_cursor` (the `lending_interest` watermark) trails `ch-entry-history-lending`'s by more than 100,000 ledgers for 2 hours. It is measured against the state pass, so a state stall fires only the alert above. A host where the interest pass was never initialised has no series and stays quiet. A refused interest run also fires `stellarindex_ops_job_run_failed` for that job.

Check and fix (operator commands: the runbook in `deploy/clickhouse/lending_interest.sql`):

1. `journalctl -u lending-state.service -n 50 | grep 'lending interest'`.
2. `backfill incomplete (N ledgers behind; run the backfill chunks)`: the timer's pass catches up at most one 100,000-ledger window. Run the backfill chunks by hand under `run-heavy-job.sh` until a run reports the tip. Each holds the state lock shared, so while one runs the timer's state pass logs `skipped, an interest run is reading lending_state` and its interest pass skips; keep chunks short (`-interest-max-windows`). Between chunks, wait for one `lending-state.service` run to complete without that line (`systemctl list-timers lending-state.timer` for the next trigger, then `journalctl -u lending-state.service -n 20`), so the state pass catches up. If state stops advancing, `stellarindex_lending_state_lagging` fires: pause the chunks until it clears.
3. `skipped, another interest run holds the lock`: a manual chunk or re-derive is running; that is progress while its cursor rises.
4. `a lending_state replay rewrote the state from ledger L` (`stellarindex_lending_interest_redo_pending`, below): run `-interest-only -from L -write` (then from each run's end) until a run reaches the tip; the redo then clears.
5. A refused window names its reason: an unmapped or mismatched WASM hash, an upgrade ledger, an instance whose executable the code history does not hold (`contract_instance_changes` behind the lake), a failed check against `lending_state_hourly`, a seed the lake does not prove (`not the lake's last version before the window`: finish the lending_state replay), `lending_state has not derived pool P` (replay lending_state from P's deploy ledger), or `bring each up with -interest-only -interest-pool <pool> -write` (a pool added after the watermark), or `lending interest residual ledger lacks contract events` (a ledger with a version the pass classifies by its events whose `contract_events` count differs from `stellar.ledgers.soroban_event_count`, or that changed under the read: re-extract that ledger's events, then rerun), or `the fold holds an hour whole: raise -interest-max-versions` (one open hour's versions and cause events reach the bound, 150,000 by default at ~4 KB each: rerun with `-interest-max-versions N` above the logged count, within `lending-state.service`'s `MemoryMax`, as a chunk under `run-heavy-job.sh`), or `seed proof read timed out` (one after-seed read of the named keys and range hit 300 s: rerun when the lake is quieter). The watermark holds until it is fixed. A pool event that does not decode never refuses: the run logs it (`… undecodable, its transaction's residuals stay unexplained`), counts it in `stellarindex_lending_interest_undecodable_events`, and leaves every residual of that transaction unexplained.
6. Expected during the `-interest-init` backfill: the cursor starts at the floor, so this fires until the chunks reach the tip. Silence it for the backfill's duration and confirm `progress_cursor` keeps rising; the expression is left as is so a stuck backfill still fires.

## Lending interest redo pending

`stellarindex_lending_interest_redo_pending` fires when the interest heartbeat's `stellarindex_lending_interest_redo_pending` has been 1 for 6 hours. A `lending_state` replay (`-lending-only -from L -write`) arms a redo before it writes; until a re-derive reaches the tip, the timer's interest pass derives nothing and exits cleanly, and every other interest run but `-interest-pool` refuses. A redo armed below the interest floor starts at the floor.

1. `journalctl -u lending-state.service -n 50 | grep 'lending_state replay rewrote'` names the first ledger L.
2. Run `ch-entry-history -lending-only -interest-only -from L -write` under `run-heavy-job.sh`, then from each run's end, until a run reaches the tip. The next timer run writes 0.

## Lending interest proof failed

`stellarindex_lending_interest_proof_failed` fires when a read-only proof found a stored row of `stellar.lending_interest_hourly` that disagrees with the lake. Per window it re-reads the versions with the writer's own coverage-proved read, folds them again and compares the rows under FINAL: the row set, every column byte-exact, `versions` against an independent count of the lake's versions, and the writer's in-line identities (chain endpoints, telescoped gross, BLNT fee carry). It proves the rows match what the current code computes from the current lake. It does not prove the accrual arithmetic itself: that is the fold's own tests and the WASM audit docs.

Two units run it (`stellarindex-ops ch-entry-history -lending-only -prove -interest`), each with its own verdict series, told apart by the alert's `ops_job` label:

- `lending-interest-prove.service`, daily, `-prove-recent 120960`: the last week to the watermark; `ops_job="ch-entry-history-lending-interest-prove"`.
- `lending-interest-prove-full.service`, weekly (Saturday): the floor to the watermark; `ops_job="ch-entry-history-lending-interest-prove-full"`, also the series of a run by hand without `-prove-recent`.

A series' gauge changes only on a run that reaches a verdict: a mismatch sets it; only a run over its whole range with no mismatch and no INCOMPLETE hour clears it. A run that errors, ends INCOMPLETE, or is narrowed by `-from`, `-to` or `-interest-pool` leaves a previous mismatch standing, so the alert clears only once step 3 below proves the whole range again.

Read the report: `journalctl -u lending-interest-prove -n 200` (or `-u lending-interest-prove-full`).

- `lending_interest <pool> <asset>: N hours proved, M mismatched, K INCOMPLETE` per reserve; the last line is the verdict (`PROVED`, `MISMATCH` or `INCOMPLETE`).
- `mismatch <pool> <asset> <hour> <column>: stored X, recomputed Y`: one column of one row. `row: stored present, recomputed none` is a stale row (a re-derive should have deleted it); `stored none` is a missing row; `versions (the lake's count)` is a version lost or doubled.
- `mismatch hours [...): the lake's versions fail the in-line identities` means the lake or `lending_state_hourly` changed after the write (a lake re-derive or a state replay).
- `INCOMPLETE ... derive_rev N, this binary derives M`: rows of another arithmetic revision; re-derive them. This exits non-zero without the proof gauge, so it fires `stellarindex_ops_job_run_failed` instead.
- `INCOMPLETE redo pending from ledger L`: a lending_state replay armed a redo; the hours from L's hour on are not compared until the re-derive it names reaches the tip ([Lending interest redo pending](#lending-interest-redo-pending)). Hours below L are still proved.
- `INCOMPLETE pools ...: the seed is not proved`: a seed in `lending_state_hourly` is not the lake's last version before the range (a lending_state replay deleted rows and has not rewritten them); finish the replay, then prove again. If the reason is `seed proof read timed out`, it names the keys and ledger range of one read the server stopped at 300 s: nothing is disproved; check the lake's load and prove again (the writer refuses the same window until it reads in time).
- `unmarked ...`: rows past the watermark or a pool's coverage row; neither proved nor failed.

Fix: re-derive the named hours, then prove them again (both from `deploy/clickhouse/lending_interest.sql`; add `-ch-addr 127.0.0.1:9300`):

1. After a lake re-derive of the range: `ALTER TABLE stellar.lending_interest_hourly DELETE WHERE last_ledger >= {lo} AND first_ledger <= {hi}`, and wait for `system.mutations`.
2. `stellarindex-ops ch-entry-history -lending-only -interest-only -from <first ledger of the hour> -to <ledger in the last hour> -write`.
3. `stellarindex-ops ch-entry-history -lending-only -prove -interest -from <l> -to <l>` exits 0; then clear the alert with the firing series' whole range: `-prove-recent 120960` for the daily one, no range flag for `…-prove-full` (or `systemctl start lending-interest-prove-full.service`).
- A seed proof error `no change after the seed of keys K over ledgers [lo, hi] (limit 300s)` is a lake read that timed out, not a verdict: the keys' seeds are old (a reserve untouched since `lo`). Each bucket of keys with seeds within a day of each other is read alone, so one old seed never widens the others; run the proof again off-peak, or look at the key's history in `stellar.ledger_entry_changes` over that range.

Stop serving: once anything reads the table, a mismatch that a re-derive does not clear means the writer and the lake disagree. Gate the read off (or serve those totals as a lower bound) until it proves.

## Lending interest proof stale

`stellarindex_lending_interest_proof_stale` fires when a proof's last finished run is older than its cadence allows, so its gauge no longer describes the table: 48 hours for the daily `ops_job="ch-entry-history-lending-interest-prove"`, 8 days for the weekly `…-prove-full`. `systemctl status lending-interest-prove.timer lending-interest-prove-full.timer`: both are enabled only where `lending_interest_prove_enabled` is set, and removed with their textfiles where it is not. A run still going for hours is `heartbeat_stale` or `no_progress` territory; a refused run (lake hole, WASM gate, an underived pool or a key with no history) fires `stellarindex_ops_job_run_failed` and names its reason in the journal. The proof holds the lending_state lock and the interest lock shared, so the state pass and the timer's interest pass skip while it runs. It waits up to 15 minutes for a writer holding either (a state pass, an interest chunk); past that a fire logs `skipped, a lending_state or interest write run held its lock past 15m0s` and exits 0 without a heartbeat, so repeated skips (a replay, state backfill or interest backfill running for days) end here.

## Lending interest unexplained versions

`stellarindex_lending_interest_unexplained_versions` fires when a proof run over its series' whole range counted, for an hour, stored rows of `stellar.lending_interest_hourly` holding a version that moved outside accrual with no same-transaction pool event stating the move exactly (`unreconciled_versions − explained_versions − attributed_versions`, summed). The rows are what the code computes, so this is not a mismatch: an unexplained version means a cause the fold does not reproduce. The gauge holds until the next such run; a host whose proof never ran has no series.

1. List the rows in `clickhouse-client`:

   ```sql
   SELECT contract_id, asset, hour, first_ledger, last_ledger, brate_mismatches, fee_mismatches,
          backstop_residual, residual_explained, residual_attributed, supply_residual
   FROM stellar.lending_interest_hourly FINAL
   WHERE unreconciled_versions > explained_versions + attributed_versions
   ORDER BY hour
   ```

2. Read the pool's events over each row's ledgers in `stellar.contract_events` and find the transaction that wrote the version. A Blend V2 `defaulted_debt` whose b_rate cut reproduces is explained and lands in `default_loss`; one left unexplained did not reproduce exactly (a supply or withdraw before the cut in the same transaction, or a contract the audit in `docs/operations/wasm-audits/blend.md`, "Interest accrual arithmetic", does not cover). Anything else is a cause the fold has no rule for.
3. Record it in that audit doc. Totals over those hours are lower bounds until a rule explains them; a new rule is a derive revision and a re-derive from the floor.

## stellarindex_catchup_lane_failed

The catch-up lanes (`si-catchup-a`, `si-catchup-b`) run the steps in `/var/lib/stellarindex/catchup/<lane>.queue` in order. A step that exits 75 defers and retries; any other failure writes `<lane>.failed` and the lane sleeps until the file is gone. `catchup-lanes.timer` publishes the lane state every 5 min.

1. `tail -20 /var/lib/stellarindex/catchup/<lane>.log` names the failed step; its own log sits beside it.
2. Fix the cause, then `rm /var/lib/stellarindex/catchup/<lane>.failed`. To skip the step instead, comment its queue line with `#`.

`stellarindex_catchup_lane_stalled` fires when a lane has queued steps, is not failed or stopped, and has logged nothing for 24 h: a step is hung, or the driver is down (`systemctl is-active si-catchup-<lane>`). A long step is normal; check its own progress before stopping it.

## Related

- [ingest-gap-detector-silent](ingest-gap.md#stellarindex_ingest_gap_detector_silent), [zfs-pool-full](infra.md#stellarindex_zfs_pool_low_space), [ch-schema-restore](ch-schema-restore.md).
- [../usd-volume-rederive.md](../usd-volume-rederive.md) "Chunk mode": the job whose cleanup sized the stop bound.
