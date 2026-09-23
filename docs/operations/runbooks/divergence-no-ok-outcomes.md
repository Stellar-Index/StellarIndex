---
title: Runbook — divergence-no-ok-outcomes
last_verified: 2026-09-23
status: living
severity: P3
---

# Runbook — `stellarindex_divergence_no_ok_outcomes`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_divergence_no_ok_outcomes` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/divergence.yml` (+ R1 overlay) |
| Typical MTTR | 15–60 min |
| Impact | No pair has had a healthy divergence comparison for 45+ min, so no `div:<asset>` entry has been written and every cached one has expired (5-min TTL). `flags.divergence_warning` and `divergence_checked` read false fleet-wide, and the cross-oracle confidence lens has nothing to use. Aggregate prices keep serving. |

## Why this exists (vs the two sibling alerts)

`stellarindex_divergence_refresh_error_dominant` and
`stellarindex_divergence_no_reference` each compare one failure outcome
against `ok`. A pass that produces only `no_vwap` (every pair frozen,
`min_usd_volume` set above real volume, or the VWAP cache writer
regressed), or no outcome at all (the pass never completes a pair),
satisfies neither. This alert fires on the absence of `ok` itself, and
only while `stellarindex_divergence_refresher_wired` is 1, so an
operator who disabled every reference is not paged.

## Symptoms

- `increase(stellarindex_divergence_refresh_total{outcome="ok"}[30m])`
  is 0 while `stellarindex_divergence_refresher_wired` is 1.
- `/v1/price` responses carry `divergence_checked: false` for every asset.

## Quick diagnosis (≤ 5 min)

```sh
# 1) Which outcome IS the pass producing?
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_divergence_(refresh_total|refresher_wired)'

# 2) Are the pairs frozen?
curl -fs http://localhost:9465/metrics | grep '^stellarindex_anomaly_freeze_active'

# 3) Did the pass panic?
journalctl -u stellarindex-aggregator --since '-1h' | grep -E 'divergence refresh (panicked|: no vwap)' | tail
```

## Most likely causes

1. **Every pair frozen** (ADR-0019) — `no_vwap` climbs at full rate. The
   freeze is the incident; this alert is the divergence-side consequence.
2. **No VWAP in cache** — `min_usd_volume` raised past real volume, or the
   VWAP writer regressed; `no_vwap` climbs with no freeze engaged.
3. **The pass dies before counting** — every outcome flat; look for
   `divergence refresh panicked` in the aggregator log.

## Mitigation (≤ 60 min)

- [ ] Frozen pairs: follow the freeze runbook; divergence recovers once a
      fresh VWAP is cached.
- [ ] Missing VWAP: restore the `[aggregate]` volume floor or fix the
      writer, then confirm `stellarindex_aggregator_vwap_writes_total`
      climbs.
- [ ] Panic: capture the stack from the log and restart the aggregator.
- [ ] Verify `ok` climbs again; the alert resolves on the next evaluation.

## Known false-positive patterns

- **`divergence_min_interval_seconds` above 30 min** — the pass then runs
  less often than the 30-min window, so `ok` can read 0 between passes.
  The default (300 s) never trips this.

## Related

- [`divergence-refresh-error-dominant.md`](divergence-refresh-error-dominant.md) — references erroring.
- [`divergence-no-reference.md`](divergence-no-reference.md) — references dark.
- `internal/aggregate/orchestrator/divergence_refresh.go`.

## Changelog

- 2026-09-23 — initial version alongside the liveness alert.
