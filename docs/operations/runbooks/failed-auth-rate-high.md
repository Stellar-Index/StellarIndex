---
title: Runbook — failed-auth-rate-high
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_failed_auth_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_failed_auth_rate_high` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 15–60 min to identify the sources. |
| Impact | **Someone may be guessing credentials.** No request has been let through: the Auth middleware rejected every one of them. |

## Why this exists

The Auth middleware's failed-auth throttle caps credential failures per
client IP and, for API keys, per presented key prefix, at
`api.failed_auth_rate_limit_per_min` (20/min by default). Guessing spread
across enough IPs and prefixes stays under every cap and never gets a 429,
so the throttle alone makes it invisible. This alert watches
`stellarindex_failed_auth_total{outcome="rejected"}` summed across the
fleet. One source can contribute at most 20/min to that series, so a
sustained 1/s means more than three sources at their cap or many sources
under it.

## Quick diagnosis (≤ 5 min)

The rejected-versus-throttled split:

```promql
sum by (outcome) (rate(stellarindex_failed_auth_total[5m]))
```

Then group the API access log's 401/403 lines by `remote_ip`:

```sh
journalctl -u stellarindex-api --since '30 min ago' --no-pager -o cat \
  | jq -r 'select(.status == 401 or .status == 403) | .remote_ip' \
  | sort | uniq -c | sort -rn | head -20
```

- **A few IPs, each near 20/min**: a handful of misconfigured clients
  (a revoked or rotated key still deployed) or a small guessing run.
  Both are already capped.
- **Many IPs, each well under 20/min**: distributed guessing. This is
  the case the alert exists for.

## Mitigation (≤ 15 min)

- [ ] If a few IPs belong to a known customer, contact them about the
      stale key; no platform action is needed.
- [ ] For distributed guessing, block the source ranges at the edge
      proxy. API keys carry a 224-bit secret after their display
      prefix, so guessing does not threaten a key directly; the cost is
      validator load.
- [ ] Verification: the rejected rate falls below 1/s and the alert
      clears 15 minutes later.

## Do NOT

- **Do not lower `api.failed_auth_rate_limit_per_min` to zero to
  silence the alert.** Zero disables the throttle.
- **Do not treat a burst of `throttled` alone as this alert.** Throttled
  attempts are already capped per source and are excluded from the
  expression.

## Related

- [ratelimit-fail-open](ratelimit-fail-open.md) — the failed-auth
  throttle shares the rate limiter's Redis. During a sustained Redis
  outage it fails closed, so the `throttled` series climbs for every
  failed request.
