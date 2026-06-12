---
title: Runbook — anomaly-freeze-sustained
last_verified: 2026-05-12
status: draft
severity: P2
---

# Runbook — `stellaratlas_anomaly_freeze_sustained`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellaratlas_anomaly_freeze_sustained` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/anomaly.yml` |
| Typical MTTR | 30–90 min |
| Impact | A class of pairs has been frozen by anomaly detection for an extended window; the API serves the LKG (last-known-good) value with `flags.frozen=true` instead of the live bucket. Sustained freeze means either real market distress is persisting or our anomaly thresholds are too tight. |

## Symptoms

- `rate(stellaratlas_anomaly_freeze_engaged_total[1h]) > 0` consistently for ≥ 1 hour.
- `/v1/price` responses for one or more `(asset, quote)` pairs carry `flags.frozen=true` AND `flags.single_source=true` for the duration.
- Customer-side: prices appear stuck for affected pairs.

## Quick diagnosis (≤ 5 min)

```sh
# Which asset class is frozen + how often
curl -s http://localhost:9090/api/v1/query?query='sum%20by%20(asset_class)%20(rate(stellaratlas_anomaly_freeze_engaged_total[1h]))'

# Sample the affected pair's flags via the API
curl -s 'http://localhost:3000/v1/price?asset=native&quote=fiat:USD' | jq '.flags'

# Recent freeze events from the durable mirror
sudo -u postgres psql -d stellaratlas -c "SELECT asset_id, quote_id, frozen_at, recovered_at, reason FROM freeze_events WHERE recovered_at IS NULL ORDER BY frozen_at DESC LIMIT 20;"
```

Key signals:
- **Single class** (`crypto`/`stablecoin`/`fiat`) freezes only → likely a per-class anomaly threshold tuned too tight.
- **All classes** freeze → upstream data quality issue (Reflector / Redstone / external venue feed gone bad).
- **Flag clears + re-engages cyclically** → market in genuine distress; the freeze is doing its job.

## Mitigation (≤ 15 min)

- [ ] Step 1 — confirm the freeze is the right call by sampling 3 cross-references (CoinGecko / CoinMarketCap / Reflector). If our LKG matches references within ±2%, the freeze is over-cautious — relax the per-class threshold via the aggregator config and restart.
- [ ] Step 2 — if reference disagree → market really is distressed; let the freeze ride and update the status page (sev-status-page-update.md).
- [ ] Step 3 — if NO reference is available either (e.g. CoinGecko 429) → the divergence worker is the upstream issue; jump to `divergence-refresh-error-dominant.md`.
- [ ] Verification: `freeze_events.recovered_at` populates within 60 s of the underlying anomaly clearing — the `internal/aggregate/freeze.Recovery` worker polls every 60 s, checks whether the Redis marker still exists for each open row, and calls `MarkRecovered` automatically once the TTL elapses. F-1229 (audit-2026-05-12) shipped the worker; operators no longer need to MarkRecovered by hand. If the durable row stays open past the marker TTL, see [freeze-recovery-stalled](freeze-recovery-stalled.md).

## Root cause analysis

Capture for postmortem:
- The full freeze_events history for the affected (asset, quote) pairs over the last 24 h.
- Snapshot of the divergence worker's `our_price` vs `reference_price` for the same window.
- Aggregator log for `class_drop_spike` / `outlier_storm` adjacent alerts.

## Known false-positive patterns

- **Phase-2 confidence-bootstrapping window**: per ADR-0019, the first 30 min after aggregator restart, every pair has a low confidence score because the baseline window hasn't filled. The freeze threshold is intentionally lenient during this window; if it still fires, suspect an upstream data issue, not a real market move.
- **Stablecoin depeg masquerading as anomaly**: ADR-0026 says we late-bind stablecoin → fiat at VWAP compute time. A real depeg looks like an anomaly; check the divergence_warning flag — if it fires, the freeze is correct.

## Related

- `anomaly-freeze-engaged.md` — the per-tick alert; this runbook covers the sustained variant.
- `aggregator-outlier-storm.md` — adjacent symptom when the σ-filter goes wide.
- `divergence-refresh-error-dominant.md` — upstream when references can't be fetched.
- ADR-0019 — anomaly response + confidence scoring.
- F-1228 + F-1229 (audit-2026-05-12) — `freeze_events.frozen_value` always written as 0 + `MarkRecovered` has no caller; tracked separately.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
