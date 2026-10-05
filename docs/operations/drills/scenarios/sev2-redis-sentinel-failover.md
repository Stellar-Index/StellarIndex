---
title: SEV-2 tabletop — Redis Sentinel master failover under live traffic
last_verified: 2026-10-05
status: draft
severity: P2
exercises_runbook: ../../runbooks/redis-master-down.md
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-2 tabletop — Redis Sentinel master failover

> **Infra not deployed.** The `redis-sentinel` role (ADR-0024) exists but no playbook runs it on r1. Run this when a Sentinel cluster exists (r2/r3).

~30 min, 3 people. Exercises the Redis-dependent surface: `/v1/price` (closed-bucket VWAP cache, freeze markers, confidence, triangulation),
`/v1/account/*` (API-key validator), `/v1/assets/{id}/metadata` (SEP-1 cache). Redis loss **degrades** rather than kills; most criteria test degraded-vs-down.

## Setup and trigger

`cache-01` master, `cache-02`/`cache-03` replicas, `redis_exporter` up on all three. 17:45 UTC Wednesday, ~1.2k req/min mostly `/v1/price`.

> 17:46 UTC `cache-01` becomes unresponsive (OOM-kill after a rogue `SCAN MATCH` from a debug session). Sentinel detects within 30 s and promotes
> `cache-02` (~5 s); API connections to Redis time out meanwhile. `/v1/price` p99 spikes 80 ms to 1.8 s for ~10 s; `flags.frozen` and
> `flags.divergence_warning` pause (freeze markers unreadable until the new master accepts traffic).

## Beats (T+ min:sec)

| T+ | Beat |
| --- | --- |
| 0:00 | `stellarindex_redis_master_down` (`redis_up == 0` for 30s) |
| 0:30 | Sentinel promoting `cache-02`; `stellarindex_ratelimit_fail_open_total` spikes |
| 1:00 | `cache-02` accepts writes; replicas re-attach; `flags.frozen` paths re-enable as cache catches up |
| 2:00 | Customer: a few 503s on `/v1/price` |
| 5:00 | Metrics baseline, but `flags.frozen` fires on a pair not frozen pre-failover: stale marker or legitimate aggregator freeze during the outage? |
| 10:00 | `cache-01` restarted, rejoins as replica; Sentinel does not fail back (ADR-0024) |
| 20:00 | Customer asks if stored API keys are affected (no: validator cache is read-through, a master swap loses no record) |

## Expected response

- **5 min:** acknowledge; open `#incident-<YYYY-MM-DD>-redis-failover`; post "brief 503s on `/v1/price` from a Redis cache failover; recovering automatically";
  status *Degraded performance* on API (not *Major outage*).
- **15 min, diagnose:** `/v1/readyz` `redis` check back to ok; `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL get-master-addr-by-name stellarindex-r1-cache`
  shows the new master (password required since Sentinel got `requirepass`); root cause from `cache-01` host logs and `redis-server` stderr; do **not** fail back.
- **30 min, verify side-effects:** `/v1/price` 5xx back to <=0.1%; `/v1/account/me` serves; `flags.frozen` markers repopulated (spot-check
  `redis-cli GET freeze:<asset>:<quote>`); canary `curl -sS https://api.stellarindex.io/v1/price?asset=native | jq '.flags'`.
  `flags.frozen` firing after failover is expected: a stale marker is re-evaluated by the aggregator's next tick.
- **1 h:** status *Degraded performance* to *Operational* with a monitoring note; post "~10 s of brief 5xx during a designed Redis failover; no data loss; no action needed".
- **24 h:** postmortem per [§6](../../sev-playbook.md#6-after-the-incident).

## Pass criteria

1. Classified SEV-2 (degraded), not SEV-1.
2. Confirmed failover completion via `/v1/readyz`, not metrics alone.
3. Treated post-failover `flags.frozen` as expected behaviour.
4. Verified the API-key validator path still serves.
5. Checked `SENTINEL get-master-addr-by-name`, not just the dashboard.
6. Did not fail back to `cache-01` (ADR-0024 "let Sentinel pick").
7. Status page severity *Degraded performance*.
8. Customer comms free of alarmist language.

## Variants

Both replicas down (no quorum; manual recovery; escalates to SEV-1 mid-drill); Sentinel split-brain (partition between Sentinel hosts; ADR-0024 three-host quorum=2 rationale).
