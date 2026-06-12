---
title: Runbook — slo-latency-burn-slow
last_verified: 2026-05-12
status: draft
severity: P3
---

# Runbook — `stellarindex_slo_latency_burn_slow`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_latency_burn_slow` |
| Severity | P3 (informational) |
| Detected by | `deploy/monitoring/rules/slo.yml` |
| Typical MTTR | days |
| Impact | Latency error budget burning at the slow rate (Google SRE workbook ch. 5 multi-window). At this trajectory we'd spend a month's budget in ~3 days. No customer impact yet, but a structural regression is in flight that will eventually bite. |

## Symptoms

Multi-window detection: 6-hour p95 burn AND 3-day p95 burn both ≥ 1× the budget rate.

## Investigation

This is the earliest signal in the burn-rate family. Treat as a planning ticket, not an incident:

1. Identify the trend — is it linear (gradual scale problem) or stepped (recent change)?
2. Sample the slow paths from `pg_stat_statements` over the prior 7 days.
3. Cross-reference with the deploy history — `git log --grep 'release:' --since '14 days ago'` — to find the inflection point.
4. File a planning ticket with the trend graphs + dominant slow path + suspected commit range.

Mitigation is usually code-side — refactor the slow path or add a cache layer. Coordinate with the team before any deploy.

## Known false-positive patterns

- **Steady traffic growth** — as customer adoption grows, baseline p95 drifts up linearly. If the trend is correlated with `rate(http_requests_total[7d])` growth, the response is "scale" (more replicas, R2/R3 cutover, paid-tier carve-out), not "fix this code path".

## Related

- `slo-latency-burn-medium.md` — next escalation when the 30-min/6-hour windows also cross 6×.
- `slo-latency-burn-fast.md` — the **P1** at the end of the chain.
- ADR-0009 — API latency budget allocation.
- F-1267 (audit-2026-05-12) — r1 currently runs at p95 = 246 ms structurally.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
