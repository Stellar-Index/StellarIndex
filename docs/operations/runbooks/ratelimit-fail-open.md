---
title: Runbook — ratelimit-fail-open
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_ratelimit_fail_open` / `stellarindex_ratelimit_fail_closed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ratelimit_fail_open` (P3 / ticket) — bypassing unlimited. `stellarindex_ratelimit_fail_closed` (page) — the companion alert below: past the same dwell time, the limiter serves 503s instead. |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: usually resolves itself the moment Redis is reachable again; the fix is whatever made Redis unreachable. |
| Impact | `_fail_open`: the API's per-key rate limit is **not being enforced**; requests are served unlimited. Metering/billing still records usage, so this is a throughput/abuse exposure, not a revenue-loss one. `_fail_closed`: every request hitting the affected bucket gets a 503 — outright customer-visible API unavailability, not a narrowed throttling effect (2026-09-16: 2.03M 503s over 2h53m). |

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

So `stellarindex_ratelimit_fail_open` sees the fail-open portion only:
repeated short error episodes, each starting a fresh fail-open window.
A hard, sustained Redis outage shows up as `stellarindex_ratelimit_fail_closed`
(below) and a red Redis readiness check, not as continued fail-open —
the counter goes flat about 30s in.

The counter has existed since the limiter shipped. Until C6-032
(audit-2026-07-23) **nothing in either rule tree selected it**, so the
limiter could be off for an arbitrary length of time with zero pages —
registered-but-unalerted, the same shape as a dead alert.

**2026-09-27 fix:** the original rule fired on `rate(...[5m]) > 0`
sustained `for: 10m` — which could **never** fire, for the exact reason
the previous paragraph explains: the fail-open window is capped at the
30s dwell time, so the rate flattens out long before a 10-minute
sustained condition could mature. It shipped dead and stayed dead
through the 09-16 incident. The rule now counts instead of rating:
`sum(increase(stellarindex_ratelimit_fail_open_total[15m])) > 100`,
`for: 0m` — more than 100 bypassed requests in a 15-minute window,
regardless of how the dwell time chops up the underlying rate.

## `stellarindex_ratelimit_fail_closed`

Past the same dwell time (`ratelimit.DefaultDwellTime`, 30s) of
*continuous* Redis errors on a bucket, the limiter stops failing open
and fails **closed** instead: every request in that bucket gets a 503
(`errors/throttle-unavailable`, `Retry-After: 30`,
`writeThrottleUnavailableProblem`) rather than being served. This is
the shape a hard, sustained Redis outage actually takes — not
continued fail-open. It clears once Redis has answered without error
for the same dwell time; a flapping Redis keeps it armed.

`expr: sum(rate(stellarindex_ratelimit_fail_closed_total[5m])) > 0`,
`for: 2m`, `severity: page` — 2 minutes, not the fail-open rule's
window, because a fail-closed state is an ongoing customer-visible
outage from the first second, not a tolerable blip. The 2026-09-16
incident held this state for 2h53m and served 2.03M 503s before it was
caught — the gap this alert closes.

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
