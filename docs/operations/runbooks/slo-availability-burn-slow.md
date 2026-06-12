---
title: Runbook — slo-availability-burn-slow
last_verified: 2026-05-12
status: draft
severity: P3
---

# Runbook — `stellaratlas_slo_availability_burn_slow`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellaratlas_slo_availability_burn_slow` |
| Severity | P3 (informational) |
| Detected by | `deploy/monitoring/rules/slo.yml` |
| Typical MTTR | days |
| Impact | Availability budget burning at 1× rate (Google SRE workbook ch. 5 multi-window). At this trajectory a month's budget is gone in ~3 days. No customer impact yet, but a structural regression is in flight that will eventually bite. |

## Symptoms

Multi-window detection: 6-hour 5xx burn AND 3-day 5xx burn both ≥ 1× the budget.

## Investigation

This is the earliest signal in the availability burn-rate family. Treat as a planning ticket, not an incident:

1. Sample the 5xx pattern across the last 7 days — is it spread across all routes (systemic) or concentrated (single endpoint regression)?
2. Cross-reference with deploy history (`git log --grep 'release:' --since '14 days ago'`) to find the inflection point.
3. File a planning ticket with the trend graphs + dominant 5xx route + suspected commit range.

## Known false-positive patterns

- **Long-tail external dependency failures** — if a small fraction of requests are failing because an external poller (CoinGecko, ECB) is having structural availability issues, those don't necessarily count as our 5xx — check whether the failing path is `/v1/sources` or another aggregator-feeding surface.

## Related

- `slo-availability-burn-medium.md` — next escalation.
- `slo-availability-burn-fast.md` — the **P1** at the end of the chain.
- ADR-0008 — HA topology + availability target.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
