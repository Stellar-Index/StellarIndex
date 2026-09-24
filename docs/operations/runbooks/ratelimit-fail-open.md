---
title: Runbook — ratelimit-fail-open
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_ratelimit_fail_open`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ratelimit_fail_open` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: usually resolves itself the moment Redis is reachable again; the fix is whatever made Redis unreachable. |
| Impact | The API's per-key rate limit is **not being enforced**. Requests are served unlimited. Metering/billing still records usage, so this is a throughput/abuse exposure, not a revenue-loss one. |

## What this fires on

The limiter is a Redis fixed-window counter (one atomic `INCRBY` +
`EXPIRE` per key per minute, `internal/ratelimit`). When that Redis
call errors, `internal/api/v1/middleware/ratelimit.go` **fails open**
for a bounded window: the request is allowed through rather than
500'd or 429'd, and `stellarindex_ratelimit_fail_open_total` is
incremented (at both the per-key and the per-IP gates). A Redis blip
should not take the whole public API down.

The window is bounded. Once a bucket's Redis calls have been failing
for longer than `ratelimit.DefaultDwellTime` (30s), the middleware
fails **closed** with `503` (`errors/throttle-unavailable`,
`Retry-After: 30`) instead, and this counter stops moving. The
closed state clears only after the same dwell time of *unbroken*
Redis successes; a flapping Redis keeps it armed. The anonymous and
authenticated tiers are separate buckets with separate clocks.

So this alert sees the fail-open portion only: repeated short error
episodes, each starting a fresh fail-open window. A hard, sustained
Redis outage shows up as API `503`s and a red Redis readiness check,
**not** as this alert (the counter goes flat about 30s in, and
`for: 10m` never elapses).

The counter has existed since the limiter shipped. Until C6-032
(audit-2026-07-23) **nothing in either rule tree selected it**, so the
limiter could be off for an arbitrary length of time with zero pages —
registered-but-unalerted, the same shape as a dead alert.

The rule fires on `rate(...[5m]) > 0` sustained `for: 10m`, so:

- a one-off blip during a Redis failover does **not** ticket;
- "the limiter has been bypassing requests continuously for ten
  minutes" always does.

## Quick diagnosis (≤ 5 min)

1. **Is Redis up?** `systemctl status redis` on r1, then
   `redis-cli -a "$REDIS_PASSWORD" ping`. Check the API's
   `/readyz` — the redis checker should already be red if the store
   is unreachable.
2. **Does this alert fire ALONE (Redis healthy)?** Then the failure is
   inside the limiter's own path, not the store's availability. Likely
   causes, in order:
   - **AUTH**: Redis password (`STELLARINDEX_REDIS_PASSWORD`) rotated on one side only.
     `journalctl -u stellarindex-api | grep -i 'ratelimit\|redis'` shows
     `NOAUTH` / `WRONGPASS`.
   - **Key namespace / eviction policy**: a `maxmemory-policy` of
     `allkeys-lru` evicting the limiter's counters mid-window produces
     errors on the increment path.
   - **Connection-pool exhaustion**: the API is saturated and the pool
     is timing out (`context deadline exceeded` in the same log line).
     `rate(http_requests_total[1m])` will be at an unusual peak.
3. **How exposed are we?** `sum(rate(stellarindex_ratelimit_fail_open_total[5m]))`
   is the requests/sec currently bypassing. Compare against
   `sum(rate(http_requests_total[5m]))` — if they are equal, *no*
   request is being limited.

## Remediation

- **Redis down** → follow the Redis recovery path. Every Redis call
  that succeeds is limited normally straight away; calls that still
  fail keep answering `503` until Redis has answered without error for
  `DefaultDwellTime`. The alert clears within ~10 min of the last
  bypass.
- **AUTH drift** → the API's Redis password is the
  `STELLARINDEX_REDIS_PASSWORD` environment override (config field
  `[storage] redis_password_env`), not a hand-edited TOML value. Re-sync it
  in the unit's `EnvironmentFile` (`/etc/default/stellarindex`) to Redis's
  `requirepass` (ansible `redis_password`), then
  `systemctl restart stellarindex-api`.
- **Sustained abuse while open** → the limiter cannot help during the
  fail-open windows. Apply the block at the edge (Caddy/HAProxy) for the offending source, per
  [api-latency](api-latency.md)'s traffic-shedding section, until
  Redis is back.

## Do NOT

- **Do not remove the fail-open window** (a zero dwell time). Failing
  closed on the first Redis error converts every Redis blip into an API
  outage. Nor disable the fail-closed switch (a negative
  `WithDwellTime`): an attacker who can degrade Redis would then pivot
  to unlimited request volume.
- Do not silence the alert while Redis is down "because we know" — the
  10-minute `for:` already absorbs failovers, so a firing instance means
  a genuinely sustained unprotected window.

## Related

- [api-down](api-down.md) — where a sustained Redis outage lands once
  the limiter has failed closed.
- [metrics-registry-absent](metrics-registry-absent.md) — the sibling
  class: a metric whose *producer* is missing rather than its consumer.
