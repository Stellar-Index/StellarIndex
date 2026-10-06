---
title: Runbook — TimescaleDB jobs, caggs and compression
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — TimescaleDB job, cagg and compression alerts

Seven storage alerts, rules in `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, the file r1 actually loads) with a multi-host twin in `deploy/monitoring/rules/storage.yml`. The first five are TimescaleDB's own job state: four of them read ONE textfile, `/var/lib/node_exporter/textfile_collector/timescale_jobs.prom`, written by `timescale-jobs-probe.timer` (every 60 s on r1), a shell probe installed by `configs/ansible/roles/archival-node/tasks/10-observability.yml` (task `TimescaleDB job/CAGG health probe (script)`). If that probe stops, or its query fails or returns nothing, the series go **absent** and those alerts go blind rather than quiet; that state is alerted as [`stellarindex_timescale_probe_degraded`](#stellarindex_timescale_probe_degraded). Whenever a series flat-lines, check probe health first: silence from the other four is not evidence of health while the probe is degraded.

## At a glance

| Alert | Severity | Fires when |
| ----- | -------- | ---------- |
| [`stellarindex_timescale_job_failures_climbing`](#stellarindex_timescale_job_failures_climbing) | P3 ticket | one job accumulates >10 failed runs in 6h, >= 3 in 3 days, or failures covering at least half its scheduled runs in 3 days; sustained 30m |
| [`stellarindex_timescale_probe_degraded`](#stellarindex_timescale_probe_degraded) | P2 ticket, `for: 15m` | the probe reports a failing/empty query or a stale stamp, or the series are absent |
| [`stellarindex_timescale_compression_lag`](#stellarindex_timescale_compression_lag) | P3 (`severity: informational`), `for: 24h` | `stellarindex_timescale_chunks_overdue_compression{hypertable="X"} > 0` sustained 24 h |
| [`stellarindex_timescale_cagg_stale`](#stellarindex_timescale_cagg_stale) | P2 ticket, `for: 5m` | a cagg's refresh is > 5x its interval overdue |
| [`stellarindex_timescale_cagg_refresh_missing`](#stellarindex_timescale_cagg_refresh_missing) | P2 ticket, `for: 15m` | a cagg's series existed within the last day and is absent now |
| [`stellarindex_late_trade_cagg_refresh_failing`](#stellarindex_late_trade_cagg_refresh_failing) | ticket | the indexer's late-trade cagg refresh failed >= 3 times in 30 min |
| [`stellarindex_late_trade_cagg_refresh_abandoned`](#stellarindex_late_trade_cagg_refresh_abandoned) | ticket | the indexer stopped with a late-trade window it could not refresh |

## stellarindex_timescale_job_failures_climbing

P3 ticket: no immediate customer impact (usually none *yet*; TimescaleDB retries on the next tick), but the safety margin is gone.

Fires when a single TimescaleDB background job accumulates **>10 failed runs in 6h**, **3 or more in 3 days**, or failures covering **at least half the runs its `schedule_interval` gave it in 3 days** (failures x interval >= 36h), sustained 30m.

Producer: `timescale-jobs-probe.timer` on r1 (60s). If that probe stops, or its `job_stats` query fails or returns nothing, this counter goes absent and the alert is blind rather than quiet; `stellarindex_timescale_probe_degraded` is the standing signal for it. Metric: `stellarindex_timescale_job_failures_total{job_id,proc,hypertable}`, less `stellarindex_timescale_job_concurrent_refresh_failures_6h` / `_3d`, weighted by `stellarindex_timescale_job_schedule_interval_seconds` (same labels, same probe).

**Why this alert exists at all.** r1 once failed **37-69 % of every CAGG refresh run** with `failed to start job` (background-worker starvation) and *nothing surfaced it*. The jobs got a slot on a later tick, so `last_run_status` read `Success`, the caggs were never stale, and both `stellarindex_timescale_cagg_stale` and `stellarindex_timescale_compression_lag` stayed correctly quiet. The only evidence was this counter, which no rule referenced until 2026-08-30 (wave-D ALERT-12). So treat a firing here as **"the thing that hid the last incident is happening again"**, not as a broken job.

**Why three arms.** The counter is per job (`{job_id,proc,hypertable}`), so a job scheduled every *T* can accrue at most `6h / T` failures, and `> 10 in 6h` therefore needs *T* under ~36 minutes. Every compression policy (12h `schedule_interval`) and five of the CAGG refresh policies (`prices_4h` 1h, `prices_1d` 6h, `prices_1w` and `prices_1mo` 1d, `twap_1d` 6h) are slower than that, so for a year this alert was *arithmetically unable* to fire for them, the same jobs the r1 probe found failing 66-81 %. The second arm, **3 or more failures in 3 days**, judges the slow half of the fleet: a job on a daily schedule failing every run trips it on the third day, a 12h compression policy inside two.

The 3-day count still caps out: a job scheduled every *T* runs at most `3d / T` times in the window, so `>= 3` is unreachable once *T* exceeds a day. The third arm is derived from the job's own `schedule_interval` instead of a fixed count: `increase(failures[3d]) x schedule_interval >= 129600` (36h, half the window) means at least half of the runs scheduled in 3 days failed, whatever *T* is: one failure for *T* >= 1.5 days, two of three for a daily job, three of six for a 12h policy (the same bar as the second arm). At a 1m schedule it needs 2160 failures, so fast jobs stay with the 6h arm. The probe emits the interval as `stellarindex_timescale_job_schedule_interval_seconds` with the counter's exact labels; a job whose interval does not parse still gets its counter (the first two arms still judge it) but no gauge, and the probe reports `query_ok{query="job_stats"} 0` so `stellarindex_timescale_probe_degraded` says the third arm is blind.

Measured on r1 (2026-09-18) while adding it: `policy_compression` on `trades` (job 1000, 12h schedule) had failed **6 times in 7 days** with `Failed to convert '1' chunks to columnstore`, three of them inside three hours, and no rule could fire on it. Over the same week the only other failing job was the `prices_1m` refresh, with a single failure, below the new threshold, which is what keeps ordinary retry noise out of the ticket queue.

The 3-day window is not a week on purpose: local Prometheus retains 7 days (`local_prometheus_retention_time`), and an `increase()` window at the retention edge loses its left-hand samples silently.

**Concurrent-refresh collisions are not counted.** A CAGG refresh policy that runs while something else refreshes the same window (an operator `projector-replay`, a backfill's own refresh) is rejected with `could not refresh continuous aggregate … due to a concurrent refresh`. The job body is fine and the next tick retries, but `total_failures` counts it, and on r1 one replay's six collisions held the 3-day arm red for three days. The probe counts those failures per job from `job_errors` over each arm's window (`stellarindex_timescale_job_concurrent_refresh_failures_6h` and `_3d`, emitted as 0 when there are none) and every arm subtracts them. Two limits, both on the loud side: a collision whose `job_errors` row is missing is still counted, and if the gauge is absent the alert falls back to the raw counter. A collision that never ends stops the refreshes, and `stellarindex_timescale_cagg_stale` reports that.

Quick diagnosis (<= 5 min):

1. **Which job, and is it starving or erroring?**

   ```sql
   SELECT job_id, proc_name, hypertable_name, total_runs, total_failures,
          last_run_status, last_run_started_at
     FROM timescaledb_information.job_stats js
     JOIN timescaledb_information.jobs j USING (job_id)
    ORDER BY total_failures DESC LIMIT 10;
   ```

2. **Read the actual error**; this is the branch point. The most recent message per job is already in Prometheus as the `reason` label of `stellarindex_timescale_job_last_failure_reason_info{job_id="…"}` (truncated to 200 characters, braces shown as parentheses); for the full history:

   ```sql
   SELECT job_id, proc_name, err_message, finish_time
     FROM timescaledb_information.job_errors
    ORDER BY finish_time DESC LIMIT 20;
   ```

   **If the failing job has no row here** (and no `reason_info` series), the failures are real but their text is not in the view; an empty `job_errors` is not "no error". `total_failures` is a lifetime counter; `job_errors` keeps only what TimescaleDB's history-retention job has not yet dropped, and a run whose worker died before its error handler (crash, OOM kill, restart mid-run) may leave no row at all. This is r1's observed state: compression jobs at 30-81 % lifetime failures with `job_errors` empty. Take the error from the Postgres server log instead (`logging_collector` is on, so it is the file, not the journal):

   ```sh
   # How far back job_errors reaches (`drop_after` in config):
   runuser -u postgres -- psql -d stellarindex -c \
     "SELECT job_id, proc_name, config FROM timescaledb_information.jobs
       WHERE proc_name LIKE 'policy_job_%retention';"
   # The job's worker is named after its application_name:
   runuser -u postgres -- psql -d stellarindex -At -c \
     "SELECT application_name FROM timescaledb_information.jobs WHERE job_id = <job_id>;"
   # Its exits (`exited with exit code 1`, `terminated by signal`) carry a PID.
   # Older runs are in the rotated .log.1 / .log.N.gz files (zgrep reads both).
   grep -F '<application_name>' /var/log/postgresql/postgresql-15-main.log | tail -20
   # ... and the ERROR that worker raised is on that PID's lines:
   grep '\[<pid>\]' /var/log/postgresql/postgresql-15-main.log | grep -E 'ERROR|FATAL|PANIC'
   ```

   Classify the log's message with the same table. A run that never started has no worker and so no exit line; look for the scheduler's `out of background workers` warning instead; that is Starvation, below.

   | `err_message` | Meaning | Go to |
   | --- | --- | --- |
   | `failed to start job` | **Starvation**: no background worker slot was free | Starvation |
   | `… due to a concurrent refresh` | A collision; already excluded from the alert, so look at the job's *other* errors | Job body |
   | anything else (SQL error, OOM, lock timeout) | The job body genuinely failed | Job body |

**Starvation (the known-incident shape).** Check headroom; the fix last time was in `postgresql.conf.j2`:

```sql
SHOW timescaledb.max_background_workers;   -- must exceed the job count
SHOW max_parallel_workers;
SHOW max_worker_processes;                 -- must exceed the sum of the above
SELECT count(*) FROM timescaledb_information.jobs;
```

`timescaledb.max_background_workers` must be **greater than the number of scheduled jobs**, and `max_worker_processes` must cover background workers plus parallel workers plus a margin. If they are tight, raise them in `configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and apply; **a Postgres restart is required**, so schedule it.

**Job body genuinely failing.** Not starvation: read the error and treat it as an ordinary job failure. If it is a CAGG refresh, `stellarindex_timescale_cagg_stale` will follow once retries stop covering it; that ticket is the customer-impact signal and takes priority over this one.

**After a TimescaleDB upgrade.** `trades_compression_policy` (migration 0205) is a custom job that calls the internal `_timescaledb_functions.policy_compression(job_id, config)` so it can set `lock_timeout`. That signature is not a public API: an upgrade that changes it turns the job into repeated failures. Before upgrading, check the release notes for `policy_compression`, and after, `CALL run_job(<id>)` once by hand. If the signature changed, update the job body in a new migration or drop the custom job and re-add the built-in `add_compression_policy`.

When NOT to act:

- **A short burst after a restart or a heavy one-shot job** is expected: contention clears and the counter stops climbing. The 6h window plus `for: 30m` is sized to ride those out. The 3-day arm needs three failures, so a single retried blip does not reach it either, except on a job scheduled every 1.5 days or slower, where one failed run is already half of its runs in the window and is worth the ticket.
- **This alert alone, with caggs fresh**, is not a customer-facing incident. It is a warning that the safety margin is gone.

## stellarindex_timescale_probe_degraded

P2 ticket (`for: 15m`). Typical MTTR 15-30 min. No direct customer impact, but three storage alerts ([cagg_stale](#stellarindex_timescale_cagg_stale), [compression_lag](#stellarindex_timescale_compression_lag) and [job_failures_climbing](#stellarindex_timescale_job_failures_climbing)) read one textfile from this probe, and while it is degraded their silence means nothing either way. The probe publishes its own state alongside its data: `stellarindex_timescale_probe_query_ok{query}`, `stellarindex_timescale_probe_rows{query}` and `stellarindex_timescale_probe_last_run_unix`.

Why this exists: everything downstream of the textfile fails quiet. `>` and `time() - x` over an empty vector are empty, so an alert whose series went absent does not fire; it goes blind, and blind looks exactly like healthy on every dashboard. Three ways the producer could reach that state without saying a word:

- its psql helper ended in `2>/dev/null || true`, so a failing query returned an empty result **and** exit 0. The file was rewritten without that query's families and node_exporter served it;
- a query that exits 0 and returns **no rows** drops the same series without raising a status at all (a renamed `timescaledb_information` view, or a filter (`proc_name`, a `config` key) that stopped matching). The compression family emits one row per policy *including zeros* precisely so that absence means "probe stopped", which makes zero rows the blind state rather than a quiet one;
- the script not running leaves node_exporter serving the **last file it saw**, indefinitely. The series stay present and get a fresh sample timestamp on every scrape while their values freeze, the same shape [`stellarindex_config_assertions_stale`](config-assertion-failed.md) was written for.

So the probe reports its own state on every run and this alert reads it. Each arm maps to one of those shapes.

Symptoms:

- `stellarindex_timescale_probe_query_ok{query=…} == 0`: that query errored, **or its reply did not have the shape the probe's read loop expects** (root cause 7). The alert names the query in the summary; the journal distinguishes the two.
- `stellarindex_timescale_probe_rows{query=…} == 0`: that query succeeded and returned nothing. `job_errors` publishes no rows gauge: an empty `timescaledb_information.job_errors` is the healthy state.
- `time() - stellarindex_timescale_probe_last_run_unix > 600`: the file has not been rewritten for ten ticks. The per-query metrics beside it will look perfectly healthy; they are frozen.
- The series are absent entirely: the probe has never written the file on this host. The summary carries no `query` label in this case.

Quick diagnosis (<= 5 min):

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

# Run it by hand — this is the fastest way to see the psql error the
# probe suppresses.
bash -x /usr/local/sbin/timescale-jobs-probe.sh
```

If a query is the problem, run it directly and read the error:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name IN ('policy_compression', 'trades_compression_policy');"
```

Typical root causes:

1. **The role was never applied.** The rule files auto-deploy (`configs/prometheus/apply-rules.sh`, run by the deploy workflow); the ansible-managed probe does not. A rule that reads a metric a newer producer emits will alert until the archival-node role is applied: `ansible-playbook … --tags observability --check --diff` first. This is the `codified is not applied` class, and it is the most likely cause the first time this alert appears.
2. **Postgres unreachable or peer auth broken.** All three queries fail together, all three `query_ok` read 0, and the stamp stays fresh. `stellarindex_timescale_primary_down` should be firing beside this.
3. **A renamed or dropped `timescaledb_information` view or column** after a TimescaleDB major upgrade. One query fails or returns nothing; the others are fine. Fix the SQL in the role, not on the host; the file under `/usr/local/sbin` is overwritten on every apply.
4. **A filter that stopped matching.** `query_ok` is 1 and `rows` is 0: the SQL is valid, the data it selects no longer exists. Check that the policies really are there before assuming the probe is wrong; a hypertable that genuinely lost its compression policy is `compression_policies_applied` in `scripts/ops/config-assertions.sh`, and a continuous aggregate that lost its refresh policy is `caggs_have_refresh_policy` there.
5. **The timer stopped or the unit is masked.** `last_run_unix` ages while everything else looks healthy.
6. **A full disk.** `mv` fails, the previous file stays in place, and the stamp ages. `stellarindex_timescale_disk_full` fires alongside.
7. **A reply the read loop could not parse** (added 2026-09-10). psql narrates: it prints a command tag (`SET`, `BEGIN`) for any statement that returns no rows, `-At` does **not** suppress it, and a NULL column renders as an empty field. Any of those lands in a field the loop reads as a number. Rows with a non-numeric field are now dropped and the query reports `query_ok 0`, so this arm covers it; `rows` will be lower than the row count the query really returned. Run the query by hand (above) and compare its raw output with what the probe wrote. The original instance is worth knowing: the convoy query was written as `SET statement_timeout = '10s'; SELECT …`, psql printed `SET` on its own line, and the probe wrote `stellarindex_pg_lock_convoy_backends SET`. node_exporter rejects the **whole file** on one unparseable line, so all four families went, including these health gauges, and nothing alerted, because the alerting that would have noticed was in the file that stopped parsing. Session settings belong in `PGOPTIONS`, and `scripts/ci/lint-textfile-exposition.sh` now fails CI on a multi-statement command string.
8. **The probe refused to publish.** The unit exits **non-zero**, the journal carries `refusing to publish an unparseable textfile`, the previous file stays in place and the stamp ages. The probe parses its own rendered bytes before the atomic `mv`, because a malformed file published is silent (node_exporter drops it and only `node_textfile_scrape_error` moves) while a file not published ages into this alert. Treat the journal line as the diagnosis: it prints the offending line.

Mitigation:

- [ ] Step 1: read the probe's own metrics to pick the arm (above).
- [ ] Step 2: for a query failure, run that query as `postgres` and read the error text; the probe discards stderr by design.
- [ ] Step 3: for a stale stamp, `systemctl start timescale-jobs-probe.service` and re-check the file's mtime.
- [ ] Step 4: fix the cause in `configs/ansible/roles/archival-node/tasks/10-observability.yml` and apply the role; never hand-edit `/usr/local/sbin`.
- [ ] Verification: every `stellarindex_timescale_probe_query_ok` reads 1, every `stellarindex_timescale_probe_rows` is non-zero, and `time() - stellarindex_timescale_probe_last_run_unix` stays under 60. The alert clears within 15 min.

Root cause analysis: `journalctl -u timescale-jobs-probe.service` for the whole degraded window (the unit exits 0 on a partial run by design, so a failure leaves no unit-level trace; the metrics are the record). Which of the three downstream alerts were blind, and for how long: `stellarindex_timescale_probe_rows` per query is the evidence, a family with zero rows over a window is a family no alert could read. If a TimescaleDB upgrade preceded it, the release notes for the `timescaledb_information` schema.

False positives:

- **A host with no TimescaleDB.** The probe is installed with the rest of observability (`run_observability`), not with Postgres (`run_postgres`), so a host configured without a database would report all three queries failing. No inventory does that today; if one ever should, remove the unit rather than muting the alert.
- **A brand-new host between database creation and migration.** The queries succeed and return nothing until the caggs and compression policies exist. `for: 15m` does not cover a long bring-up; this is expected noise during a build and clears with the first migration.
- **Not a false positive: a dropped row.** A row with a non-numeric field is omitted rather than published, and its query drops to `query_ok 0`. The value was never recoverable (a command tag or a NULL is evidence the reply did not have the expected shape, not a number to be repaired) and publishing it would have cost every family in the file, not just that one.
- **Not a false positive: a partial file.** One failing query still writes the other three families on purpose; dying instead would leave the previous file on disk to be re-scraped forever. A partial file with an honest `query_ok 0` is the designed outcome, not a bug.

## stellarindex_timescale_compression_lag

P3 (`severity: informational`, `for: 24h`). Typical MTTR 1 h - 1 day. Producer: `stellarindex_timescale_chunks_overdue_compression{hypertable=…}` is written by `timescale-jobs-probe.timer` into the textfile, one row per compression policy including zeros. If the probe stops, or its compression query fails or returns nothing, **every** row goes absent and this alert is blind rather than quiet; that state is alerted as `stellarindex_timescale_probe_degraded`.

Impact: not customer-visible directly. But uncompressed chunks use 5-20x more disk than compressed, so sustained lag is a runway to `db-disk-full.md`. The `for: 24h` threshold makes it a trending problem, not an incident.

**Not this alert:** a hypertable with **no** compression policy at all is invisible here by design; that is `compression_policies_applied` in `scripts/ops/config-assertions.sh`, surfaced through `stellarindex_config_assertion_failed`. On r1 2026-09-05 the `pools_per_source_1h` (92 GB) and `prices_1m` (52 GB) continuous-aggregate materialisations are entirely uncompressed for exactly that reason, and neither signal covers a CAGG.

Symptoms:

- `stellarindex_timescale_chunks_overdue_compression{hypertable="X"} > 0` sustained 24 h. "Overdue" means past hypertable X's OWN `compress_after`, plus one of its `schedule_interval`s of grace, not past a fixed 7 days. The alert names the hypertable.
- Disk usage growing faster than expected.
- `SELECT * FROM timescaledb_information.jobs WHERE proc_name IN ('policy_compression', 'trades_compression_policy')` shows failures or skipped runs. `trades` compresses through the custom job `trades_compression_policy` (migration 0205), which has no `hypertable_name` in the jobs view; the queries below map it to `trades`.

Quick diagnosis (<= 5 min):

```sh
# Which chunks are overdue? Same predicate the probe uses: each
# policy's own compress_after plus one schedule_interval of grace.
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

# Manual compression — does it work? Chunks live in the
# _timescaledb_internal schema; compress_chunk needs the
# schema-qualified name or it errors with "relation does not exist".
psql -c "SELECT compress_chunk('<chunk_schema>.<chunk_name>');"
```

Typical root causes:

1. **Compression job hitting a lock** with insert traffic. A hot chunk gets new rows while the job tries to compress it.
   - Mitigation: widen the `compress_after` interval so only truly-cold chunks are touched.
   - On `trades` the job asks for each chunk lock under a 5 s `lock_timeout` and fails the run ("columnstore policy failure") rather than queue readers behind it; a long reader of an old chunk (a historical CAGG refresh) shows up here as repeated failures until it finishes.
2. **Schema change conflicts**: an `ALTER TABLE` on the hypertable invalidates pending compression. TimescaleDB's compression is sensitive to schema.
   - Mitigation: complete the ALTER; may need to uncompress then recompress affected chunks.
3. **Disk IO saturated**: compression is IO-heavy. If the primary is close to IO limits, compression gets queued out.
   - Mitigation: scale IO (better disk, more parallelism caps).
4. **Job scheduler wedged** (same root cause as the cagg_stale scheduler issue).

Mitigation:

- [ ] Step 1: confirm which chunks are stuck + why (above).
- [ ] Step 2: try manual compression on one chunk to reproduce the error in isolation.
- [ ] Step 3: address the specific cause (lock, schema, IO).
- [ ] Step 4: catch up the backlog. You can run compression in parallel carefully:

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

- [ ] Verification: `stellarindex_timescale_chunks_overdue_compression{hypertable="X"}` drops to zero; disk usage trends back down.

False positives:

- **Recent schema migration**: the policy is disabled intentionally for a while until the migration completes. Silence during planned windows.
- **Historical backfill** adding new chunks for old data: those chunks are instantly past `compress_after` and the compression policy needs a cycle or two to catch up. The metric's built-in one-`schedule_interval` grace plus the rule's `for: 24h` cover 36 h of that; a backfill wider than two missed 12 h ticks will still surface. Expected; subsides.
- **Per-table `compress_after` longer than 7 days: RESOLVED 2026-09-05, no longer a false positive.** The metric used to count uncompressed chunks older than a hardcoded 7 days, so `fx_quotes` (90 days, `migrations/0028`), `aquarius_admin` and `defindex_fees` (30 days) had their by-design-uncompressed chunks counted as overdue. The producer now subtracts each policy's own `compress_after`. A nonzero value on those hypertables today is a real backlog, not their policy shape.
- **The daily sawtooth: RESOLVED 2026-09-05.** Every policy on r1 runs on a 12 h `schedule_interval`, so day-chunks crossed the old metric's 7-day line at 00:00 and were cleared by ~10:00: the metric read 2-28 for a third of every day and the alert sat permanently pending, saved from firing only by its 24 h `for:`. The producer now waits one `schedule_interval` past `compress_after` before counting a chunk, so a queued chunk is not lag. Measured on r1 at the metric swap: old query 3, peak 28 on a daily cycle; new query 0 across all 46 policies.

## stellarindex_timescale_cagg_stale

P2 ticket, `for: 5m`. Typical MTTR 15-60 min. Fires when `(time() - stellarindex_cagg_last_refresh_unix) > 5 * stellarindex_cagg_refresh_interval_seconds` for some `cagg` label for >= 5 min: a cagg's refresh is > 5x its interval overdue (the series exists but is old).

Producer: both metrics (`stellarindex_cagg_last_refresh_unix`, `stellarindex_cagg_refresh_interval_seconds`) come from `timescale-jobs-probe.timer`. If the probe stops ENTIRELY, or its refresh-policy query fails or returns zero rows, the series all go **absent** and both cagg alerts go blind rather than quiet.

Impact: `/v1/vwap`, `/v1/twap`, `/v1/ohlc` rely on these caggs; API queries either read stale windows or fall back to raw aggregation (slow). Price data accuracy for aggregate endpoints degrades.

Symptoms:

- `timescaledb_information.job_stats` shows `last_run_status != 'Success'` or `last_successful_finish` well behind expected; `timescaledb_information.job_errors` has the error text when it has a row for the run; it often does not (below).
- `/v1/vwap` responses have `observed_at` that doesn't track recent trades.

Quick diagnosis (<= 5 min). **Step 0: is the probe that produces the metric alive?** The alert reads textfile-collector series, not Postgres directly. A dead probe means absent series and a silently blind alert:

```sh
ssh root@136.243.90.96
systemctl status timescale-jobs-probe.timer --no-pager
ls -l --time-style=full-iso /var/lib/node_exporter/textfile_collector/timescale_jobs.prom
# mtime should be < 2 min old. If the probe is dead, fix THAT first —
# the CAGGs may be fine and you have no signal either way.
```

Then ask Postgres who's stale and why (run on r1 as postgres):

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
# No job_errors row for the stale cagg's job does NOT mean no error:
# read it from the server log instead (next paragraph).

# Is the timescaledb scheduler even running?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT * FROM pg_stat_activity WHERE application_name LIKE '%timescale%';"

# Try a manual refresh of the trailing window — does it succeed? Never a
# NULL start: on twap_1h/twap_1d that deletes history once prices_1m's
# retention is armed (data-freshness.md#stellarindex_twap_history_missing). <min_window> is the view's
# MinWindow in internal/storage/timescale/diagnostics.go (TradesCAGGs,
# OracleCAGGs, SupplyCAGG) — e.g. '3 hours' for *_1h, '3 days' for *_1d,
# '21 days' for *_1w, '93 days' for *_1mo; narrower fails 22023 "refresh
# window too small".
runuser -u postgres -- psql -d stellarindex -c \
  "CALL refresh_continuous_aggregate('<cagg_name>', now() - interval '<min_window>', now());"
```

**`job_errors` empty while the job is failing.** `total_failures` is a lifetime counter; `job_errors` keeps only what TimescaleDB's history-retention job has not dropped, and a run whose worker died before its error handler (crash, OOM kill, restart mid-run) may leave no row at all (r1 has shown failing jobs with the view empty). Take the error text from `/var/log/postgresql/postgresql-15-main.log` by the job's `application_name`, using the commands in [job_failures_climbing step 2](#stellarindex_timescale_job_failures_climbing).

Typical root causes:

1. **Refresh job encountering an error** that gets swallowed into `last_run_status = 'Failed'`. Could be: source hypertable constraint violation (bad data snuck in); lock conflict with a concurrent vacuum/migration; out-of-memory for a window function on a large window. Mitigation: read `err_message` from `timescaledb_information.job_errors`, or, if it has no row for the job, from the server log as above; address the specific error.
2. **timescaledb-scheduler hung.** The background scheduler worker can wedge (rarely). Restart Postgres (or just the scheduler-related background worker).
3. **Refresh runs but takes longer than its schedule interval.** Each run starts before the previous finishes; the scheduler skips queued runs and the CAGG falls behind. Mitigation: widen the schedule interval, or narrow the refresh window, or pre-aggregate upstream.
4. **Refresh window extends past current data.** If refresh is set to include `now()` with an `end_offset = 0`, it may skip windows that are still being written to. Mitigation: add an `end_offset` >= largest expected late-arriving event gap.

Mitigation:

- [ ] Step 1: read `err_message` in `timescaledb_information.job_errors` for the specific error; if it has no row for the job, read the server log as above.
- [ ] Step 2: manually refresh: `CALL refresh_continuous_aggregate(...)` to see if it's a one-off.
- [ ] Step 3: fix the underlying error (schema / constraint / lock).
- [ ] Step 4: once stable, re-enable the policy if it was disabled.
- [ ] Verification: `job_stats` shows `last_run_status = 'Success'` and `last_successful_finish` within the schedule interval; alert clears.

Root cause analysis: `timescaledb_information.job_errors` rows for every recent run, and the server-log ERROR lines for the runs it has no row for; timescaledb version + known bug tracker; were there schema changes on the source hypertable recently?; is the CAGG definition using a pattern known to be expensive (unbounded `time_bucket_gapfill` over very long windows)?

False positives:

- **Fresh CAGG creation** triggers this until the first refresh completes. Expected; the alert's `for: 5m` threshold helps.
- **Postgres restart**: the scheduler doesn't start refreshing until a few seconds after startup; briefly skips a cycle.
- **Probe dead is not the alert firing**: the inverse trap. If `timescale-jobs-probe.timer` stops, this alert goes silently absent rather than firing. Check probe health whenever the series flat-lines.

## stellarindex_timescale_cagg_refresh_missing

P2 ticket, `for: 15m`. Fires when the cagg's series existed within the last day and is absent now: its refresh policy was dropped or renamed, which `stellarindex_timescale_cagg_stale`'s `time() - x` cannot see over an absent series. It covers the narrower case the probe-health alert cannot: ONE cagg's row disappearing while the probe and every other cagg keep reporting fine.

Diagnose with step 0 and the `policy_refresh_continuous_aggregate` job listing in the cagg_stale section above (does the cagg still have a refresh policy?); a continuous aggregate that lost its refresh policy is also `caggs_have_refresh_policy` in `scripts/ops/config-assertions.sh`.

## stellarindex_late_trade_cagg_refresh_failing

Ticket. Source: `stellarindex_late_trade_cagg_refresh_total{outcome="error"}`, emitted by the indexer itself, not the probe. Each trades cagg policy re-aggregates only its last `start_offset` (`prices_1m`: 15 min), so trades the live writers land later than that, after a projector or dispatcher outage, are materialised only by the indexer's late-trade refresher (`internal/pipeline/late_trade_refresh.go`). It refreshes each affected view at most once per its policy's `schedule_interval`, non-forced, and retries a failure with backoff up to 10 min, so a single error is noise and a run of them means those buckets are under-reported now.

Diagnose: `journalctl -u stellarindex-indexer --since -1h | grep 'late-trade cagg refresh failed'` names the view, the refresh window and the trades window. The usual causes are the ones in [cagg_stale](#stellarindex_timescale_cagg_stale) (lock conflict with the policy job, `55P03`; statement timeout on a wide window). It clears on its own once a refresh succeeds (`outcome="ok"` increments). If it does not, refresh by hand over the logged window, non-forced, padded to the view's MinWindow:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "CALL refresh_continuous_aggregate('<view>', '<from>', '<to>');"
```

Refresh `prices_1m` before `twap_1h` / `twap_1d`: the twaps are materialised from it.

## stellarindex_late_trade_cagg_refresh_abandoned

Ticket. Source: `stellarindex_late_trade_cagg_refresh_total{outcome="abandoned"}`. At shutdown the indexer flushes the late-trade window once more, bounded at 30 s; if that fails or times out it counts `abandoned` and logs every pending window at ERROR. The increment happens just before `/metrics` shuts down, so a scrape can miss it: the log line is the record. A crash or SIGKILL skips the flush and leaves no record at all; after one that followed an outage, refresh the trades caggs over the outage window by hand.

```sh
journalctl -u stellarindex-indexer --since -2h | grep 'late-trade cagg refresh abandoned'
```

Each line carries `view`, `trades_from` and `trades_to`. Refresh each view over that window as in [the section above](#stellarindex_late_trade_cagg_refresh_failing), `prices_1m` first. Nothing else will: no policy reaches those buckets.

## Related

- [config-assertion-failed](config-assertion-failed.md): the same monitoring-of-monitoring shape one layer out, and the alert that catches codified-but-not-applied config.
- `db-disk-full.md`: where uncompressed chunks end up if unchecked, and a different cause of job failure worth ruling out.
- `api.md#stellarindex_api_latency_p95_high`: downstream effect when VWAP queries fall back to raw aggregation. `api.md#stellarindex_api_price_stale`: aggregator staleness visible through the API. `postgres.md#stellarindex_timescale_connections_saturated`: can cascade if a refresh is holding connections.
- Fixtures: `scripts/ci/timescale-jobs-probe-test.sh`; alert cases in `deploy/monitoring/rule-tests/storage_test.yml`.
