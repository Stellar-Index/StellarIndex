---
title: Runbook — stellarindex_timescale_job_failures_climbing
last_verified: 2026-08-30
status: ratified
severity: P3
---

# Runbook — `stellarindex_timescale_job_failures_climbing`

## At a glance

| | |
| --- | --- |
| **Severity** | informational (P3) — no immediate customer impact |
| **Fires when** | a single TimescaleDB background job accumulates >10 failed runs in 6h, sustained 30m |
| **Producer** | `timescale-jobs-probe.timer` on r1 (60s), writing `timescale_jobs.prom` into the node_exporter textfile dir |
| **Metric** | `stellarindex_timescale_job_failures_total{job_id,proc,hypertable}` |
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

## Quick diagnosis (≤ 5 min)

1. **Which job, and is it starving or erroring?**

   ```sql
   SELECT job_id, proc_name, hypertable_name, total_runs, total_failures,
          last_run_status, last_run_started_at
     FROM timescaledb_information.job_stats js
     JOIN timescaledb_information.jobs j USING (job_id)
    ORDER BY total_failures DESC LIMIT 10;
   ```

2. **Read the actual error** — this is the branch point:

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
  `for: 30m` is sized to ride those out.
- **This alert alone, with caggs fresh**, is not a customer-facing
  incident. It is a warning that the safety margin is gone.

## Related

- [`cagg-stale.md`](cagg-stale.md) — the ticket-severity sibling; fires when retries stop hiding the failures.
- [`db-disk-full.md`](db-disk-full.md) — a different cause of job failure worth ruling out.
- Producer + the incident that motivated the counter: `configs/ansible/roles/archival-node/tasks/10-observability.yml` (TimescaleDB job/CAGG health probe).
