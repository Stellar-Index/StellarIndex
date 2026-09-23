---
title: Runbook — sla-probe-availability-breach
last_verified: 2026-09-23
status: current
severity: P2
---

# Runbook — `stellarindex_sla_probe_availability_breach`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sla_probe_availability_breach` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/sla-probe.yml` (group `stellarindex.sla_probe`, `for: 30m`) — the file r1 loads; multi-host twin in `deploy/monitoring/rules/sla-probe.yml` (same expr/for/labels). |
| Typical MTTR | 15–60 min |
| Impact | The published availability target (`/sla`: ≥ 99.9 % of probe requests answered 2xx) is breached on the named endpoint. |

## Symptoms

- `stellarindex_sla_probe_availability_pct{endpoint=…} < 99.9` for
  ≥ 30 min of consecutive probe runs.
- A run holds roughly 150 samples per endpoint, so a single non-2xx
  answer puts the run below 99.9 %. The `for` is what separates a
  sustained breach from one unlucky request.
- Latency and freshness may be healthy while this fires: the probe
  counts every 429, 5xx and client timeout as unavailable.
- `stellarindex_sla_probe_unit_failed_alert` (ticket) follows on the
  same window; it is the umbrella, this alert names the endpoint.

## Quick diagnosis (≤ 5 min)

```sh
# 1. The last run's per-endpoint verdict (the JSON never reaches
#    journald — read the textfile the alert scrapes):
ssh root@136.243.90.96 cat /var/lib/node_exporter/textfile_collector/sla_probe.prom

# 2. What the failures were. A one-off run prints failed_reasons.
#    Keep -concurrency 1 (F-1305 self-saturation) and authenticate —
#    a keyless run hits the anonymous 60/min limit and 429-fails
#    (F-1311), which reads exactly like this alert.
export STELLARINDEX_PROBE_API_KEY=<key>
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -duration 30s -concurrency 1 -report-format json | jq .failed_reasons

# 3. Does real traffic agree? (Prometheus listens on localhost on r1.)
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (status) (rate(http_requests_total{job=~"stellarindex[_-]api"}[15m]))' | \
  jq -r '.data.result[] | "\(.metric.status): \(.value[1])"'
```

## Mitigation (≤ 15 min)

- [ ] Step 1 — Split the failures by status. 429 on the probe only →
      the probe's key is missing, revoked or under-quota; fix the
      key, not the API.
- [ ] Step 2 — 5xx → route to `api-5xx.md`.
- [ ] Step 3 — Timeouts → route to `api-latency.md`.
- [ ] Verification: two consecutive probe runs at ≥ 99.9 % on the
      endpoint (the alert clears on the same 30 m `for`).

## Known false-positive patterns

- **Probe-side 429s** — the probe's API key missing or rate-limited
  (F-1311). The API is healthy; the measurement is not.
- **Deploy restart inside a run** — a handful of connection refusals
  during the swap. One run only; the `for` absorbs it.

## Related

- `sla-probe-p95-breach.md`, `sla-probe-p99-breach.md`,
  `sla-probe-freshness-breach.md` — the other per-target alerts.
- `sla-probe-unit-failed.md` — the umbrella verdict alert.
- `slo-availability-burn-fast.md` — real-traffic availability burn.
- `api-5xx.md` — server-error triage.

## Changelog

- 2026-09-23 — initial version, alongside the rule. The probe exported
  availability for months with no rule selecting it (#741).
