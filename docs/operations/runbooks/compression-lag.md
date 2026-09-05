---
title: Runbook — compression-lag
last_verified: 2026-09-05
status: current
severity: P3
---

# Runbook — `stellarindex_timescale_compression_lag`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_compression_lag` |
| Severity | P3 (`severity: informational`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: informational`, `for: 24h`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. **Producer:** `stellarindex_timescale_chunks_overdue_compression{hypertable=…}` is written by `timescale-jobs-probe.timer` (every 60 s, `configs/ansible/roles/archival-node/tasks/10-observability.yml`) into `/var/lib/node_exporter/textfile_collector/timescale_jobs.prom`, one row per compression policy including zeros. If the probe dies **every** row goes absent and the alert is blind — check `systemctl status timescale-jobs-probe.timer` before trusting silence. |
| Typical MTTR | 1 h – 1 day |
| Impact | Not customer-visible directly. But uncompressed chunks use 5–20× more disk than compressed, so sustained lag is a runway to `db-disk-full.md`. The alert's `for: 24h` threshold makes it a trending problem, not an incident. |
| Not this alert | A hypertable with **no** compression policy at all is invisible here by design — that is `compression_policies_applied` in `scripts/ops/config-assertions.sh`, surfaced through `stellarindex_config_assertion_failed`. On r1 2026-09-05 the `pools_per_source_1h` (92 GB) and `prices_1m` (52 GB) continuous-aggregate materialisations are entirely uncompressed for exactly that reason, and neither signal covers a CAGG. |

## Symptoms

- `stellarindex_timescale_chunks_overdue_compression{hypertable="X"} > 0`
  sustained 24 h. "Overdue" means past hypertable X's OWN
  `compress_after`, plus one of its `schedule_interval`s of grace —
  not past a fixed 7 days. The alert names the hypertable.
- Disk usage growing faster than expected.
- `SELECT * FROM timescaledb_information.jobs WHERE proc_name =
  'policy_compression'` shows failures or skipped runs.

## Quick diagnosis (≤ 5 min)

```sh
# Which chunks are overdue? Same predicate the probe uses: each
# policy's own compress_after plus one schedule_interval of grace.
psql -c "SELECT c.hypertable_name, c.chunk_schema, c.chunk_name,
                c.range_start, c.range_end,
                j.config->>'compress_after' AS compress_after,
                j.schedule_interval
         FROM timescaledb_information.jobs j
         JOIN timescaledb_information.chunks c
              ON c.hypertable_schema = j.hypertable_schema
             AND c.hypertable_name = j.hypertable_name
         WHERE j.proc_name = 'policy_compression'
           AND c.is_compressed = false
           AND c.range_end < now()
               - COALESCE((j.config->>'compress_after')::interval, interval '7 days')
               - j.schedule_interval
         ORDER BY c.range_end
         LIMIT 20;"

# Why is the job failing?
psql -c "SELECT * FROM timescaledb_information.job_stats
         WHERE job_id IN (SELECT job_id FROM timescaledb_information.jobs
                          WHERE proc_name = 'policy_compression');"

# Manual compression — does it work? Chunks live in the
# _timescaledb_internal schema; compress_chunk needs the
# schema-qualified name or it errors with "relation does not exist".
psql -c "SELECT compress_chunk('<chunk_schema>.<chunk_name>');"
```

## Typical root causes

1. **Compression job hitting a lock** with insert traffic. A
   hot chunk gets new rows while the job tries to compress it.
   - Mitigation: widen the `compress_after` interval so only
     truly-cold chunks are touched.

2. **Schema change conflicts** — a `ALTER TABLE` on the hypertable
   invalidates pending compression. TimescaleDB's compression is
   sensitive to schema.
   - Mitigation: complete the ALTER; may need to uncompress then
     recompress affected chunks.

3. **Disk IO saturated** — compression is IO-heavy. If the
   primary is close to IO limits, compression gets queued out.
   - Mitigation: scale IO (better disk, more parallelism caps).

4. **Job scheduler wedged** (same root cause as
   `cagg-stale.md`'s scheduler issue).

## Mitigation

- [ ] Step 1 — confirm which chunks are stuck + why (above).
- [ ] Step 2 — try manual compression on one chunk to reproduce
      the error in isolation.
- [ ] Step 3 — address the specific cause (lock, schema, IO).
- [ ] Step 4 — catch up the backlog. You can run compression
      in parallel carefully:
      ```sh
      psql -c "SELECT compress_chunk(c.chunk_schema || '.' || c.chunk_name)
               FROM timescaledb_information.jobs j
               JOIN timescaledb_information.chunks c
                    ON c.hypertable_schema = j.hypertable_schema
                   AND c.hypertable_name = j.hypertable_name
               WHERE j.proc_name = 'policy_compression'
                 AND c.is_compressed = false
                 AND c.range_end < now()
                     - COALESCE((j.config->>'compress_after')::interval, interval '7 days')
                     - j.schedule_interval
               LIMIT 10;"
      ```
- [ ] Verification:
      `stellarindex_timescale_chunks_overdue_compression{hypertable="X"}`
      drops to zero; disk usage trends back down.

## Known false-positive patterns

- **Recent schema migration** — the policy is disabled
  intentionally for a while until the migration completes.
  Silence during planned windows.
- **Historical backfill** adding new chunks for old data — those
  chunks are instantly past `compress_after` and the compression
  policy needs a cycle or two to catch up. The metric's built-in
  one-`schedule_interval` grace plus the rule's `for: 24h` cover
  36 h of that; a backfill wider than two missed 12 h ticks will
  still surface. Expected; subsides.
- **Per-table `compress_after` longer than 7 days — RESOLVED
  2026-09-05, no longer a false positive.** The metric used to
  count uncompressed chunks older than a hardcoded 7 days, so
  `fx_quotes` (90 days, `migrations/0028`), `aquarius_admin` and
  `defindex_fees` (30 days) had their by-design-uncompressed chunks
  counted as overdue. The producer now subtracts each policy's own
  `compress_after`. A nonzero value on those hypertables today is a
  real backlog, not their policy shape.
- **The daily sawtooth — RESOLVED 2026-09-05.** Every policy on r1
  runs on a 12 h `schedule_interval`, so day-chunks crossed the old
  metric's 7-day line at 00:00 and were cleared by ~10:00: the
  metric read 2–28 for a third of every day and the alert sat
  permanently pending, saved from firing only by its 24 h `for:`.
  The producer now waits one `schedule_interval` past
  `compress_after` before counting a chunk, so a queued chunk is
  not lag.

## Related

- `db-disk-full.md` — where this ends up if unchecked.
- `cagg-stale.md` — related scheduler issues; same
  `timescale-jobs-probe.timer` producer.

## Changelog

- 2026-09-05 — metric replaced:
  `stellarindex_uncompressed_chunks_older_than_7d` (one unlabelled
  scalar, hardcoded 7 days) →
  `stellarindex_timescale_chunks_overdue_compression{hypertable=…}`
  (per policy, judged against that policy's own `compress_after`
  plus one `schedule_interval`). Both "known false-positive"
  entries this runbook carried — per-table `compress_after` and the
  backfill sawtooth — were the metric being wrong, and are now
  fixed rather than documented. Measured on r1: old query 3, peak
  28 on a daily cycle; new query 0 across all 46 policies. Added
  the "not this alert" row for policy-less hypertables.
- 2026-08-28 — re-verified against HEAD. `compress_chunk` example now
  schema-qualified (chunks live in `_timescaledb_internal`); rule
  citation → `rules.r1/storage.yml` with the `timescale-jobs-probe.timer`
  producer noted (probe dead ⇒ alert blind); added the per-table
  `compress_after` false-positive (`fx_quotes` = 90 d, migration 0028).
- 2026-04-23 — initial draft.
