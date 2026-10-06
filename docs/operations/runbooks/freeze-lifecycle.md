---
title: Runbook — freeze lifecycle
last_verified: 2026-10-06
status: draft
---

# Runbook — freeze lifecycle (ADR-0019 ladder)

Alerts: `configs/prometheus/rules.r1/freeze-lifecycle.yml` (mirror, identical: `deploy/monitoring/rules/freeze-lifecycle.yml`). Engage-side alerts live in `anomaly.md`.

Ladder: initial hold + 4 × 30 min extensions. Auto-unfreeze needs confidence > 0.30 AND z < 3.0 for two consecutive buckets, each corroborated by a lens agreeing within 5% with the fresh candidate (ADR-0019). A pair with no usable reference (SuccessCount < 2; on the default set, EUR/GBP-quoted pairs whose only reference is CoinGecko) never auto-releases and always escalates, even on a calm price (calm is gameable). An escalated freeze is operator-only: `/v1/price` serves the last-known-good (LKG) value with `flags.frozen=true` (typically also `flags.single_source=true`).

## At a glance

- [`stellarindex_anomaly_freeze_escalated`](#stellarindex_anomaly_freeze_escalated)
- [`stellarindex_anomaly_freeze_extension_rate`](#stellarindex_anomaly_freeze_extension_rate)
- [`stellarindex_anomaly_freeze_ladder_write_failures`](#stellarindex_anomaly_freeze_ladder_write_failures)
- [`stellarindex_api_freeze_lookup_failing`](#stellarindex_api_freeze_lookup_failing)
- [`stellarindex_anomaly_freeze_operator_unfreeze_rate`](#stellarindex_anomaly_freeze_operator_unfreeze_rate)

## Shared diagnosis

```sh
# WHICH pair (the escalated counter has no pair/class label)
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
sudo -u postgres psql -d stellarindex -c "SELECT asset_id, quote_id, frozen_at, reason, extensions_used, escalated, hold_until FROM freeze_events WHERE recovered_at IS NULL ORDER BY frozen_at DESC;"
redis-cli --scan --pattern 'freeze:*'
redis-cli GET <key> | jq .state      # fired_at, hold_until, extensions_used, escalated, unfreeze_streak
redis-cli TTL 'freeze:<asset>:<quote>'  # escalated: TTL keeps sliding (remaining hold + 5 min grace)
journalctl -u stellarindex-aggregator | grep -E "freeze (engaged|hold extended|escalated|released)"
curl -s http://localhost:9090/api/v1/query?query='sum%20by%20(class)%20(rate(stellarindex_anomaly_freeze_engaged_total[1h]))'
curl -s 'http://localhost:3000/v1/price?asset=<asset>&quote=<quote>' | jq '.flags'
```

`engaged_total` is labelled `class` (NOT `asset_class`; the wrong name collapses all series into one empty label); `escalated_total` is unlabelled.

- Single class (`stablecoin` / `treasury` / `crypto` / `governance` / `default`) → per-class threshold too tight. `default` = unclassified asset, or Phase-1 checker disabled (`[anomaly] enabled=false`; `classOf()` then emits `default` for everything).
- All classes → upstream data quality: a divergence lens (coingecko / reflector / chainlink / synthetic cross, `internal/divergence`) or a CEX/DEX source went bad.
- Flag clears + re-engages cyclically → genuine distress; the freeze is working (does not page by itself).

## Lifting a freeze

`stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -asset <A> -quote <Q> -reason "..." [-actor NAME] -write`. Default is DRY-RUN; `-write` applies. It clears the marker, stamps `recovered_at` on the `freeze_events` row, and counts `stellarindex_anomaly_freeze_released_total{mode="operator"}`. If the condition still fires the pair re-freezes (intended; fix thresholds/baseline, do not unfreeze repeatedly).

Do NOT `redis-cli DEL` the marker of an escalated freeze: since migration 0119 the aggregator rehydrates the ladder from `freeze_events` when the marker is absent and re-writes the key next tick, so the key vanishes while the price stays pinned.

Verify after: `redis-cli EXISTS freeze:<asset>:<quote>` = 0, row has `recovered_at`, `/v1/price` has no `flags.frozen`. An escalated freeze never auto-recovers (hold slides each tick, `internal/aggregate/freeze/lifecycle.go`); only `freeze-unfreeze -write` stamps `recovered_at`. The 60 s `internal/aggregate/freeze.Recovery` sweep covers NON-escalated freezes whose marker lapsed (it consults the durable ladder and leaves a row open while the hold is live); a non-escalated row open past marker TTL → `anomaly.md#stellarindex_anomaly_freeze_recovery_stalled`.

## stellarindex_anomaly_freeze_escalated

P1 page. `increase(stellarindex_anomaly_freeze_escalated_total[15m]) > 0`, `for: 0m`. Only escalation pages; engage → extend → auto-release cycling does not. Typical MTTR 30-90 min.

Means: a freeze exhausted the ladder (≥ 2 h) and is held until an operator acts: sustained manipulation, a genuine repricing the baseline has not absorbed, no corroborating reference (see above), or Phase-2 thresholds too tight. Customer view: price stuck for the pair; `stellarindex_anomaly_freeze_active > 0` holds steady; `freeze_events` has an open row with `escalated = true`, `recovered_at IS NULL`.

Diagnose: shared commands above (the `freeze escalated` journal line is one ERROR per escalation with pair, fired_at and the confidence/z/source_count reason).

Fix:
1. Confirm via cross-references (`curl -s 'localhost:3000/v1/divergence?limit=50'`, `SELECT * FROM divergence_observations WHERE asset_id = ... ORDER BY observed_at DESC` (`our_price`, `ref_price`, `delta_pct`, `status`), CoinGecko directly, Reflector, Chainlink, the sibling USD-quoted pair × the fiat cross). LKG within ±2% of references → freeze is over-cautious; level genuine → lift (above).
2. On r1 the firing layer is Phase 2: tune `[anomaly.phase2]` (`z_score_min_freeze`, `confidence_max_freeze`, `source_count_max_freeze`; ansible var `stellarindex_phase2_z_min_freeze`) via `configs/ansible` in the same PR, then `sudo systemctl restart stellarindex-aggregator` (no SIGHUP reload). The Phase-1 `[anomaly.thresholds.<class>]` table applies only when `[anomaly] enabled = true`, which the r1 template does not set.
3. References disagree → market really distressed: keep held, update status page (`sev-status-page-update.md`), re-check until safe. Confirmed manipulation → leave frozen, ack.
4. No reference available (e.g. CoinGecko 429) → `divergence.md#stellarindex_divergence_refresh_error_dominant`. If a lens-less pair pages repeatedly, the durable fix is a second reference source for its quote currency, not loosening the gate.

False-positive patterns:
- Phase-2 sparse baseline: the baseline is a persisted 30-day density-based row. While too sparse, `computeConfidence` returns `confOK=false`, the bucket is UNSCORED, not freeze-eligible and cannot earn an unfreeze streak, so a freeze straddling it walks the ladder to escalated. There is no bootstrap leniency. After an aggregator restart a frozen pair is scorable from its second bucket (`frozenPrevVWAPs` shadow comparator, #142); do not blame a recent restart.
- Stablecoin depeg: a real depeg looks like an anomaly (ADR-0026 late-binds stablecoin → fiat). Check `divergence_warning` only when `flags.divergence_checked=true`; `false` with `divergence_checked=false` means the check was blind. If it fires, the freeze is correct.

Postmortem capture: `freeze_events` history for the pairs over 24 h (incl. `extensions_used` / `hold_until`); `divergence_observations` or `GET /v1/divergence` for the window; adjacent `stellarindex_aggregator_class_drop_spike` / `stellarindex_aggregator_outlier_storm` (`configs/prometheus/rules.r1/aggregator.yml`) and the aggregator journal. Related: `aggregator.md#stellarindex_aggregator_outlier_storm`, `anomaly.md#stellarindex_anomaly_freeze_engaged`.

## stellarindex_anomaly_freeze_extension_rate

P3 ticket. `increase(stellarindex_anomaly_freeze_extensions_total[1h]) >= 3`, `for: 10m`.

Means: ≥ 3 holds extended in the last hour; each is a pair that hit hold expiry without meeting auto-unfreeze. Leading indicator of the P1 (four extensions on one pair → page).
- Extensions climbing, `stellarindex_anomaly_freeze_active` flat at 1 → ONE pair stuck; expect the P1 within 2 h.
- Both climbing → broad false-firing, usually cold/sparse per-asset baselines (Phase-2 sparse baseline above; `anomaly.md` "Cold baseline (Phase 2)"), not a market event.

Fix: shared diagnosis; tune per the escalated section. The freeze itself is correct in both cases.

## stellarindex_anomaly_freeze_ladder_write_failures

P3 ticket. `increase(stellarindex_anomaly_freeze_ladder_write_failures_total[15m]) > 0`, `for: 0m`. Label `op` = `mark_hold|clear`.

Means: a durable ladder write (migration 0119) did not land. `mark_hold` failing is the dangerous direction: the ladder was never recorded, so a Redis flush during the freeze rehydrates nothing and an escalated pair silently releases. Causes: Postgres error, or a deploy that swapped the binary before 0119 applied (every write matches zero rows). `clear` failing only widens the recovery-worker window.

Fix: check the aggregator's Postgres connectivity; confirm migration 0119 (`migrations/0119_freeze_events_lifecycle`) is applied in this environment.

## stellarindex_api_freeze_lookup_failing

P3 ticket. `increase(stellarindex_api_freeze_lookup_failures_total[10m]) > 0`, `for: 5m`.

Means: the API cannot read freeze markers (Redis outage/timeout) for 5+ minutes; `/v1/price` is served with `frozen_checked=false` (no freeze verdict) and a frozen pair may be published as live.

Fix: check API → Redis connectivity.

## stellarindex_anomaly_freeze_operator_unfreeze_rate

P3 ticket. `increase(stellarindex_anomaly_freeze_released_total{mode="operator"}[1h]) >= 3`, `for: 10m`.

Means: ≥ 3 freezes cleared via `freeze-unfreeze` in the last hour; each is a pair auto-unfreeze never satisfied. Sustained rate = calibration produces freezes needing manual correction.

Fix: change thresholds/baseline (escalated section step 2), not more unfreezes; consider a second reference source for lens-less pairs.

See also: ADR-0019 (anomaly response, confidence scoring, extension ladder).

## Related

- [Alerts catalogue](../alerts-catalog.md)
