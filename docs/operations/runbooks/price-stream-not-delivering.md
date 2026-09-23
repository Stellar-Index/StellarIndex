---
title: Runbook — price-stream-not-delivering
last_verified: 2026-09-23
status: current
severity: P3
---

# Runbook — `stellarindex_api_price_stream_not_delivering`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_price_stream_not_delivering` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/api.yml` (`for: 15m`); multi-host twin in `deploy/monitoring/rules/api.yml`. |
| Typical MTTR | 15–60 min |
| Impact | `/v1/price/stream` answers 200 and sends keepalives, but no `price_update` events. |

## Symptoms

- `stellarindex_aggregator_stream_publish_total{outcome="ok"}` is rising
  while `stellarindex_api_stream_subscribe_total{outcome="ok"}` is flat
  (or absent) for 15 min.

## Quick diagnosis (≤ 5 min)

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (outcome) (increase(stellarindex_api_stream_subscribe_total[15m]))' | \
  jq -r '.data.result[] | "\(.metric.outcome): \(.value[1])"'
```

- `future_observed_at` rising — the API host's clock lags the
  aggregator's by more than 5 min; every event is rejected as
  future-dated. Check `timedatectl` / chrony on both hosts.
- `stale_observed_at` rising — events are more than 24 h old: a
  replay, or the aggregator's clock is far behind.
- `malformed` / `decode_error` rising — wire-format drift between the
  aggregator's publisher and the API's subscriber (a version skew).
- Nothing rising at all — the API receives no messages. go-redis
  retries a dead pubsub connection internally with no app-level log;
  check `redis-cli PUBSUB NUMSUB stellarindex:closed-bucket:v1` and the API's
  stderr for go-redis reconnect lines.

## Mitigation (≤ 15 min)

- [ ] Step 1 — Clock skew: resync NTP on the lagging host.
- [ ] Step 2 — No messages: confirm Redis is reachable from the API
      host, then restart the API to force a fresh SUBSCRIBE.
- [ ] Verification: `outcome="ok"` rising again for 15 min.

## Known false-positive patterns

- **API restart in the window** — the counter resets; `rate()` handles
  it, and one scrape gap does not hold for 15 min.

## Related

- `aggregator-silent.md` — when the aggregator is not publishing at all
  (this alert cannot fire then).
- `api-5xx.md` — general API triage.

## Changelog

- 2026-09-23 — initial version, alongside the rule (#753).
