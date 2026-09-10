---
title: Runbook — pg-lock-convoy
last_verified: 2026-09-10
status: current
severity: P1
---

# Runbook — `stellarindex_pg_lock_convoy`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_pg_lock_convoy` |
| Severity | P1 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: page`, `for: 2m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. **Producer:** `timescale-jobs-probe.timer` (every 60 s), installed by `configs/ansible/roles/archival-node/tasks/10-observability.yml`, publishing `stellarindex_pg_lock_convoy_backends`, `stellarindex_pg_lock_convoy_wait_seconds_max` and `stellarindex_pg_lock_blocked_backends` through node_exporter's textfile collector. |
| Typical MTTR | 5–20 min (the 2026-09-10 convoy drained in under 20 s once its head was cancelled; finding the head took the rest) |
| Impact | Database-wide. Every request for the contended object queues, including work unrelated to the original contention: the API's reads, the size watchers, and **postgres_exporter's scrapes** — so most `pg_*` alerts go blind while this fires. |

## Why this exists

PostgreSQL's lock queue is fair, and that is the problem. Once a request
for an exclusive lock is **pending**, every later request for that object
is queued behind it, however trivial and however compatible it would have
been with the lock actually held. One blocked writer therefore blocks
readers that the current lock holder would have let through.

Measured on r1, 2026-09-10 00:12–00:31 UTC. A deploy restarted
`stellarindex-aggregator`; its cold-start VWAP alias-map aggregation
spilled to disk (`wait_event = IO/BufFileRead`) and held `AccessShareLock`
on `trades` for 18+ minutes. A `usd-volume-restamp -chunks` run was
mid-window and its `decompress_chunk` wanted `AccessExclusiveLock` on a
chunk of that hypertable. It queued, and the queue grew:

```
decompress_chunk (restamp)      blocked 1,984 s
UPDATE trades  x2 (restamp)     blocked 1,164 s
postgres_exporter scrapes x3    blocked   917 s
chunks_detailed_size (watcher)  blocked   904 s
```

Nothing paged on the convoy. What fired described the wreckage —
[`stellarindex_postgres_exporter_down`](exporter-down.md) (direct
scrape returned HTTP 000 after 30 s; Prometheus logged `context deadline
exceeded`) and [`stellarindex_aggregator_silent`](aggregator-silent.md)
— and **both systemd units read `active` throughout**. `/v1/status` went
`degraded`, the restamp stalled 33 minutes, and it was cleared by hand
with `pg_cancel_backend()` on the aggregator's `SELECT`.

Seven aggregator restarts in the preceding 14 hours did not jam. A
restart is not the trigger; the collision of a heavy cold-start read with
a restamp's decompress/compress phase is.

### Where this check runs, and why that is the point

`postgres_exporter` was **in** the convoy, so an alert carried by it could
not fire during the window it was needed. This gauge comes from a
different process on a different scrape path: `timescale-jobs-probe.sh`
opens its own short-lived `psql` connection and writes a textfile that
`node_exporter` serves. Its query reads only `pg_stat_activity` and
`pg_blocking_pids()`, which take no lock on `trades` — measured answering
in 7.5 ms with a real convoy in place, while `chunks_detailed_size` (which
calls `pg_relation_size`, and therefore opens the chunk at
`AccessShareLock`) was stuck.

### Why the predicate is "blocked behind a blocked blocker"

Decompressing the 159.7 GB `trades` chunk legitimately holds
`AccessExclusiveLock` for about 1.5 hours, and everything reading that
chunk legitimately waits behind it. An alert on "backends are waiting"
would fire on every healthy restamp — and an alert that fires on every
healthy run is one the operator stops reading.

`stellarindex_pg_lock_convoy_backends` counts only backends whose blocker
is **itself** blocked. In the incident the exporter waited on the
decompress, which was waiting on the aggregator. Under a healthy
decompress the waiters' blocker is *running*, so the count is zero.

## Symptoms

- `stellarindex_pg_lock_convoy_wait_seconds_max > 120` for 2 min — the
  alert. The value is the longest wait among the convoyed backends.
- `stellarindex_pg_lock_convoy_backends` climbing — the queue is growing,
  not draining.
- Very likely alongside: `stellarindex_postgres_exporter_down`,
  `stellarindex_aggregator_silent`, `/v1/status` = `degraded`, API
  latency alerts. **Treat all of those as consequences until proven
  otherwise.**
- `systemctl status` on every unit reads `active`. Unit state is useless
  here; the processes are alive and waiting.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. The whole chain, blocked backends and who blocks them. This reads
#    shared memory only — it is answerable from inside the convoy.
runuser -u postgres -- psql -d stellarindex -x -c "
  SELECT a.pid,
         a.wait_event_type,
         pg_blocking_pids(a.pid)                        AS blocked_by,
         now() - a.query_start                          AS waited,
         a.application_name,
         a.client_port,
         left(a.query, 120)                             AS query
    FROM pg_stat_activity a
   WHERE a.wait_event_type = 'Lock'
   ORDER BY a.query_start"

# 2. The HEAD of the chain: the backend that is blocking others and is
#    itself blocked by something that is NOT waiting. This is the one
#    decision to make.
runuser -u postgres -- psql -d stellarindex -x -c "
  SELECT h.pid, now() - h.query_start AS running, left(h.query, 200) AS query
    FROM pg_stat_activity h
   WHERE h.wait_event_type IS DISTINCT FROM 'Lock'
     AND h.pid IN (SELECT unnest(pg_blocking_pids(a.pid))
                     FROM pg_stat_activity a
                    WHERE a.wait_event_type = 'Lock')"
```

`client_port` correlates a backend to the owning OS process
(`ss -tnp | grep <port>`) when `application_name` is empty — that is how
the 2026-09-10 head was identified as the aggregator rather than the
restamp.

## Typical root causes

1. **A long read colliding with a chunk decompress/compress.** The
   2026-09-10 shape: an aggregator cold start (or any multi-minute scan of
   `trades`) overlapping a `usd-volume-restamp -chunks` window. The
   restamp no longer contributes the pending request — it asks under
   `SET LOCAL lock_timeout` and retries (see
   `internal/storage/timescale/trades_chunks.go`) — so if this recurs with
   a restamp at the head, check whether the deployed binary predates that
   change.
2. **A by-hand `ALTER TABLE` / `CREATE INDEX` / `VACUUM FULL` on a busy
   relation.** Any of them takes `AccessExclusiveLock` and will convoy a
   live table. Run these under an explicit `SET lock_timeout` in the same
   session, and retry, rather than letting one park.
3. **A migration applied against live traffic.** Migrations auto-deploy
   (`deploy.yml` syncs and applies), so a DDL migration can land mid-day.
4. **The TimescaleDB compression policy's own proc** compressing a chunk
   a long reader is scanning.
5. **An idle-in-transaction session** holding a lock nobody expects.
   `state = 'idle in transaction'` on the head of the chain is the tell.

## Mitigation

- [ ] Step 1 — run the two queries above. Do not act on anything until
      you have the **head** of the chain: cancelling a backend in the
      middle frees its own slot and nothing else, and the convoy re-forms.
- [ ] Step 2 — decide about the head, and only the head.
      - A long **read** that is safe to lose (an aggregator scan, an
        ad-hoc query): `SELECT pg_cancel_backend(<pid>);`. This is what
        cleared 2026-09-10; the convoy drained in under 20 s.
      - A **decompress/compress** that is holding the lock and *working*:
        let it finish. `chunks_detailed_size` on that chunk climbing means
        real progress; flat for minutes means wedged. Killing it mid-flight
        can leave a 160 GB chunk decompressed with the compression policy
        paused — see [ops-job-stalled](ops-job-stalled.md) and
        `docs/operations/usd-volume-rederive-2026-08.md` before you do.
      - A **DDL** you or a deploy started: cancel it and re-run it with a
        `lock_timeout` and a retry.
- [ ] Step 3 — prefer `pg_cancel_backend` over `pg_terminate_backend`.
      Cancel aborts the statement and rolls its transaction back cleanly;
      terminate drops the connection and can leave a client reconnecting
      into the same convoy.
- [ ] Step 4 — once it drains, confirm the alerting layer came back:
      `stellarindex_postgres_exporter_down` clears and
      `up{job="postgres"}` returns to 1. Anything still firing after that
      is a real problem rather than a shadow of this one.
- [ ] Verification: `stellarindex_pg_lock_convoy_backends` reads 0 and
      `stellarindex_pg_lock_convoy_wait_seconds_max` reads 0 on the next
      probe tick (≤ 60 s). The alert clears within 2 min.

## Root cause analysis

- The chain, written down before you cancel anything. `pg_stat_activity`
  is not retained; once the convoy drains the evidence is gone. Copy the
  output of the first query into the incident note.
- What the head was doing and why it was slow. For an aggregator scan,
  `wait_event = IO/BufFileRead` means it spilled to disk — a work_mem or
  cold-cache question, not a lock question.
- Whether a deploy, a migration or a restamp window opened in the ten
  minutes before. All three are visible in the deploy workflow and
  `journalctl -u stellarindex-aggregator`.
- How long `postgres_exporter` was blind, from
  `stellarindex_pg_lock_convoy_wait_seconds_max` and the exporter's `up`
  series. Every `pg_*` alert was unable to fire for that window; say so
  explicitly rather than reading their silence as health.

## Related

- [timescale-probe-degraded](timescale-probe-degraded.md) — the same
  producer's self-report. If the probe is degraded, this alert is blind
  too; that one is the meta-alert and takes precedence.
- [exporter-down](exporter-down.md) — usually a
  consequence of this, not an independent fault.
- [aggregator-silent](aggregator-silent.md) — the aggregator stops
  writing VWAP when its own reads are queued.
- [pg-conns-saturated](pg-conns-saturated.md) — a convoy pins connections
  as it grows, so the two can fire together.
- [ops-job-stalled](ops-job-stalled.md) — the heavy-job side: a restamp
  that is queued rather than wedged looks identical from its heartbeat,
  and its by-hand repair for a chunk left decompressed is there.
