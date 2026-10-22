---
title: Runbook — TimescaleDB jobs, caggs and compression
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — TimescaleDB job, cagg and compression alerts

## At a glance

Rules: `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, loaded on r1), twin in `deploy/monitoring/rules/storage.yml`. Four alerts read ONE textfile, `/var/lib/node_exporter/textfile_collector/timescale_jobs.prom`, written every 60 s by `timescale-jobs-probe.timer` (installed by `configs/ansible/roles/archival-node/tasks/10-observability.yml`). If the probe stops, or its query fails or returns nothing, the series go absent and those alerts go blind, not quiet: that is [`stellarindex_timescale_probe_degraded`](#stellarindex_timescale_probe_degraded). Silence from the others is not health while the probe is degraded.

## stellarindex_timescale_job_failures_climbing

P3. A single TimescaleDB job has accumulated `stellarindex_timescale_job_failures_total{job_id,proc,hypertable}` failures (less `stellarindex_timescale_job_concurrent_refresh_failures_6h` / `_3d`, weighted by `stellarindex_timescale_job_schedule_interval_seconds`). Usually no customer impact yet, but this counter is the only evidence of background-worker starvation, which once failed 37-69 % of cagg refreshes while `last_run_status` read `Success`. Three arms: `> 10` in 6h (jobs faster than ~36 min), `>= 3` in 3 days (daily and 12h jobs), and `increase(failures[3d]) x schedule_interval >= 129600` (jobs slower than a day). The 3-day window is not a week because local Prometheus retains 7 days and `increase()` at the retention edge loses samples. Concurrent-refresh collisions (`could not refresh continuous aggregate ... due to a concurrent refresh`, e.g. from a `projector-replay`) are subtracted by the probe; a collision that never ends is reported by `cagg_stale`.

Check:

1. Which job:

   ```sql
   SELECT job_id, proc_name, hypertable_name, total_runs, total_failures,
          last_run_status, last_run_started_at
     FROM timescaledb_information.job_stats js
     JOIN timescaledb_information.jobs j USING (job_id)
    ORDER BY total_failures DESC LIMIT 10;
   ```

2. The error. The latest per job is the `reason` label of `stellarindex_timescale_job_last_failure_reason_info{job_id="..."}`; the history:

   ```sql
   SELECT job_id, proc_name, err_message, finish_time
     FROM timescaledb_information.job_errors
    ORDER BY finish_time DESC LIMIT 20;
   ```

   An empty `job_errors` is not "no error": it keeps only what the history-retention job has not dropped, and a worker killed before its error handler (crash, OOM, restart) leaves no row. Read the Postgres server log instead (a file, not the journal):

   ```sh
   # How far back job_errors reaches (`drop_after` in config):
   runuser -u postgres -- psql -d stellarindex -c \
     "SELECT job_id, proc_name, config FROM timescaledb_information.jobs
       WHERE proc_name LIKE 'policy_job_%retention';"
   # The job's worker is named after its application_name:
   runuser -u postgres -- psql -d stellarindex -At -c \
     "SELECT application_name FROM timescaledb_information.jobs WHERE job_id = <job_id>;"
   # Its exits (`exited with exit code 1`, `terminated by signal`) carry a PID.
   # Older runs are in rotated .log.1 / .log.N.gz files (zgrep reads both).
   grep -F '<application_name>' /var/log/postgresql/postgresql-15-main.log | tail -20
   # ... and the ERROR that worker raised is on that PID's lines:
   grep '\[<pid>\]' /var/log/postgresql/postgresql-15-main.log | grep -E 'ERROR|FATAL|PANIC'
   ```

   A run that never started has no worker: look for the scheduler's `out of background workers` warning (Starvation).

   | `err_message` | Meaning |
   | --- | --- |
   | `failed to start job` | Starvation: no background worker slot free |
   | `... due to a concurrent refresh` | Collision, already excluded; look at the job's other errors |
   | anything else | The job body failed |

Likely causes and fix:

- Starvation. `timescaledb.max_background_workers` must exceed the scheduled job count and `max_worker_processes` must cover background plus parallel workers plus margin. If tight, raise them in `configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and apply (a Postgres restart is required; schedule it):

  ```sql
  SHOW timescaledb.max_background_workers;   -- must exceed the job count
  SHOW max_parallel_workers;
  SHOW max_worker_processes;                 -- must exceed the sum of the above
  SELECT count(*) FROM timescaledb_information.jobs;
  ```

- Job body failing: treat as an ordinary job failure. If it is a cagg refresh, `cagg_stale` follows once retries stop covering it and takes priority.
- After a TimescaleDB upgrade: `trades_compression_policy` (migration 0205) calls the internal `_timescaledb_functions.policy_compression(job_id, config)` to set `lock_timeout`; that signature is not public API. Check release notes before upgrading and `CALL run_job(<id>)` once after. If it changed, update the job body in a new migration or re-add the built-in `add_compression_policy`.

Do not act on a short burst after a restart or heavy one-shot job, or on this alert alone with caggs fresh. Escalate: if `cagg_stale` is also firing, treat that as the incident.

## stellarindex_timescale_probe_degraded

P2. Three storage alerts read the probe's textfile, so while it is degraded their silence means nothing. Downstream series going empty looks identical to healthy (`>` and `time() - x` over an empty vector are empty), so the probe publishes its own state each run: `stellarindex_timescale_probe_query_ok{query}`, `stellarindex_timescale_probe_rows{query}`, `stellarindex_timescale_probe_last_run_unix`.

Symptoms:

- `query_ok{query=...} == 0`: that query errored, or its reply did not parse (cause 7).
- `rows{query=...} == 0`: the query succeeded and returned nothing. (`job_errors` publishes no rows gauge: empty is healthy there.)
- `time() - last_run_unix > 600`: the file is frozen; the other metrics look healthy but are stale.
- Series absent entirely (summary has no `query` label): the probe never wrote the file on this host.

Check:

```sh
ssh root@136.243.90.96

# Is the timer alive, and did the last run succeed?
systemctl status timescale-jobs-probe.timer --no-pager
systemctl status timescale-jobs-probe.service --no-pager
journalctl -u timescale-jobs-probe.service --since -30min --no-pager

# Is the file fresh? mtime should be < 2 min old.
ls -l --time-style=full-iso /var/lib/node_exporter/textfile_collector/timescale_jobs.prom

# What does the probe say about itself?
grep stellarindex_timescale_probe_ \
  /var/lib/node_exporter/textfile_collector/timescale_jobs.prom

# Run it by hand: fastest way to see the psql error the probe suppresses.
bash -x /usr/local/sbin/timescale-jobs-probe.sh
```

To test one query directly:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name IN ('policy_compression', 'trades_compression_policy');"
```

Likely causes:

1. The role was never applied: rule files auto-deploy (`configs/prometheus/apply-rules.sh`), the ansible-managed probe does not. Most likely the first time this appears. Run `ansible-playbook ... --tags observability --check --diff` first.
2. Postgres unreachable or peer auth broken: all three `query_ok` read 0 and `stellarindex_timescale_primary_down` fires beside it.
3. A renamed `timescaledb_information` view or column after a major upgrade: one query fails. Fix the SQL in the role; `/usr/local/sbin` is overwritten on apply.
4. A filter stopped matching: `query_ok` 1, `rows` 0. Check the policies exist; a hypertable that lost its compression policy is `compression_policies_applied`, a cagg that lost its refresh policy is `caggs_have_refresh_policy` (both in `scripts/ops/config-assertions.sh`).
5. Timer stopped or unit masked: `last_run_unix` ages.
6. Full disk: `mv` fails and the stamp ages; `stellarindex_timescale_disk_full` fires too.
7. Unparseable reply: psql prints command tags (`SET`, `BEGIN`) even with `-At`, and NULL renders as empty. Rows with a non-numeric field are dropped and the query reports `query_ok 0`. node_exporter rejects the whole file on one bad line, so session settings belong in `PGOPTIONS`, never a multi-statement command string (`scripts/ci/lint-textfile-exposition.sh`).
8. The probe refused to publish: unit exits non-zero with `refusing to publish an unparseable textfile` (it prints the offending line); the previous file stays and the stamp ages.

Fix:

1. Read the probe metrics to pick the arm.
2. For a query failure, run it as `postgres` and read the error.
3. For a stale stamp, `systemctl start timescale-jobs-probe.service` and re-check the mtime.
4. Fix the cause in `configs/ansible/roles/archival-node/tasks/10-observability.yml` and apply the role; never hand-edit `/usr/local/sbin`.
5. Verify every `query_ok` is 1, every `rows` non-zero, `time() - last_run_unix` under 60; the alert clears within 15 min.

## stellarindex_timescale_compression_lag

P3 informational. `stellarindex_timescale_chunks_overdue_compression{hypertable=...}` (one row per compression policy, including zeros) stays above 0 for 24 h. "Overdue" means past that hypertable's own `compress_after` plus one `schedule_interval` of grace. Not customer-visible, but uncompressed chunks use 5-20x more disk, a runway to [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full). A hypertable with no compression policy at all is invisible here by design (`compression_policies_applied` in `scripts/ops/config-assertions.sh`, via `stellarindex_config_assertion_failed`); cagg materialisations are not covered by either. `trades` compresses via the custom job `trades_compression_policy` (migration 0205), which has no `hypertable_name` in the jobs view.

Check:

```sh
# Which chunks are overdue? Same predicate the probe uses.
psql -c "SELECT c.hypertable_name, c.chunk_schema, c.chunk_name,
                c.range_start, c.range_end,
                j.config->>'compress_after' AS compress_after,
                j.schedule_interval
         FROM timescaledb_information.jobs j
         JOIN timescaledb_information.chunks c
              ON c.hypertable_schema = COALESCE(j.hypertable_schema, j.proc_schema)
             AND c.hypertable_name = COALESCE(j.hypertable_name, 'trades')
         WHERE j.proc_name IN ('policy_compression', 'trades_compression_policy')
           AND c.is_compressed = false
           AND c.range_end < now()
               - COALESCE((j.config->>'compress_after')::interval, interval '7 days')
               - j.schedule_interval
         ORDER BY c.range_end
         LIMIT 20;"

# Why is the job failing?
psql -c "SELECT * FROM timescaledb_information.job_stats
         WHERE job_id IN (SELECT job_id FROM timescaledb_information.jobs
                          WHERE proc_name IN ('policy_compression', 'trades_compression_policy'));"

# Manual compression: compress_chunk needs the schema-qualified name
# (chunks live in _timescaledb_internal) or it errors "relation does not exist".
psql -c "SELECT compress_chunk('<chunk_schema>.<chunk_name>');"
```

Likely causes:

- Lock contention with insert traffic. `trades` asks for each chunk lock under a 5 s `lock_timeout` and fails the run ("columnstore policy failure"); a long reader of an old chunk (historical cagg refresh) causes repeated failures until it ends. Widen `compress_after` so only cold chunks are touched.
- A schema change (`ALTER TABLE`) invalidating pending compression: finish it; may need to uncompress and recompress the chunks.
- Disk IO saturated: compression is IO-heavy.
- Job scheduler wedged (as in `cagg_stale`).

Fix:

1. Confirm which chunks are stuck and why (above).
2. Compress one chunk by hand to reproduce the error.
3. Address the cause (lock, schema, IO).
4. Catch up the backlog, carefully:

   ```sh
   psql -c "SELECT compress_chunk(c.chunk_schema || '.' || c.chunk_name)
            FROM timescaledb_information.jobs j
            JOIN timescaledb_information.chunks c
                 ON c.hypertable_schema = COALESCE(j.hypertable_schema, j.proc_schema)
                AND c.hypertable_name = COALESCE(j.hypertable_name, 'trades')
            WHERE j.proc_name IN ('policy_compression', 'trades_compression_policy')
              AND c.is_compressed = false
              AND c.range_end < now()
                  - COALESCE((j.config->>'compress_after')::interval, interval '7 days')
                  - j.schedule_interval
            LIMIT 10;"
   ```

5. Verify the metric drops to zero and disk trends down.

Escalate: if lag grows toward `postgres_disk_full`, page the database owner.

## stellarindex_timescale_cagg_stale

P2. `(time() - stellarindex_cagg_last_refresh_unix) > 5 * stellarindex_cagg_refresh_interval_seconds` for a `cagg` for 5 min (both metrics come from the probe). Impact: `/v1/vwap`, `/v1/twap`, `/v1/ohlc` read stale windows or fall back to slow raw aggregation.

Check. Step 0: the alert reads textfile series, so a dead probe means a blind alert. Fix the probe first.

```sh
ssh root@136.243.90.96
systemctl status timescale-jobs-probe.timer --no-pager
ls -l --time-style=full-iso /var/lib/node_exporter/textfile_collector/timescale_jobs.prom
# mtime should be < 2 min old.
```

Then (as postgres on r1):

```sh
runuser -u postgres -- psql -d stellarindex <<'SQL'
SELECT j.job_id, j.hypertable_name, j.schedule_interval,
       s.last_run_status, s.last_run_duration, s.last_successful_finish
FROM timescaledb_information.jobs j
JOIN timescaledb_information.job_stats s USING (job_id)
WHERE j.proc_name = 'policy_refresh_continuous_aggregate'
ORDER BY s.last_successful_finish DESC NULLS FIRST;
-- error text for failed runs:
SELECT job_id, finish_time, sqlerrcode, err_message
FROM timescaledb_information.job_errors
ORDER BY finish_time DESC LIMIT 10;
SQL
# No job_errors row does NOT mean no error: use the server log recipe in job_failures_climbing.

# Is the timescaledb scheduler even running?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT * FROM pg_stat_activity WHERE application_name LIKE '%timescale%';"

# Manual refresh of the trailing window. Never a NULL start: on twap_1h/twap_1d that
# deletes history once prices_1m's retention is armed
# (data-freshness.md#stellarindex_twap_history_missing). <min_window> is the view's
# MinWindow in internal/storage/timescale/diagnostics.go (TradesCAGGs, OracleCAGGs,
# SupplyCAGG): '3 hours' for *_1h, '3 days' for *_1d, '21 days' for *_1w,
# '93 days' for *_1mo; narrower fails 22023 "refresh window too small".
runuser -u postgres -- psql -d stellarindex -c \
  "CALL refresh_continuous_aggregate('<cagg_name>', now() - interval '<min_window>', now());"
```

Likely causes:

- The refresh job errors (constraint violation, lock conflict with vacuum/migration, OOM on a large window): read `err_message` or the server log.
- timescaledb scheduler hung (rare): restart Postgres.
- Refresh takes longer than its schedule interval, so runs are skipped: widen the interval or narrow the window.
- Refresh window includes data still being written (`end_offset = 0`): add an `end_offset` >= the largest expected late-arrival gap.

Fix:

1. Read the error (above).
2. Refresh manually to see if it was a one-off.
3. Fix the underlying error (schema, constraint, lock).
4. Re-enable the policy if it was disabled.
5. Verify `job_stats` shows `Success` with `last_successful_finish` inside the schedule interval.


## stellarindex_timescale_cagg_refresh_missing

P2. A cagg series existed within the last day and is absent now: its refresh policy was dropped or renamed, which `cagg_stale` cannot see over an absent series. It covers ONE cagg's row vanishing while the probe and the others report fine. Check step 0 and the `policy_refresh_continuous_aggregate` listing in `cagg_stale` (does the cagg still have a policy?); also `caggs_have_refresh_policy` in `scripts/ops/config-assertions.sh`.

## stellarindex_late_trade_cagg_refresh_failing

Ticket. `stellarindex_late_trade_cagg_refresh_overdue_seconds > 600` for 30m, emitted by the indexer. Cagg policies re-aggregate only their last `start_offset`, so late trades are materialised by the indexer's refresher (`internal/pipeline/late_trade_refresh.go`; non-forced, `twap_*` wait on `prices_1m`). Firing means buckets under-reported for 40+ min.

Check: `journalctl -u stellarindex-indexer --since -1h | grep 'late-trade cagg refresh failed'` names the view and windows. Usual causes are those of `cagg_stale` (lock conflict `55P03` with the policy job; statement timeout on a wide window). Otherwise refresh by hand over the logged window, non-forced, padded to the view's MinWindow, `prices_1m` before `twap_1h` / `twap_1d`:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "CALL refresh_continuous_aggregate('<view>', '<from>', '<to>');"
```

Pending windows persist in `cagg_late_refresh_windows`.

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT view, from_ts, to_ts, first_seen FROM cagg_late_refresh_windows ORDER BY view;"
```

A row persisting across several intervals is the failing case: refresh by hand over `[from_ts, to_ts]`; never delete a row unrefreshed.

### Catching up a late-trade backlog

When the windows are unknown (a SIGKILL, or late trades written before the refresher existed), size the backlog from Timescale's invalidation log, then refresh non-forced:

```sh
# 1. Size it (read-only). One line per trades cagg; open-ended=2 is normal.
#    A `below-floor` line names ranges a non-forced run refuses.
stellarindex-ops trades-cagg-refresh -config /etc/stellarindex.toml -size

# 2. Catch up under the heavy-job wrapper (day-sized CALLs commit one by one).
/usr/local/sbin/run-heavy-job.sh trades-cagg-catchup \
  stellarindex-ops trades-cagg-refresh -config /etc/stellarindex.toml \
    -force=false -from <from> -to <to> -write

# 3. Verify: every view should show ranges=0 outside the policies' own trailing windows.
stellarindex-ops trades-cagg-refresh -config /etc/stellarindex.toml -size
```

The floor is `prices_1m`'s earliest materialised bucket. Below it, or on the first day where `trades` and `prices_1m` disagree, `-force=false` refuses and names the window; refresh those ranges with the forced form (rebuilds `prices_1m` from `trades` first) with the retention policy disarmed. A piece failing with `55P03` raced the policy job: re-run step 2.

## stellarindex_pg_compressed_chunks_unreadable

`pg-chunk-scan.timer` (04:20 UTC) decodes every `compressed_data` column of every compressed chunk with `compressed_data_info()`. A chunk that fails is listed in `stellarindex_pg_compressed_chunk_unreadable{hypertable,chunk}`. `count(*)` and `chunks_detailed_size` stay normal on such a chunk; a read that touches it errors or crashes the backend, and a crash loop restarts Postgres.

The known cause is `merge_chunks()` on a compressed chunk (r1, TimescaleDB 2.26.4). `scripts/ci/lint-chunk-functions.sh` keeps it and `drop_chunks()` out of the tree.

1. Do not merge, drop, decompress or recompress the chunk. Each rewrites it and can lose the evidence or the rows.
2. Find which rows it held: `SELECT range_start, range_end FROM timescaledb_information.chunks WHERE chunk_name = '<chunk>';`.
3. Restore those rows. A derived table (a cagg, a projection) re-derives from its source for that range. A primary table restores from a pgBackRest PITR copy on another host; r1 has no room for a full restore (plan §5.1). Copy the range back with `COPY`, then drop the broken chunk only after the copy verifies, with Ash's yes.
4. Re-run the scan: `systemctl start pg-chunk-scan.service`. It keeps a list of verified chunks in `/var/lib/pg-chunk-scan/verified` and re-scans only new or rewritten ones; delete that file to force a full scan.

`stellarindex_pg_chunk_scan_stale` means no scan has completed for 36 h, so corruption would go unseen. Read `journalctl -u pg-chunk-scan -n 50`; the probe prints the psql ERROR line.

## stellarindex_pg_compressed_chunk_index_bloat

The same nightly scan lists fully compressed chunks (status without the partial bit) whose indexes exceed 1 GiB, in `stellarindex_pg_compressed_chunk_index_bytes{hypertable,chunk}`. Their rows live in the compressed chunk, so a healthy heap is near-empty and its indexes are tens of MB. On r1 (2026-10-21) 13 `trades` chunks held 3-26 GB of indexes each over 0-byte heaps, 161 GB in all; `n_tup_del` matched `n_tup_ins`. Reproduced on 2.26.4: a by-hand `compress_chunk()` on a partial chunk (status 9) recompresses segment-wise and leaves the heap's indexes at full size. The compression policy (`CALL run_job(<job>)`), `compress_chunk()` after a full `decompress_chunk()`, and DELETE or upsert through a compressed chunk all leave them small. Recompress a partial chunk with the policy job, never `compress_chunk()`.

1. Rebuild each listed chunk's indexes. It takes a short exclusive lock, so cap the wait:
   `SET lock_timeout = '5s'; REINDEX TABLE _timescaledb_internal.<chunk>;` (r1: ~150 ms each). Re-run on `55P03`.
2. The space returns to ZFS once the snapshots holding the old blocks expire (2 days for `data/postgres`).
3. Re-run the scan (`systemctl start pg-chunk-scan.service`) to clear the alert.

## Related

- [config-assertion-failed](config-assertion-failed.md); `postgres.md#stellarindex_timescale_disk_full`; `postgres.md#stellarindex_timescale_connections_saturated`; `api.md#stellarindex_api_latency_p95_high`; `api.md#stellarindex_api_price_stale`.
