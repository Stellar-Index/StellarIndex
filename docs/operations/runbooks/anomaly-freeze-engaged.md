---
title: Runbook — anomaly-freeze-engaged
last_verified: 2026-07-05
status: ratified
severity: P3
---

# Runbook — `stellarindex_anomaly_freeze_engaged`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_anomaly_freeze_engaged` (P3 / ticket) · `stellarindex_anomaly_freeze_sustained` (P1 / page once sustained ≥ 1h) |
| Severity | P3 on first 5m window; escalates to P1 at 1h sustained |
| Detected by | Prometheus rules in `deploy/monitoring/rules/anomaly.yml` + `configs/prometheus/rules.r1/anomaly.yml` |
| Typical MTTR | 5–30 min for a confirmed decision (confirm or override); cold-baseline false-fires resolve with a config tune |
| Impact | The affected pair's `/v1/price` serves the last-known-good (LKG) VWAP with `flags.frozen: true`. `/v1/price/tip` and `/v1/observations` keep serving live data (ADR-0018 per-surface policy). Lending-style consumers of the closed-bucket surface are protected; nothing is deleted. |
| Per-pair triage | The counter is class-labelled only (cardinality bound). Find the pair via the Redis markers: `redis-cli --scan --pattern 'freeze:*'`, or the aggregator journal (grep below). |

## What freeze means

Two freeze layers run in the aggregator (`internal/aggregate/orchestrator`),
both per [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md);
either can emit `ActionFreeze`:

1. **Phase 1 — per-class thresholds** (`[anomaly.thresholds.*]` TOML):
   bucket-to-bucket deviation above the class `freeze_pct` on a
   single-source bucket. Log line: `anomaly freeze engaged` (WARN).
2. **Phase 2 — 3-signal AND** (per-asset MAD baseline + multi-factor
   confidence): fires only when ALL of
   `confidence < 0.10` AND `z_score > 5.0` AND `source_count <= 1`
   hold (knobs: `[anomaly.phase2]` `confidence_max_freeze` /
   `z_score_min_freeze` / `source_count_max_freeze`). Log line:
   `phase2 freeze engaged` (INFO), with per-pair confidence, z-score
   and source count.

This is the extreme corner: anomalous movement on a single source
with no multi-source consensus. It catches USTRY-shape oracle
manipulation and is designed NOT to fire on real market events
(those have multi-source coverage, so `source_count > 1`).

Cross-oracle agreement enters the freeze decision ONLY through the
confidence score (the `cross_oracle` factor fed by the divergence
worker's per-pair result) — there is no separate cross-oracle freeze
condition, by ADR-0019 design.

On freeze the orchestrator: skips the pair's VWAP cache write (the
prior bucket's value keeps serving, its TTL refreshed for the freeze
lifetime), writes a `freeze:<asset>:<quote>` Redis marker
(`cachekeys.FreezeTTL` = 5 min, refreshed while the condition keeps
firing), INSERTs an open `freeze_events` row (durable mirror for the
explorer /anomalies timeline), and increments
`stellarindex_anomaly_freeze_engaged_total{class}`.

**Auto-clear:** when the condition stops firing, the marker's 5-min
TTL elapses, `/v1/price` returns to live serving, and the freeze
recovery worker (60s poll) stamps `recovered_at` on the durable row.
There is no operator-facing `unfreeze` CLI today — the ADR's
30-min-extension state machine is not implemented; the `_sustained`
1h P1 alert plays the escalation role instead.

## Symptoms

- `rate(stellarindex_anomaly_freeze_engaged_total[5m]) > 0` for some
  `class` label; a sustained anomaly shows a steady increment stream
  (the metric is a counter, not a gauge).
- Affected pair's `/v1/price` carries `flags.frozen: true`; the
  `price` + `observed_at` stop advancing (LKG bucket).
- `redis-cli --scan --pattern 'freeze:*'` shows the affected
  pair(s); `freeze_events` has open rows
  (`SELECT * FROM freeze_events WHERE recovered_at IS NULL`).

## Quick diagnosis (≤ 5 min)

The single most important question: **real market event or
manipulation?** Real events move every venue; manipulation typically
hits one thin venue.

```sh
# 1) Which pairs are frozen, and on which phase/signals?
ssh root@136.243.90.96
redis-cli --scan --pattern 'freeze:*'
journalctl -u stellarindex-aggregator --since -1h | grep -E 'freeze engaged'
# Phase 2 lines carry confidence= z= sources= per pair.

# 2) What does the confidence decomposition say? (served on /v1/price)
curl -s "https://api.stellarindex.io/v1/price?asset=<base>&quote=<quote>" \
  | jq '.data | {price, confidence, confidence_factors, flags: .flags}'
# cross_oracle_checked=true + cross_oracle_agreement>=2 → external
# references corroborate OUR price → lean "real market event".
# cross_oracle_checked=false → we could NOT verify externally —
# never read that as agreement (CS-087).

# 3) What do the cross-oracle references say right now?
curl -s "https://api.stellarindex.io/v1/divergence?limit=50" \
  | jq '.observations[] | select(.asset_id=="<base>")'
# References: reflector-dex/cex/fx, redstone, band (on-chain) +
# coingecko, chainlink. status=firing rows disagree with us.

# 4) Raw per-source observations — which venue moved?
curl -s "https://api.stellarindex.io/v1/observations?asset=<base>&quote=<quote>" | jq
```

Decision tree:

| Cross-oracle references | Our raw observations | Probable cause | Action |
|---|---|---|---|
| ≥ 2 references corroborate the NEW price (`cross_oracle_agreement >= 2`, `/v1/divergence` deltas small) | Move visible across sources | **Real market event.** | Override: widen/tune the class threshold (Phase 1) or the `[anomaly.phase2]` knobs, restart aggregator. Annotate the incident. |
| References stick to the OLD price (divergence firing against us) | Move on one venue only, thin book | **Manipulation.** | Confirm freeze (do nothing). LKG keeps serving; anomaly usually arbitrages out within minutes. |
| No reference covers the asset (`cross_oracle_checked=false`, no `/v1/divergence` rows) | Unclear | **Cannot tell — defer to caution.** | Leave frozen; escalate to on-call lead for human judgment. |
| Conflicting | n/a | Investigation needed | Leave frozen, dig into per-reference rows in `divergence_observations`. |

## Mitigation (≤ 15 min)

- [ ] **Step 1 — identify pair + phase + signals** (diagnosis block
      above). Capture the journal lines and the
      `confidence_factors` JSON for the postmortem.

- [ ] **Step 2 — decide: confirm or override.**

  **Confirming (manipulation suspected):** no action. The freeze
  re-engages every tick while the condition holds and auto-clears
  5 min after it stops. If it sustains 1h the P1 `_sustained` alert
  escalates to human review by design.

  **Overriding (legitimate market event / mis-tune):** the freeze is
  config-driven — fix the config, not the marker:
  ```sh
  # Phase 1 mis-tune: widen the class threshold or fix the asset's
  # class in [anomaly.classifications]; Phase 2 false-fire: raise
  # z_score_min_freeze or lower confidence_max_freeze in
  # [anomaly.phase2]. Codify in configs/ansible/ (vault for
  # secrets), then:
  sudo systemctl restart stellarindex-aggregator
  # Optional immediate flag-clear (marker would otherwise expire
  # within 5 min anyway; it re-engages if the condition still fires):
  redis-cli DEL 'freeze:<asset>:<quote>'
  ```
  Any r1 config change lands in `configs/ansible/` in the same PR
  (CLAUDE.md rule) — a hand-edited TOML will page Monday morning.

- [ ] **Step 3 — cold-baseline false-fire pattern** (most common in
      practice — see `_sustained` alert annotation): if MANY pairs
      freeze at once with `writer_wired=false` or near-genesis
      baselines (backfill mid-run), the per-asset baseline is
      unstable, not the market. Wait for backfill/baseline maturity
      or raise `z_score_min_freeze`; there is no customer impact
      when no marker is being written (`redis-cli --scan --pattern
      'freeze:*'` empty while the counter climbs).

- [ ] **Verification:** `rate(stellarindex_anomaly_freeze_engaged_total[5m])`
      returns to 0; the `freeze:*` marker expires;
      `flags.frozen` disappears from `/v1/price`; the recovery
      worker closes the `freeze_events` row within ~6 min
      (marker TTL + 60s sweep).

## Root cause analysis

For the postmortem, capture:

- Bucket-by-bucket `confidence`, `z_score`, `source_count` from the
  aggregator journal for ±1h around the event.
- The raw trades in the anomalous bucket(s): price, volume, tx-hash
  (`/v1/observations` + the `trades` hypertable).
- Per-reference `divergence_observations` rows for the same window
  (`SELECT * FROM divergence_observations WHERE asset_id=... AND
  observed_at BETWEEN ...`) — did the references corroborate us or
  diverge, and how many (`agreement_count` on the cached result)?
- The verdict: market event vs manipulation vs mis-tune, and which
  evidence decided it.
- Whether auto-clear worked or config intervention was needed;
  whether the `[anomaly]` thresholds should be retuned.
- If manipulation: update
  [oracle-manipulation-defense.md](../../architecture/oracle-manipulation-defense.md)
  "Known incidents".

## Known false-positive patterns

- **Cold baseline (Phase 2).** Sparse history (mid-backfill, young
  deployment) makes the MAD baseline unstable → false-fires across
  many pairs simultaneously. Symptom: counter climbs on many pairs
  at once, often with `writer_wired=false`. Tune or wait; see
  Mitigation Step 3.
- **Asset just listed (bootstrap window).** Confidence is capped at
  0.5 for the first 30 days (`baseline_age_days < 30` in
  `confidence_factors`); combined with single-source coverage it can
  cross the freeze corner on modest moves. Expected; confirm via the
  decomposition and leave frozen or reclassify.
- **Source feed restarting.** A connector restart can briefly drop a
  multi-source pair to single-source; a normal move during that
  window can fire. Auto-clears within one marker TTL; no minimum-
  duration guard exists yet (tracked follow-up).
- **Asset class mis-classification (Phase 1).** A stablecoin
  classified `crypto` gets a 20/50% threshold instead of 1/3% (or
  vice versa — a governance token classed `stablecoin` freezes
  constantly). Audit `[anomaly.classifications]`.

## Related

- Implementation: `internal/aggregate/orchestrator/phase2_freeze.go`
  (3-signal AND), `internal/aggregate/anomaly/` (Phase 1),
  `internal/aggregate/confidence/` (multi-factor score incl. the
  cross-oracle agreement decomposition),
  `internal/aggregate/freeze/` (marker lookup + recovery worker),
  `internal/divergence/` (cross-oracle references + agreement count).
- [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md) —
  the policy this runbook serves.
- [ADR-0018](../../adr/0018-api-consistency-surfaces.md) —
  per-surface freeze application (closed-bucket honours; tip/observations don't).
- [oracle-manipulation-defense.md](../../architecture/oracle-manipulation-defense.md) —
  attack catalogue; this runbook is the operational arm of Layer 9.
- [freeze-recovery-stalled.md](freeze-recovery-stalled.md) —
  companion: the recovery worker (durable-row close side) stalling.
- [price-divergence.md](price-divergence.md) — cross-reference
  divergence triage; often co-fires with a real anomaly.
- [divergence-no-reference.md](divergence-no-reference.md) — when
  the cross-oracle checker itself is dark (CS-087/CS-088): an
  unverifiable freeze decision is weaker evidence in both directions.
- [aggregator-outlier-storm.md](aggregator-outlier-storm.md) —
  distinct: one source diverging inside a multi-source pair.
- Postmortems tagged `anomaly-freeze` — `docs/operations/postmortems/`.

## Changelog

- 2026-07-05 — full rewrite against the shipped implementation:
  real metric names (counter, not gauges), both freeze phases, the
  5-min marker TTL + recovery-worker lifecycle, config-driven
  override path (no `unfreeze` CLI exists), cross-oracle agreement
  triage via `confidence_factors.cross_oracle_checked` /
  `cross_oracle_agreement` and `/v1/divergence`. Status draft →
  ratified.
- 2026-04-28 — initial draft alongside ADR-0019.
