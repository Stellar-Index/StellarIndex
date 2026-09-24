---
title: Runbook — stellarindex_timescale_job_failures_climbing
last_verified: 2026-09-18
status: ratified
severity: P3
---

# Runbook — `stellarindex_timescale_job_failures_climbing`

## At a glance

| | |
| --- | --- |
| **Severity** | ticket — no immediate customer impact, but the safety margin is gone |
| **Fires when** | a single TimescaleDB background job accumulates **>10 failed runs in 6h**, **3 or more in 3 days**, or failures covering **at least half the runs its `schedule_interval` gave it in 3 days** (failures × interval ≥ 36h), sustained 30m |
| **Producer** | `timescale-jobs-probe.timer` on r1 (60s), writing `timescale_jobs.prom` into the node_exporter textfile dir. If that probe stops, or its `job_stats` query fails or returns nothing, this counter goes absent and the alert is blind rather than quiet — `stellarindex_timescale_probe_degraded` ([timescale-probe-degraded](timescale-probe-degraded.md)) is the standing signal for it. |
| **Metric** | `stellarindex_timescale_job_failures_total{job_id,proc,hypertable}`, weighted by `stellarindex_timescale_job_schedule_interval_seconds` (same labels, same probe) |
| **Customer impact** | usually none *yet* — TimescaleDB retries on the next tick |

**Why this alert exists at all.** r1 once failed **37–69 % of every CAGG
refresh run** with `failed to start job` — background-worker starvation —
and *nothing surfaced it*. The jobs got a slot on a later tick, so
`last_run_status` read `Success`, the caggs were never stale, and both
`stellarindex_timescale_cagg_stale` and
`stellarindex_timescale_compression_lag` stayed correctly quiet. The only
evidence was this counter, which no rule referenced until 2026-08-30
(wave-D ALERT-12).

So treat a firing here as **"the thing that hid the last incident is
happening again"**, not as a broken job.

**Why three arms.** The counter is per job
(`{job_id,proc,hypertable}`), so a job scheduled every *T* can accrue at
most `6h / T` failures — and `> 10 in 6h` therefore needs *T* under ~36
minutes. Every compression policy (12h `schedule_interval`) and five of
the CAGG refresh policies (`prices_4h` 1h, `prices_1d` 6h, `prices_1w`
and `prices_1mo` 1d, `twap_1d` 6h) are slower than that, so for a year
this alert was *arithmetically unable* to fire for them — the same jobs
the r1 probe found failing 66–81 %. The second arm, **3 or more failures
in 3 days**, is the one that judges the slow half of the fleet: a job on
a daily schedule failing every run trips it on the third day, a 12h
compression policy inside two.

The 3-day count still caps out: a job scheduled every *T* runs at most
`3d / T` times in the window, so `≥ 3` is unreachable once *T* exceeds a
day. The third arm is derived from the job's own `schedule_interval`
instead of a fixed count: `increase(failures[3d]) × schedule_interval ≥
129600` (36h, half the window) means at least half of the runs scheduled
in 3 days failed, whatever *T* is — one failure for *T* ≥ 1.5 days, two
of three for a daily job, three of six for a 12h policy (the same bar as
the second arm). At a 1m schedule it needs 2160 failures, so fast jobs
stay with the 6h arm. The probe emits the interval as
`stellarindex_timescale_job_schedule_interval_seconds` with the
counter's exact labels; a job whose interval does not parse still gets
its counter (the first two arms still judge it) but no gauge, and the
probe reports `query_ok{query="job_stats"} 0` so
`stellarindex_timescale_probe_degraded` says the third arm is blind.

Measured on r1 (2026-09-18) while adding it: `policy_compression` on
`trades` (job 1000, 12h schedule) had failed **6 times in 7 days** with
`Failed to convert '1' chunks to columnstore`, three of them inside
three hours, and no rule could fire on it. Over the same week the only
other failing job was the `prices_1m` refresh, with a single failure —
below the new threshold, which is what keeps ordinary retry noise out of
the ticket queue.

The 3-day window is not a week on purpose: local Prometheus retains 7
days (`local_prometheus_retention_time`), and an `increase()` window at
the retention edge loses its left-hand samples silently.

## Quick diagnosis (≤ 5 min)

1. **Which job, and is it starving or erroring?**

   ```sql
   SELECT job_id, proc_name, hypertable_name, total_runs, total_failures,
          last_run_status, last_run_started_at
     FROM timescaledb_information.job_stats js
     JOIN timescaledb_information.jobs j USING (job_id)
    ORDER BY total_failures DESC LIMIT 10;
   ```

2. **Read the actual error** — this is the branch point. The most recent
   message per job is already in Prometheus as the `reason` label of
   `stellarindex_timescale_job_last_failure_reason_info{job_id="…"}`
   (truncated to 200 characters, braces shown as parentheses); for the
   full history:

   ```sql
   SELECT job_id, proc_name, err_message, finish_time
     FROM timescaledb_information.job_errors
    ORDER BY finish_time DESC LIMIT 20;
   ```

   | `err_message` | Meaning | Go to |
   | --- | --- | --- |
   | `failed to start job` | **Starvation** — no background worker slot was free | §Starvation |
   | anything else (SQL error, OOM, lock timeout) | The job body genuinely failed | §Job body |

## Starvation (the known-incident shape)

Check headroom — the fix last time was in `postgresql.conf.j2`:

```sql
SHOW timescaledb.max_background_workers;   -- must exceed the job count
SHOW max_parallel_workers;
SHOW max_worker_processes;                 -- must exceed the sum of the above
SELECT count(*) FROM timescaledb_information.jobs;
```

`timescaledb.max_background_workers` must be **greater than the number of
scheduled jobs**, and `max_worker_processes` must cover background workers
plus parallel workers plus a margin. If they are tight, raise them in
`configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and
apply — **a Postgres restart is required**, so schedule it.

## Job body genuinely failing

Not starvation — read the error and treat it as an ordinary job failure.
If it is a CAGG refresh, `stellarindex_timescale_cagg_stale` will follow
once retries stop covering it; that ticket is the customer-impact signal
and takes priority over this one.

## When NOT to act

- **A short burst after a restart or a heavy one-shot job** is expected —
  contention clears and the counter stops climbing. The 6h window plus
  `for: 30m` is sized to ride those out. The 3-day arm needs three
  failures, so a single retried blip does not reach it either — except
  on a job scheduled every 1.5 days or slower, where one failed run is
  already half of its runs in the window and is worth the ticket.
- **This alert alone, with caggs fresh**, is not a customer-facing
  incident. It is a warning that the safety margin is gone.

## Related

- [`cagg-stale.md`](cagg-stale.md) — the ticket-severity sibling; fires when retries stop hiding the failures.
- [`db-disk-full.md`](db-disk-full.md) — a different cause of job failure worth ruling out.
- Producer + the incident that motivated the counter: `configs/ansible/roles/archival-node/tasks/10-observability.yml` (TimescaleDB job/CAGG health probe).
- [`timescale-probe-degraded.md`](timescale-probe-degraded.md) — the producer side: the alert that fires when this counter's own source has stopped reporting.
