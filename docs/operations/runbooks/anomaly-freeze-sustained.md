---
title: Runbook — anomaly-freeze-sustained
last_verified: 2026-08-28
status: draft
severity: P1
---

# Runbook — `stellarindex_anomaly_freeze_sustained`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_anomaly_freeze_sustained` (companion: `stellarindex_anomaly_freeze_escalated`) |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/anomaly.yml` (what r1 loads; `deploy/monitoring/rules/anomaly.yml` is the mirror). The same escalation is also paged by `stellarindex_anomaly_freeze_escalated` in `configs/prometheus/rules.r1/freeze-lifecycle.yml`, whose `runbook_url` points here. |
| Typical MTTR | 30–90 min |
| Impact | At least one `(asset, quote, window)` freeze has EXHAUSTED the ADR-0019 extension ladder (initial hold + 4 × 30 min extensions) and is now held operator-only: it will NOT auto-unfreeze. The API serves the LKG (last-known-good) value with `flags.frozen=true` for that pair until a human lifts it. Either real market distress persisted for > 2 h, the pair has no corroborating reference and so can never auto-release, or the Phase-2 thresholds are too tight. |

## Symptoms

- Alert expr (reshaped 2026-08-06): `sum by (class) (increase(stellarindex_anomaly_freeze_escalated_total[1h])) > 0`, `for: 5m`. Engage → extend → auto-release cycling that resolves itself does NOT page (that is the system working); only escalation does.
- NOTE: `stellarindex_anomaly_freeze_escalated_total` is an UNLABELLED counter (`internal/obs/metrics.go`), so the alert's `class` label is empty. The alert tells you *that* a freeze escalated, not *which pair* — find the pair with the diagnosis commands below.
- `stellarindex_anomaly_freeze_active > 0` holds steady rather than cycling.
- `/v1/price` responses for the affected `(asset, quote)` pair carry `flags.frozen=true` (typically also `flags.single_source=true`) for the duration.
- `freeze_events` has an open row with `escalated = true` and `recovered_at IS NULL`.
- Customer-side: the price appears stuck for the affected pair.

## Quick diagnosis (≤ 5 min)

```sh
# 1. WHICH pair escalated — the alert's class label is empty. Start here.
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list

# Same fact from the durable mirror (ladder columns landed in migration 0119)
sudo -u postgres psql -d stellarindex -c "SELECT asset_id, quote_id, frozen_at, reason, extensions_used, escalated, hold_until FROM freeze_events WHERE recovered_at IS NULL ORDER BY frozen_at DESC;"

# 2. Context: which asset class is engaging + how often (engaged_total IS
# labelled by class; escalated_total is not). The label is `class`, NOT
# `asset_class` — grouping by the wrong name collapses every series into
# one with an empty label, which is what this command did until 2026-08-04.
curl -s http://localhost:9090/api/v1/query?query='sum%20by%20(class)%20(rate(stellarindex_anomaly_freeze_engaged_total[1h]))'

# 3. Sample the affected pair's flags via the API
curl -s 'http://localhost:3000/v1/price?asset=<asset>&quote=<quote>' | jq '.flags'

# 4. Marker state: an escalated pair's TTL keeps sliding (remaining hold + 5 min grace)
redis-cli KEYS 'freeze:*'
redis-cli TTL 'freeze:<asset>:<quote>'
```

Key signals:
- **Single class** (`stablecoin` / `treasury` / `crypto` / `governance` / `default`) freezes only → likely a per-class threshold tuned too tight. `default` means the asset is unclassified — or the Phase-1 checker is disabled (`[anomaly] enabled=false`), in which case `classOf()` emits `default` for everything.
- **All classes** freeze → upstream data-quality issue: a divergence lens (coingecko / reflector / chainlink / synthetic cross — `internal/divergence`) or a CEX/DEX trade source has gone bad.
- **Flag clears + re-engages cyclically** → market in genuine distress; the freeze is doing its job. (This pattern does not page by itself — if you are here, something ALSO escalated.)

## Mitigation (≤ 15 min)

- [ ] Step 1 — confirm the freeze is the right call by sampling the cross-references we actually have (`GET /v1/divergence`, CoinGecko directly, Reflector, Chainlink). If our LKG matches references within ±2%, the freeze is over-cautious. On r1 the firing layer is Phase 2: tune `[anomaly.phase2]` (`z_score_min_freeze`, `confidence_max_freeze`, `source_count_max_freeze`; ansible var `stellarindex_phase2_z_min_freeze`) via `configs/ansible` in the same PR, then `sudo systemctl restart stellarindex-aggregator` — there is no SIGHUP reload. The Phase-1 `[anomaly.thresholds.<class>]` table only applies when `[anomaly] enabled = true`, which the r1 template does not set. Then, if the frozen level is genuine, lift the freeze (default is DRY-RUN; `-write` applies):
  `stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -asset <A> -quote <Q> -reason "..." -write`
- [ ] Step 2 — if references disagree → market really is distressed. An escalated freeze will not self-clear, so decide explicitly: keep it held and update the status page (sev-status-page-update.md), re-checking references until the pair is safe to unfreeze by hand.
- [ ] Step 3 — if NO reference is available either (e.g. CoinGecko 429) → the divergence worker is the upstream issue; jump to `divergence-refresh-error-dominant.md`.
- [ ] Verification: an escalated freeze NEVER auto-recovers — its hold slides forward every tick (`internal/aggregate/freeze/lifecycle.go`) and the marker TTL tracks the remaining hold, so `recovered_at` is stamped only by `freeze-unfreeze -write` (which clears the marker and closes the row). After unfreezing confirm `redis-cli EXISTS freeze:<asset>:<quote>` = 0, the `freeze_events` row has `recovered_at` set, and `/v1/price` no longer carries `flags.frozen`. The 60 s `internal/aggregate/freeze.Recovery` sweep applies to NON-escalated freezes whose marker lapsed (it also consults the durable ladder and leaves a row open while the hold is live); if a non-escalated row stays open past its marker TTL, see [freeze-recovery-stalled](freeze-recovery-stalled.md).

## Root cause analysis

Capture for postmortem:
- The full freeze_events history for the affected (asset, quote) pairs over the last 24 h, including `extensions_used` / `hold_until`.
- Snapshot of the divergence worker's `divergence_observations` (`our_price`, `ref_price`, `delta_pct`, `status`) for the same window, or `GET /v1/divergence`.
- Adjacent Prometheus alerts `stellarindex_aggregator_class_drop_spike` / `stellarindex_aggregator_outlier_storm` (`configs/prometheus/rules.r1/aggregator.yml`) and the aggregator journal: `journalctl -u stellarindex-aggregator | grep "phase2 freeze"`.

## Known false-positive patterns

- **Phase-2 sparse baseline (UNSCORED buckets)**: the per-asset baseline is a persisted 30-day density-based row, not a 30-minute in-memory window. While it is too sparse, `computeConfidence` returns `confOK=false`, the bucket is UNSCORED and therefore not freeze-eligible — and an unscored bucket also cannot earn an unfreeze streak, so a freeze that straddles a sparse baseline walks the ladder toward `escalated` instead of releasing. Corrected 2026-08-04: an earlier version claimed the threshold is "intentionally lenient" during bootstrap; there is no such leniency. The post-restart variant of this (a frozen pair had no comparator after an aggregator restart and every bucket was unscored) was FIXED 2026-08-24 by #142: `frozenPrevVWAPs` is a shadow comparator, so a frozen pair is scorable from its second bucket after a restart. Do not chase "it restarted recently" as the cause anymore.
- **Stablecoin depeg masquerading as anomaly**: ADR-0026 says we late-bind stablecoin → fiat at VWAP compute time. A real depeg looks like an anomaly; check the `divergence_warning` flag — but ONLY when `flags.divergence_checked=true` (CS-087): a `false` warning with `divergence_checked=false` means the check was blind, not that prices agree. If it fires, the freeze is correct.
- **Lens-less pair escalating on a calm level (expected since 2026-08-24)**: auto-unfreeze additionally requires a corroborating lens agreeing within 5% with the fresh candidate (ADR-0019 amendment 2026-08-24). A pair with no usable cross-oracle reference (SuccessCount < 2 — on the default set, the EUR/GBP-quoted pairs whose only reference is CoinGecko) can NEVER auto-release: it walks the ladder here even if the price looks perfectly calm. That is by design — calm is gameable (a held manipulated level reads calm per-tick). Your job is the judgment the machine refused to make: compare the frozen pair's fresh candidate against any reference you trust (`curl -s 'localhost:3000/v1/divergence?limit=50'` or `SELECT * FROM divergence_observations WHERE asset_id = ... ORDER BY observed_at DESC`, coingecko directly, the sibling USD-quoted pair × the fiat cross), and if the level is genuine, `stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -asset <A> -quote <Q> -reason "..." -write`. If these pages become frequent on a specific pair, the durable fix is adding a second reference source for its quote currency, not loosening the gate.

## Related

- `anomaly-freeze-engaged.md` — the per-tick alert; this runbook covers the escalated/sustained variant. NOTE: the sustained rule's own `runbook_url` (`configs/prometheus/rules.r1/anomaly.yml` + deploy mirror) still points at anomaly-freeze-engaged.md — follow-up: point it here.
- `stellarindex_anomaly_freeze_escalated` (`configs/prometheus/rules.r1/freeze-lifecycle.yml`) — the companion P1 for the same escalation; its runbook_url links here. Two pages fire per escalation.
- `freeze-recovery-stalled.md` — a non-escalated durable row that outlives its marker.
- `aggregator-outlier-storm.md` — adjacent symptom when the σ-filter goes wide.
- `divergence-refresh-error-dominant.md` — upstream when references can't be fetched.
- ADR-0019 — anomaly response + confidence scoring + the extension ladder.
- F-1228 + F-1229 (audit-2026-05-12) — closed: `freeze_events.frozen_value` is written verbatim (0 only on a first-tick freeze with no prior bucket); `MarkRecovered` is called by `freeze.Recovery` and by `freeze-unfreeze`.
- Follow-ups worth filing: make `stellarindex_anomaly_freeze_escalated_total` a CounterVec by class (or drop `sum by (class)` from the sustained expr); add `-write` to the `freeze-unfreeze` usage text in `cmd/stellarindex-ops/main.go`.

## Changelog

- 2026-08-28 — re-verified against HEAD: alert is P1 page on `escalated_total` (reshaped 2026-08-06), rule path is `configs/prometheus/rules.r1/`, class enum corrected, Phase-2 tuning + restart (no SIGHUP), escalated freezes never auto-recover, `ref_price` column, #142 restart-stall fix, ops command flag set, F-1228/F-1229 closed.
- 2026-08-24 — corroborated-release amendment: lens-less pairs always escalate; added the expected-pattern entry.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
