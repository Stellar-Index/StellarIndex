---
title: Runbook — slo-availability-burn-slow
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_slo_availability_burn_slow`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_availability_burn_slow` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`) |
| Typical MTTR | days |
| Impact | Availability budget (99.99 % non-5xx over 30 d) burning at 1× rate (Google SRE workbook ch. 5 multi-window). At 1× the budget lasts **exactly 30 days — zero slack**: any additional incident this month overspends the SLA. No acute customer impact, but a structural regression is in flight. |

## Symptoms

Multi-window detection: `stellarindex:api_error_ratio:6h` AND
`stellarindex:api_error_ratio:24h` (slo `api_availability_3_nines_9`) both
**> 1×** the budget (1 × 0.0001 = 0.01 % 5xx), sustained `for: 30m`.

Note the alert's `runbook_url` annotation points at `api-5xx.md`, not this
file — this runbook is the family-specific supplement.

Unlike the latency burn family, the availability burn rules have **no
min-traffic guard** — on quiet r1 a trickle of synthetic 5xx can hold the
ratio above 0.01 % indefinitely. Check the traffic floor before treating the
trend as a real regression:

```sh
curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:24h' | jq .data.result
curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[24h]))' | jq .data.result
```

## Investigation

This is the earliest signal in the availability burn-rate family. Treat as a planning ticket, not an incident:

1. Sample the 5xx pattern across the last 7 days — is it spread across all routes (systemic) or concentrated (single endpoint regression)?

   ```sh
   curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum by (route,status)(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[24h]))' | jq .data.result

   journalctl -u stellarindex-api --since '24 hours ago' --no-pager -o cat \
     | jq -r 'select(.status >= 500) | [.status, .path] | @tsv' | sort | uniq -c | sort -rn | head -20
   ```

2. Cross-reference with deploy history (`git log --grep 'release:' --since '14 days ago'`) to find the inflection point.
3. File a planning ticket with the trend graphs + dominant 5xx route + suspected commit range.

## Known false-positive patterns

- **Synthetic probes at low traffic** — `stellarindex-sla-probe.timer` and `stellarindex-smoke.timer` (`configs/healthchecks/`) plus cache prewarm hit the local API; with no min-traffic guard, at near-zero real traffic a few probe 5xx exceed 0.01 %. Check the total request rate first.
- **Long-tail external dependency failures** — if a small fraction of requests are failing because an external poller (CoinGecko, ECB) is having structural availability issues, those don't necessarily count as our 5xx — check whether the failing path is `/v1/sources` or another aggregator-feeding surface.

## Related

- `slo-availability-burn-medium.md` — next escalation (30m + 6h windows at 6×, `severity: page`).
- `slo-availability-burn-fast.md` — the **P1** at the end of the chain.
- `api-5xx.md` (the alert's `runbook_url` target).
- ADR-0008 — HA topology + availability target (multi-region decision amended by ADR-0050 / `docs/architecture/multi-region-ha.md`).

## Changelog

- 2026-08-29 — re-verified against HEAD: windows are 6h AND 24h (not 3d),
  `for: 30m`, `severity: ticket`; impact arithmetic — 1× burn means the
  budget lasts exactly 30 days (zero slack), not "gone in ~3 days"; rule
  path → r1 overlay primary; `runbook_url` → api-5xx.md note;
  no-min-traffic-guard note + synthetic-probe false positive; self-contained
  r1 diagnosis commands (`-o cat`).
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
