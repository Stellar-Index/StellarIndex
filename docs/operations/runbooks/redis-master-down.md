---
title: Runbook — redis-master-down
last_verified: 2026-08-29
status: current (r1 is single-node Redis — Sentinel topology is the future multi-host shape)
severity: P1
---

# Runbook — `stellarindex_redis_master_down`

> **r1 deployment posture (re-verified 2026-08-29).** On r1 there is
> ONE Debian-packaged `redis-server` running on the archival host —
> no Sentinel, no replicas, no `cache-01..03` hosts. The
> `redis-sentinel` ansible role exists in the repo but is NOT
> applied to archival nodes (see the comment in
> `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`
> — "the redis-sentinel role is the future HA-cluster shape, NOT
> applied to archival nodes"). `redis_up == 0` therefore means THAT
> instance is down; there is nothing to fail over to. Diagnose:
>
> ```sh
> ssh root@136.243.90.96 "systemctl status redis-server --no-pager | head -15"
> ssh root@136.243.90.96 "journalctl -u redis-server -n 100 --no-pager"
> ```
>
> Restart: `ssh root@136.243.90.96 systemctl restart redis-server`.
> The Sentinel sections below describe the FUTURE multi-host shape
> (ADR-0024 / ha-plan §3.4) and are kept for that rollout.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_redis_master_down` |
| Severity | P1 (`severity: page` — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/cache.yml` (group `stellarindex.cache`, `severity: page`, `for: 30s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/cache.yml`. |
| Typical MTTR | 1–15 min (Sentinel-driven failover: < 1 min; manual: longer) |
| Impact | Hot-path cache for `/v1/price` gone. Rate-limiter fails open (no throttling). Clients still get served via Timescale fallback with `stale=true` and increased latency — so not an outage, but a **degraded SLA** + **no rate-limiting** (fail-open abuse window). |

## Symptoms

- `redis_up == 0` for ≥ 30 s. (The pre-F-1329 expr
  `redis_up{role="master"} == 0` was DEAD — `redis_exporter` emits
  no `role` label, so it matched no series; the rule now watches
  bare `redis_up`, the exporter's could-I-reach-the-server signal.)
- API latency rises (cache miss → Timescale path).
- `stellarindex_ratelimit_fail_open_total` counter jumps — this
  metric is the deliberate "Redis outage" signal (fail-open by
  design per HA plan §3.4).
- API logs: "redis get: connection refused" or
  "cache miss: pool exhausted".

## Quick diagnosis (≤ 5 min)

**On r1:** single instance — the two commands in the banner above
are the whole diagnosis. Everything below in this section is the
**future multi-host shape** (not deployed anywhere today).

In the multi-host topology Redis runs as `redis-server.service` +
`redis-sentinel.service` on three bare-metal hosts `cache-01..03`
(per the `redis-sentinel` ansible role; ADR-0024 / ADR-0008 §3.4).
Per-host primary role is in the inventory's `redis_role` var;
current role at runtime comes from Sentinel.

```sh
# Is it a single instance or the whole shard?
for h in cache-01 cache-02 cache-03; do
  echo -n "$h: "
  redis-cli -h $h -a "$REDIS_PASSWORD" ping
done

# Sentinel's view of the world (any one cache host serves)
redis-cli -h cache-01 -p 26379 -a "$REDIS_PASSWORD" \
  SENTINEL masters

# Is the host up but redis-server process dead?
ssh root@cache-01 "systemctl status redis-server --no-pager | head -15"
ssh root@cache-01 "journalctl -u redis-server -n 100 --no-pager"

# Sentinel itself running?
ssh root@cache-01 "systemctl status redis-sentinel --no-pager | head -10"
```

## Typical root causes

1. **Sentinel mid-failover.** Redis Sentinel promotes a replica on
   master failure. Detection is fast (< 30 s) but the alert `for:
   30s` means we sometimes page right as Sentinel is resolving it.
   Wait one poll interval; if Sentinel's `sentinel masters` shows
   a new master, the alert will clear.

2. **OOMKilled on the master host.** Redis's `maxmemory` setting
   is independent of the kernel's view — if the host memory is
   under pressure (noisy neighbor, something else leaking), the
   kernel OOM-killer takes Redis.

3. **Persistence write stalled the primary.** AOF rewrite or an
   RDB save on a large dataset blocks `fork()` — seems to clients
   like the master is down because responses stall.
   - Signal: `redis-cli info persistence` shows `rdb_last_bgsave_status:err`
     or a running `aof_rewrite` that's been going for > 60 s.

4. **Network partition** between the API pods and the master. If
   the master is alive but unreachable from the API, Sentinel sees
   it and fails over. `up{role="master"}` from Prometheus's POV
   is 0 if Prometheus is in the same partitioned zone as the API.

## Mitigation

### A. Automatic Sentinel failover — the happy path (multi-host only)

In the future multi-host topology (the `redis-sentinel` role
applied to dedicated cache hosts — NOT r1), this is the
**default** path. Sentinel's `down-after-milliseconds=5000` +
`failover-timeout=60000` mean a primary failure typically
recovers in 15–30 s without operator intervention. The
`go-redis/v9` `FailoverClient` clients re-discover the new
primary automatically — no app restart required.

- [ ] Step 1 — check Sentinel's view first. If failover is in
      progress, hold — it should complete in 15–30 s.
      `redis-cli -h cache-01 -p 26379 -a "$REDIS_PASSWORD" \
       SENTINEL get-master-addr-by-name stellarindex-r1-cache`
      returns the current primary; `stellarindex_redis_sentinel_primary`
      gauge (multi-host only; absent on r1) sums to 1 across hosts
      when steady-state.
- [ ] Step 2 — verify clients reconnected. API + aggregator logs
      should show `redis configured mode=sentinel` at startup;
      after failover, look for "redis: reconnected" or absence
      of "connection refused". `ratelimit_fail_open_total` rate
      drops back to zero.

### B. Stuck or split-brain — manual failover

Resort to this **only** if Sentinel's automatic failover hasn't
completed within 60 s OR `SENTINEL ckquorum` reports < 2 alive
sentinels.

- [ ] Step 1 — confirm the stuck state:
      `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL ckquorum stellarindex-r1-cache`
      and `SENTINEL master stellarindex-r1-cache` (look for
      `last-ok-ping-reply` > 10000 ms or `flags` containing
      `s_down,o_down`).
- [ ] Step 2 — force a promotion:
      `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL failover stellarindex-r1-cache`.
      Do this with a clear head; forcing failover on a transient
      network blip can split-brain if Sentinels rejoin and
      disagree on who's primary.
- [ ] Step 3 — if the primary host itself is gone: nothing to do
      from the cache cluster's side (Sentinel already promoted).
      Hand off to host-bringup runbook to restore the failed node
      as a fresh replica via `ansible-playbook --tags redis --limit cache-X`.
- [ ] Step 4 — verify the promoted replica is caught up:
      `redis-cli info replication` on the new primary should
      show every follower's `lag` column at 0–1.
- [ ] Verification: `redis_up == 1` on every instance,
      `stellarindex_redis_sentinel_primary` (multi-host only;
      absent on r1) sums to 1
      across hosts, API logs show "redis: reconnected",
      `ratelimit_fail_open_total` rate drops to zero (it's
      cumulative — watch the rate, not the gauge).

## Data loss considerations

- `/v1/price` hot-cache entries are re-derivable from Timescale on
  next request. Zero data loss risk there.
- Rate-limit counters are stored in Redis with ~1 min TTL. A
  failover resets them to zero; clients who were throttled get a
  fresh quota. Acceptable.
- API keys / SEP-10 session tokens must not live only in Redis —
  they are backed by Timescale (see `internal/auth/` +
  `internal/platform/`). A Redis outage costs cache warmth, not
  credentials.

## Root cause analysis

- Sentinel log — ordered events: `+sdown`, `+odown`, `+new-epoch`,
  `+switch-master`.
- Redis log from both old and new masters around the event.
- Host-level: OOM log (`dmesg | grep -i oom`), load avg, network.
- Was a rolling restart in progress? If so, was the rollout policy
  respecting the Sentinel quorum?

## Known false-positive patterns

- **Rolling restart of redis-server across the cluster** (e.g.
  apt upgrade rolled out via the `redis-sentinel` ansible role
  with `--limit`): rolling the current primary always trips this
  alert for ~30 s while Sentinel promotes a replica. Muting the
  alert for the duration of a planned maintenance is acceptable;
  the role's README documents the safe one-host-at-a-time apply
  pattern.
- **Prometheus-exporter crash (not Redis crash)**: `redis_up`
  comes from `redis_exporter`. If the exporter sidecar died but
  Redis is fine, we page on a phantom outage. Check the exporter's
  own health before acting.

## Related

- `redis-memory.md` — OOM / eviction issues.
- `redis-replication.md` — replicas not following.
- HA plan §3.4: `docs/architecture/ha-plan.md` (Redis topology,
  fail-open rationale).
- ADR-0024 — `docs/adr/0024-redis-ha-via-sentinel.md` (the future
  Sentinel topology).
- ADR-0007 (key schema) — `docs/adr/0007-redis-cache-schema.md`.

## Changelog

- 2026-04-23 — initial draft, called out the fail-open behaviour
  as a deliberate design choice (not a bug to fix).
- 2026-05-02 — diagnosis converted from kubectl/StatefulSet
  commands to the `cache-01..03` bare-metal hosts +
  `redis-server.service` / `redis-sentinel.service` shape that
  the `redis-sentinel` ansible role actually deploys (ADR-0008
  §3.4).
- 2026-08-29 — re-verified against HEAD: r1-reality banner (one
  Debian-packaged redis-server, no Sentinel — the redis-sentinel
  role is NOT applied to archival nodes), dead pre-F-1329 expr
  `redis_up{role="master"}` replaced with the real `redis_up == 0`
  rule, Sentinel sections marked as the future multi-host shape,
  `stellarindex_redis_sentinel_primary` marked multi-host-only,
  auth-token "when they land" hedge dropped (they landed),
  dual-tree Detected-by, ADR-0024 cited. Status "ratified" →
  current.
