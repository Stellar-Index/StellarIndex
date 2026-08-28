---
title: Runbook — cagg-stale
last_verified: 2026-08-28
status: current
severity: P2
---

# Runbook — `stellarindex_timescale_cagg_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_cagg_stale` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. **Producer:** both metrics (`stellarindex_cagg_last_refresh_unix`, `stellarindex_cagg_refresh_interval_seconds`) come from `timescale-jobs-probe.timer` (every 60 s), a shell probe installed by `configs/ansible/roles/archival-node/tasks/10-observability.yml` that writes `/var/lib/node_exporter/textfile_collector/timescale_jobs.prom` for the node_exporter textfile collector. If the probe dies, the series go **absent** and the alert goes blind — it does not fire. |
| Typical MTTR | 15–60 min |
| Impact | A continuous aggregate is > 5× its refresh interval overdue. `/v1/vwap`, `/v1/twap`, `/v1/ohlc` rely on these — API queries either read stale windows or fall back to raw aggregation (slow). Price data accuracy for aggregate endpoints degrades. |

## Symptoms

- `(time() - stellarindex_cagg_last_refresh_unix) > 5 * stellarindex_cagg_refresh_interval_seconds`
  for some `cagg` label for ≥ 5 min.
- `timescaledb_information.job_stats` shows `last_run_status != 'Success'`
  or `last_successful_finish` well behind expected;
  `timescaledb_information.job_errors` has the error text.
- `/v1/vwap` responses have `observed_at` that doesn't track
  recent trades.

## Quick diagnosis (≤ 5 min)

**Step 0 — is the probe that produces the metric alive?** The alert
reads textfile-collector series, not Postgres directly. A dead probe
means absent series and a silently blind alert:

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

# Is the timescaledb scheduler even running?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT * FROM pg_stat_activity WHERE application_name LIKE '%timescale%';"

# Try a manual refresh — does it succeed?
runuser -u postgres -- psql -d stellarindex -c \
  "CALL refresh_continuous_aggregate('<cagg_name>', NULL, NULL);"
```

## Typical root causes

1. **Refresh job encountering an error** that gets swallowed into
   `last_run_status = 'Failed'`. Could be:
   - Source hypertable constraint violation (bad data snuck in)
   - Lock conflict with a concurrent vacuum/migration
   - Out-of-memory for a window function on a large window
   - Mitigation: read `err_message` from
     `timescaledb_information.job_errors`; address the specific error.

2. **timescaledb-scheduler hung**. The background scheduler
   worker can wedge (rarely). Restart Postgres (or just the
   scheduler-related background worker).

3. **Refresh runs but takes longer than its schedule interval.**
   Each run starts before the previous finishes; the scheduler
   skips queued runs and the CAGG falls behind.
   - Mitigation: widen the schedule interval, or narrow the
     refresh window, or pre-aggregate upstream.

4. **Refresh window extends past current data.** If refresh is
   set to include `now()` with an `end_offset = 0`, it may skip
   windows that are still being written to.
   - Mitigation: add an `end_offset` ≥ largest expected
     late-arriving event gap.

## Mitigation

- [ ] Step 1 — read `err_message` in
      `timescaledb_information.job_errors` for the specific error.
- [ ] Step 2 — manually refresh: `CALL refresh_continuous_aggregate(...)`
      to see if it's a one-off.
- [ ] Step 3 — fix the underlying error (schema / constraint /
      lock).
- [ ] Step 4 — once stable, re-enable the policy if it was
      disabled.
- [ ] Verification: `job_stats` shows `last_run_status = 'Success'`
      and `last_successful_finish` within the schedule interval;
      alert clears.

## Root cause analysis

- `timescaledb_information.job_errors` rows for every recent run.
- timescaledb version + known bug tracker.
- Were there schema changes on the source hypertable recently?
- Is the CAGG definition using a pattern known to be expensive
  (unbounded `time_bucket_gapfill` over very long windows)?

## Known false-positive patterns

- **Fresh CAGG creation** triggers this until the first refresh
  completes. Expected; the alert's `for: 5m` threshold helps.
- **Postgres restart** — scheduler doesn't start refreshing until
  a few seconds after startup; briefly skips a cycle.
- **Probe dead ≠ alert firing** — the inverse trap: if
  `timescale-jobs-probe.timer` stops, this alert goes silently
  absent rather than firing. Check probe health whenever the
  series flat-lines.

## Related

- `api-latency.md` — downstream effect when VWAP queries fall
  back to raw aggregation.
- `price-stale.md` — aggregator staleness visible through the API.
- `pg-conns-saturated.md` — can cascade if the refresh is holding
  connections.

## Changelog

- 2026-08-28 — re-verified against HEAD. Quick-diagnosis SQL rewritten:
  `job_stats` has no `last_finish`/`last_run_err` columns
  (→ `last_successful_finish`; error text lives in
  `timescaledb_information.job_errors.err_message`). Interval metric name
  corrected to `stellarindex_cagg_refresh_interval_seconds`. Documented
  the producer (`timescale-jobs-probe.timer` → node_exporter textfile
  collector) and added the probe-health check as diagnosis step 0; rule
  citation → `rules.r1/storage.yml`; commands use r1 shapes
  (`ssh root@136.243.90.96`, `runuser -u postgres -- psql -d stellarindex`).
- 2026-04-23 — initial draft.
