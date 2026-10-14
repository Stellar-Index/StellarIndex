---
title: Runbook — freeze lifecycle
last_verified: 2026-10-06
status: draft
---

# Runbook — freeze lifecycle (ADR-0019 ladder)

## At a glance

Anomaly-freeze ladder and API freeze-lookup alerts.

Alerts: `configs/prometheus/rules.r1/freeze-lifecycle.yml` (mirror `deploy/monitoring/rules/freeze-lifecycle.yml`). Engage-side alerts are in `anomaly.md`. Ladder: initial hold + 4 x 30 min extensions; auto-unfreeze needs confidence > 0.30 AND z < 3.0 for two consecutive buckets, each corroborated by a lens within 5% (ADR-0019). A pair with no usable reference (SuccessCount < 2, e.g. EUR/GBP-quoted pairs referenced only by CoinGecko) never auto-releases and always escalates. An escalated freeze is operator-only: `/v1/price` serves the last-known-good (LKG) value with `flags.frozen=true`.

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

`engaged_total` is labelled `class` (NOT `asset_class`); `escalated_total` is unlabelled.

- One class (`stablecoin` / `treasury` / `crypto` / `governance` / `default`): per-class threshold too tight. `default` = unclassified asset, or Phase-1 checker disabled (`[anomaly] enabled=false`).
- All classes: upstream data quality (a divergence lens in `internal/divergence`, or a CEX/DEX source).
- Clears and re-engages cyclically: genuine distress; the freeze is working.

## Lifting a freeze

`stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -asset <A> -quote <Q> -reason "..." [-actor NAME] -write`. Default is DRY-RUN. It clears the marker, stamps `recovered_at` on the `freeze_events` row and counts `stellarindex_anomaly_freeze_released_total{mode="operator"}`. If the condition still fires the pair re-freezes: fix thresholds/baseline, do not unfreeze repeatedly.

Do NOT `redis-cli DEL` an escalated freeze's marker: the aggregator rehydrates the ladder from `freeze_events` (migration 0119) and re-writes the key next tick while the price stays pinned.

Verify: `redis-cli EXISTS freeze:<asset>:<quote>` = 0, the row has `recovered_at`, `/v1/price` has no `flags.frozen`. Only `freeze-unfreeze -write` recovers an escalated freeze (`internal/aggregate/freeze/lifecycle.go`); the 60 s `freeze.Recovery` sweep covers non-escalated ones, and such a row open past marker TTL goes to `anomaly.md#stellarindex_anomaly_freeze_recovery_stalled`.

## stellarindex_anomaly_freeze_escalated

P1 page. `increase(stellarindex_anomaly_freeze_escalated_total[15m]) > 0`, `for: 0m`. A freeze exhausted the ladder (>= 2 h) and is held until an operator acts: manipulation, a genuine repricing the baseline has not absorbed, no corroborating reference, or Phase-2 thresholds too tight. Price is stuck for the pair; `stellarindex_anomaly_freeze_active > 0`; `freeze_events` has an open row with `escalated = true`.

**Check** Shared diagnosis (the `freeze escalated` journal line carries pair, fired_at and the confidence/z/source_count reason).

**Fix**

1. Confirm against references: `curl -s 'localhost:3000/v1/divergence?limit=50'`, `SELECT * FROM divergence_observations WHERE asset_id = ... ORDER BY observed_at DESC` (`our_price`, `ref_price`, `delta_pct`, `status`), CoinGecko, Reflector, Chainlink, the USD-quoted sibling x fiat cross. LKG within +-2% of references: over-cautious; level genuine: lift (above).
2. Tune `[anomaly.phase2]` (`z_score_min_freeze`, `confidence_max_freeze`, `source_count_max_freeze`; ansible `stellarindex_phase2_z_min_freeze`) in `configs/ansible` in the same PR, then `sudo systemctl restart stellarindex-aggregator` (no SIGHUP reload). Phase-1 `[anomaly.thresholds.<class>]` applies only with `[anomaly] enabled = true`, which the r1 template does not set.
3. References disagree: market genuinely distressed; keep held and update the status page (`sev-status-page-update.md`). Confirmed manipulation: leave frozen, ack.
4. No reference (e.g. CoinGecko 429): `divergence.md#stellarindex_divergence_refresh_error_dominant`. A lens-less pair that pages repeatedly needs a second reference source for its quote currency, not a looser gate.

**Likely false positives** Sparse Phase-2 baseline (30-day persisted row): confidence is unscorable, so the bucket cannot be freeze-eligible or earn an unfreeze streak and a straddling freeze walks the ladder; a frozen pair is scorable from its second bucket after a restart. A real stablecoin depeg looks like an anomaly (ADR-0026): the freeze is correct; `divergence_checked=false` means the check was blind.

**Escalate** Capture `freeze_events` history over 24 h, `divergence_observations`, adjacent `stellarindex_aggregator_class_drop_spike` / `stellarindex_aggregator_outlier_storm` (`aggregator.yml`) and the journal. See `aggregator.md#stellarindex_aggregator_outlier_storm`, `anomaly.md#stellarindex_anomaly_freeze_engaged`.

## stellarindex_anomaly_freeze_extension_rate

P3. `increase(stellarindex_anomaly_freeze_extensions_total[1h]) >= 3`, `for: 10m`. Leading indicator of the P1: extensions climbing with `stellarindex_anomaly_freeze_active` flat at 1 is ONE stuck pair (P1 within 2 h); both climbing is broad false-firing, usually cold/sparse baselines (`anomaly.md` "Cold baseline (Phase 2)"). Fix: shared diagnosis, tune as in escalated step 2.

## stellarindex_anomaly_freeze_ladder_write_failures

P3. `increase(stellarindex_anomaly_freeze_ladder_write_failures_total[15m]) > 0`, `for: 0m`; label `op` = `record_freeze|mark_hold|clear`. A durable write (migration 0119) failed after one retry. `record_freeze`: the `freeze_events` row is missing, Redis still freezes but SQL readers treat the minute as unfrozen (log: `component=freeze-writer`). `mark_hold`: the ladder was not recorded, so a Redis flush silently releases an escalated pair. `clear`: only widens the recovery window. Causes: Postgres error, or a binary deployed before 0119 applied. Fix: check the aggregator's Postgres connectivity; confirm `migrations/0119_freeze_events_lifecycle` is applied.

## stellarindex_api_freeze_lookup_failing

P3. `increase(stellarindex_api_freeze_lookup_failures_total[10m]) > 0`, `for: 5m`. The API cannot read freeze markers (Redis outage/timeout); `/v1/price` is served with `frozen_checked=false` and a frozen pair may publish as live. Fix: check API to Redis connectivity.

## stellarindex_anomaly_freeze_operator_unfreeze_rate

P3. `increase(stellarindex_anomaly_freeze_released_total{mode="operator"}[1h]) >= 3`, `for: 10m`. Calibration produces freezes needing manual release. Fix: change thresholds/baseline (escalated step 2), not more unfreezes; consider a second reference for lens-less pairs. ADR-0019.

## Related

[Alerts catalogue](../alerts-catalog.md)
