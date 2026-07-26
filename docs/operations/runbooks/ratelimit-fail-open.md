---
title: Runbook — ratelimit-fail-open
last_verified: 2026-07-26
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

`internal/api/v1/middleware/ratelimit.go` deliberately **fails open**:
if the Redis token-bucket read/write errors, the request is allowed
through rather than 500'd or 429'd. That is the right trade — a Redis
outage should not take the whole public API down — but it means the
abuse control silently switches itself off, and the only trace is
`stellarindex_ratelimit_fail_open_total` (incremented at both the
per-key and the per-IP gates).

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
   - **AUTH**: `storage.redis_url` password rotated on one side only.
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

- **Redis down** → follow the Redis recovery path; the limiter
  self-heals on the first successful command and the alert clears
  within ~10 min of the last bypass.
- **AUTH drift** → re-sync the password into `/etc/stellarindex.toml`
  (`storage.redis_url`) and `systemctl restart stellarindex-api`.
- **Sustained abuse while open** → the limiter cannot help. Apply the
  block at the edge (Caddy/HAProxy) for the offending source, per
  [api-latency](api-latency.md)'s traffic-shedding section, until
  Redis is back.

## Do NOT

- **Do not "fix" this by failing closed.** Returning 429/500 on a Redis
  error converts a Redis outage into a full API outage. The fail-open
  behaviour is deliberate; this alert exists so it is *observed*, not so
  it is removed.
- Do not silence the alert while Redis is down "because we know" — the
  10-minute `for:` already absorbs failovers, so a firing instance means
  a genuinely sustained unprotected window.

## Related

- [api-down](api-down.md) — the failing-closed outcome we are avoiding.
- [metrics-registry-absent](metrics-registry-absent.md) — the sibling
  class: a metric whose *producer* is missing rather than its consumer.
