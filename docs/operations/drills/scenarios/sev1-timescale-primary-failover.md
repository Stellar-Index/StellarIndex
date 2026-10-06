---
title: SEV-1 tabletop — Timescale primary disk-full failover
last_verified: 2026-10-05
status: ratified
severity: P1
exercises_runbook: ../../runbooks/postgres.md#stellarindex_timescale_primary_down
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-1 tabletop — Timescale primary disk-full

~30 min, 3 people. Exercises [`postgres.md`](../../runbooks/postgres.md#stellarindex_timescale_primary_down)
and [SEV playbook §4](../../sev-playbook.md). r1 has no automatic failover; recovery
is operator-driven. Successor once a Patroni cluster exists:
[sev1-patroni-failover](sev1-patroni-failover.md).

## Setup and trigger

All services up, `/v1/readyz` ok, 14:30 UTC Tuesday, routine traffic. Oncall and backup named.

> 14:32 UTC: the primary rejects writes with `FATAL: out of disk space`; `pgdata` has 0 bytes
> free. The replica is healthy but read-only; failover is manual. API 5xx rises from <0.1% to 4% in 60 s.

## Beats (T+ min:sec)

| T+ | Beat |
| --- | --- |
| 0:00 | `stellarindex_api_error_rate_high` (>1% for 2m) pages oncall |
| 0:30 | `stellarindex_api_error_rate_critical` (>5% for 2m) |
| 1:00 | `stellarindex_timescale_disk_full` (<10% free) |
| 1:30 | `stellarindex_ingestion_insert_errors` for every source |
| 2:00 | `stellarindex_api_price_stale`, ~60 s after the cache TTL bleeds |
| 5:00 | Customer (Freighter) DMs about a degraded price feed |
| 8:00 | Replica disk 30% free, so failover budget exists |
| 12:00 | pgBackRest archive healthy; backups not corrupted |
| 25:00 | Disk-clearing options still being weighed; engineering manager joins |

## Expected response

- **5 min** ([§2](../../sev-playbook.md#2-timelines-the-sla-promises)): acknowledge; open `#incident-<YYYY-MM-DD>-<short>`;
  post "API 5xx elevated; investigating; updates every 15 min"; status page *Investigating*.
- **15 min, diagnose:** confirm `postgres` check failing on `/v1/readyz` (before metrics); root cause is
  disk-full on `pgdata` (runbook root cause #1); decide failover (promote replica, repair primary later)
  vs fix-in-place, with stated rationale.
- **30 min, mitigate:** replica serving, either by promotion per the runbook (manual) or by freeing space
  and restarting. Disk relief is pool-level ([infra.md#stellarindex_zfs_pool_low_space](../../runbooks/infra.md#stellarindex_zfs_pool_low_space)); never `drop_chunks`
  on data tables ([db-disk-full.md](../../runbooks/db-disk-full.md)).
- **1 h, communicate:** status page *Investigating, Identified, Mitigated*; customer post; update every 15 min.
- **24 h:** postmortem per [§6](../../sev-playbook.md#6-after-the-incident), action items with owner and due date.

## Pass criteria (aim >= 80% pass)

1. Acknowledged within 5 min and channel opened.
2. Found `postgres.md` first try (alert `runbook_url`).
3. Confirmed against `/v1/readyz`, not only the metric.
4. Identified disk-full as root cause within 15 min.
5. Chose failover vs fix-in-place with explicit rationale.
6. Recognised the missing automatic failover (Patroni/HAProxy roles not deployed) as the gap that makes this slow.
7. Used the customer-comms templates with no violations of [§5.4](../../sev-playbook.md#54-what-we-do-not-say).
8. Writeup within 24 h with action items.

## Variants

Network partition (primary unreachable from API and indexer, replica reachable read-only);
slow degradation (disk fills over 6 h from a stuck pgBackRest job; tests `stellarindex_timescale_disk_warning`).
