---
title: Runbook — slo-latency-burn-slow
last_verified: 2026-09-19
status: current
severity: P3
---

# Runbook — `stellarindex_slo_latency_burn_slow`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_latency_burn_slow` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`) |
| Typical MTTR | days |
| Impact | Latency budget (99.9 % of `/v1/price`, `/v1/price/batch` + SEP-40 oracle requests under 200 ms over 30 d, per ADR-0009) burning at 1× rate (Google SRE workbook ch. 5 multi-window). At 1× the budget lasts **exactly 30 days — zero slack**: any additional latency incident this month overspends the SLO. No acute customer impact, but a structural regression is in flight. |

## Symptoms

Multi-window detection: the slow-request fraction
(`1 - stellarindex:api_slow_request_ratio:{6h,24h}`, slo
`api_latency_p95_under_200ms`) both **> 1×** the budget (1 × 0.001 = 0.1 %
of requests slower than 200 ms), sustained `for: 30m`.

Note the alert's `runbook_url` annotation points at `api-latency.md`, not
this file — this runbook is the family-specific supplement.

The latency burn rules carry a **min-signal guard**:
`stellarindex:api_slow_request_count:1h > 5` — an absolute count of bad
(slow-or-error) requests over the SLO-scoped routes in the trailing hour,
not a floor on total traffic. (RLT-329, #739): the guard used to compare
TOTAL request rate against a 5 req/s floor sized off a ~2.4 req/s
synthetic-monitoring baseline; once the smoke/prewarm/probe User-Agent
filter excluded that traffic from the histogram, real pre-launch traffic
never cleared 5 req/s and every latency burn alert sat permanently
disarmed regardless of how badly the SLO was burning. The count-based
guard fires on a handful of genuinely bad real requests no matter how few
total requests came with them, while a single cold-cache outlier in a
near-empty window still can't trip it.

## Investigation

This is the earliest signal in the burn-rate family. Treat as a planning ticket, not an incident:

1. Identify the trend — is it linear (gradual scale problem) or stepped (recent change)?
2. Sample the slow paths from `pg_stat_statements` over the prior 7 days
   (`runuser -u postgres -- psql -d stellarindex`).
3. Cross-reference with the deploy history — `git log --grep 'release:' --since '14 days ago'` — to find the inflection point.
4. File a planning ticket with the trend graphs + dominant slow path + suspected commit range.

Mitigation is usually code-side — refactor the slow path or add a cache layer. Coordinate with the team before any deploy.

## Known false-positive patterns

- **Steady traffic growth** — as customer adoption grows, baseline p95 drifts up linearly. If the trend is correlated with `rate(http_requests_total[7d])` growth, the response is "scale" (more replicas, R2/R3 cutover, paid-tier carve-out), not "fix this code path".

## Related

- `slo-latency-burn-medium.md` — next escalation when the 30-min/6-hour windows also cross 6× (**P1** — `severity: page`).
- `slo-latency-burn-fast.md` — the **P1** at the end of the chain.
- `api-latency.md` (the alert's `runbook_url` target).
- ADR-0009 — API latency budget allocation.
- F-1267 (audit-2026-05-12) — r1 currently runs at p95 = 246 ms structurally.

## Changelog

- 2026-09-19 — re-armed the min-signal guard (RLT-329, #739). The
  `> 5 req/s` TOTAL-traffic floor from 2026-07-02 was sized off a
  ~2.4 req/s synthetic-monitoring baseline; once the smoke/prewarm/probe
  User-Agent filter (2026-09-16) excluded that traffic from
  `http_request_duration_seconds` entirely, real pre-launch traffic never
  cleared 5 req/s and this alert was disarmed regardless of burn severity.
  Replaced with `stellarindex:api_slow_request_count:1h > 5`, an absolute
  count of bad (slow-or-error) requests, independent of total traffic
  volume. Dropped the old "if it fired, real traffic exceeds the floor"
  triage note — it no longer applies to a count-based guard.
- 2026-08-29 — re-verified against HEAD: windows are 6h AND 24h (not 3d),
  `for: 30m`, `severity: ticket` (P3 frontmatter was already correct);
  impact arithmetic — 1× burn means the budget lasts exactly 30 days (zero
  slack), not "gone in ~3 days"; rule path → r1 overlay primary;
  min-traffic guard (deliberately cannot fire on quiet r1) + `runbook_url`
  → api-latency.md notes; medium-burn severity in Related corrected to page.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
