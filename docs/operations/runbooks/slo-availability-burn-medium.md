---
title: Runbook — slo-availability-burn-medium
last_verified: 2026-05-12
status: draft
severity: P2
---

# Runbook — `stellarindex_slo_availability_burn_medium`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_availability_burn_medium` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/slo.yml` |
| Typical MTTR | 30–90 min |
| Impact | Availability budget burning at 6× rate. A month's budget gone in ~6 hours at this trajectory. Some customers see 5xx; act before the **P1** fast-burn alert fires. |

## Symptoms

Multi-window detection: 30-min 5xx burn AND 6-hour 5xx burn both ≥ 6× the budget.

## Investigation + Mitigation

Same triage tree as `slo-availability-burn-fast.md` — the difference is urgency. At medium burn rate you have time to:

1. Inspect the actual 5xx errors before mitigating (the diagnosis-step `jq` query in fast-burn).
2. Coordinate via the SEV channel.
3. Apply a forward-fix if a recent deploy is the obvious cause.

If the 5-min/1-hour burn windows ALSO cross 14.4× → escalate to **P1** immediately.

## Known false-positive patterns

- **Sustained synthetic load** — `k6-weekly.yml` is in-band traffic and intentionally pushes 5xx on overload paths.
- **Customer behaviour** — a single misbehaving authenticated client retrying a 4xx-able malformed request can occasionally shift to 5xx if the route's input validation has bugs; check rate-limit headers on the failing requests.

## Related

- `slo-availability-burn-fast.md`, `slo-availability-burn-slow.md` — same family.
- `api-5xx.md` / `api-latency.md` — adjacent route-level alerts.
- ADR-0008 — HA topology + availability target.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
