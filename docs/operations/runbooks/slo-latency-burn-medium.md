---
title: Runbook — slo-latency-burn-medium
last_verified: 2026-05-12
status: draft
severity: P2
---

# Runbook — `stellarindex_slo_latency_burn_medium`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_latency_burn_medium` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/slo.yml` |
| Typical MTTR | 30–90 min |
| Impact | Latency error budget burning at 6× rate (Google SRE workbook ch. 5 multi-window medium burn). At this trajectory we'd spend a month's budget in ~6 hours. Customer-visible latency is degraded but not catastrophic; act before this escalates to the **P1** fast-burn alert. |

## Symptoms

Multi-window detection: 30-min p95 burn AND 6-hour p95 burn both ≥ 6× the budget.

## Quick diagnosis + Mitigation

Same investigation tree as `slo-latency-burn-fast.md` — the difference is urgency, not cause. At medium burn rate you have time to:

1. Capture a CPU profile + analyse before mitigating.
2. Coordinate with the team via the SEV channel before rolling back.
3. Apply a forward-fix if a recent deploy is the obvious cause.

If the burn rate accelerates and the **fast** alert fires, escalate immediately.

## Root cause analysis

Same as fast-burn — capture p95 trend graphs, recent deploy timestamps, and `pg_stat_statements` snapshots.

## Known false-positive patterns

- **Sustained synthetic load** — the weekly k6 run (`k6-weekly.yml`) is in-band traffic from R1's perspective; if the burn coincides with the cron, suppress.

## Related

- `slo-latency-burn-fast.md` — escalation when the 5-min/1-hour window also crosses 14.4×.
- `slo-latency-burn-slow.md` — earliest signal, longer windows.
- ADR-0009 — API latency budget allocation.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
