---
title: Runbook — sla-probe-p99-breach
last_verified: 2026-09-23
status: current
severity: P2
---

# Runbook — `stellarindex_sla_probe_p99_breach`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sla_probe_p99_breach` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/sla-probe.yml` (group `stellarindex.sla_probe`, `for: 30m`) — the file r1 loads; multi-host twin in `deploy/monitoring/rules/sla-probe.yml` (same expr/for/labels). |
| Typical MTTR | 15–60 min |
| Impact | The published tail-latency target (`/sla`: p99 ≤ 500 ms) is breached on the named endpoint. |

## Symptoms

- `stellarindex_sla_probe_latency_ms{endpoint=…,quantile="0.99"} > 500`
  for ≥ 30 min of consecutive probe runs.
- With p95 healthy, the tail is usually one slow code path or one
  heavy pair on the endpoint, not a saturated backend.
- `stellarindex_api_latency_p99_high` (real traffic, > 2 s, ticket)
  may or may not follow: it is a fleet-wide backstop, not the SLA line.

## Quick diagnosis (≤ 5 min)

Follow `sla-probe-p95-breach.md` "Quick diagnosis" with the 0.99
quantile in step 2:

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.99, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~"stellarindex[_-]api"}[5m])))' | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head
```

## Mitigation (≤ 15 min)

- [ ] Step 1 — Confirm the breach is real (real-traffic p99 on the
      same route agrees), not a probe-host artefact.
- [ ] Step 2 — Route to `api-latency.md` for the latency-triage flow.
- [ ] Verification: probe p99 back under 500 ms for 30 min.

## Known false-positive patterns

- **Cold caches after a deploy** — the 30 m `for` absorbs one run.
- **Probe host CPU contention** — confirm on the host's own metrics.

## Related

- `sla-probe-p95-breach.md` — the p95 twin; same diagnostics.
- `api-latency.md` — the underlying latency-triage flow.
- `sla-probe-unit-failed.md` — the umbrella verdict alert.

## Changelog

- 2026-09-23 — initial version, alongside the rule. The probe exported
  p99 for months with no rule selecting it (#741).
