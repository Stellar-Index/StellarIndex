# Outlier-trim drift artifact: MAD-over-window trims agreed regime shifts; alert measures re-counted window tail, not venue disagreement

> **Status (2026-09-02): SHIPPED WITH AMENDMENTS (#244, 2026-08-28).** The mechanics as built are **not** the §Design sketch below — read `internal/aggregate/outliers_local.go` (its header documents the anchored, time-local median+1.4826·MAD filter and why the union predicate and nearest-neighbour fallback were added). The alert as built is `stellarindex_aggregator_outlier_trim_fraction` (`configs/prometheus/rules.r1/aggregator.yml:115`), with the pre-2026-08-28 counter gate kept as `stellarindex_aggregator_outlier_trim_rate_legacy` (`:162`) for a one-week overlap that **retires 2026-09-04**. **PR-5 (the window-cap rework) was NOT shipped**, so the §Risks window-cap item still stands.
>
> *Original header:* Status: DESIGN (Wave B, 2026-08-28). Produced by a read-only investigation against HEAD 4aa93e96; every claim cites file:line. Open questions at the end are decisions for the operator.

# Outlier-trim drift artifact (2026-08-28)

**Status:** design, read-only investigation. **Scope:** `internal/aggregate/outliers.go`, `robust.go`, `orchestrator.refreshPairWindow`, `stellarindex_aggregator_outlier_storm`.

## Incident
2026-08-28, r1: `stellarindex_aggregator_outlier_storm` fired on `crypto:XLM/fiat:USD` at ~51 drops/s while binance/coinbase/kraken agreed within 0.9% and served prices were correct.

## What the code actually does
- `FilterOutliers(trades, sigma)` (outliers.go:53) drops a trade when `|p − median| > sigma · max(1.4826·MAD, floor)` where the floor (0.5%·centre) applies only when MAD==0 (robust.go:118). It is **not** mean/σ; the orchestrator Config comment (orchestrator.go:291) is stale.
- `refreshPairWindow` (orchestrator.go:1028) runs it on the whole trailing window for each of `[5m,1h,24h]` every 30 s and adds `pre − post` to `stellarindex_aggregator_dropped_trades_total{reason=\"outlier\",pair}` — the same trades are re-counted every tick, summed across windows. 51/s ≈ 1 530 trades currently outside the band.
- The window is the newest `MaxTradesPerWindow`=10 000 rows **per source pair** (trades.go:1470, orchestrator.go:1491); on CEX pairs the \"24h\" slice is hours.

## Why an agreed move gets trimmed
MAD measures the majority regime's dispersion (~0.1–0.3% on XLM/USD). Band = 5.93·MAD ≈ 0.6–1.8%. Any agreed move larger than that, occupying < 50% of the slice, is dropped wholesale until it becomes the majority, then the *old* regime is dropped.

Red-proofs on HEAD (`go test -overlay`, sigma 4, 0.1875 → −2.5%):

| case | result on HEAD |
|---|---|
| 8 400 old + 1 600 new (16% tail) | 1 600 dropped |
| window VWAP bias | +0.40% toward stale level |
| 51/49 → 49/51 split | served VWAP −2.50% in one tick (true −0.05%) |
| linear 2%/day drift | 0 dropped (uniform spread: MAD=R/4) |

## Serving impact
- Affected: `/v1/price?window=300|3600|86400`, confidence, anomaly warn/div flag, `min_usd_volume` survivor gate.
- Not affected: `/v1/price` (closed 1m bucket), `/v1/price/tip` (no filter).
- Bias ≈ f·d (max d/2) then a discontinuity ≈ d; lag to the flip: 5m ≈ 2.5 min, 1h ≈ 30 min, 24h ≈ half the truncated slice.

## Design: time-local residual trimming
`FilterOutliersLocal(trades, OutlierOptions{Sigma, Bucket=1m, MinBucket=3, MinSources=2, RelFloor=1/200})`:
1. per-bucket median as local reference when the bucket has ≥3 prices from ≥2 sources, else the window median (legacy);
2. residual = min-|·| of relative deviation to own/prev/next bucket reference;
3. median+MAD on residuals, scale = max(1.4826·MAD, RelFloor) always;
4. drop when |r − centre| > Sigma·scale.

Prototype passes: masking [100×4,200], honest [100×4,101], fat-finger 110, 20+10 000 tail, 10× print inside a shift window, 16% tail, 49/51 flip, mid-bucket step (all 0 false drops). `FilterOutliers` keeps its signature for `/v1/vwap` and `/v1/ohlc`.

## Alert redesign
Metrics: `stellarindex_aggregator_venue_vwap{pair,window,source}` (pre-outlier per-source VWAP, 5m), `stellarindex_aggregator_window_trades{pair,window,stage}`, and `dropped_trades_total{reason=outlier}` deduped by trade ID.

```promql
(max by (pair)(stellarindex_aggregator_venue_vwap{window=\"5m\"})
 / min by (pair)(stellarindex_aggregator_venue_vwap{window=\"5m\"}) - 1) > 0.01
and on (pair) count by (pair)(stellarindex_aggregator_venue_vwap{window=\"5m\"}) >= 2
```
`for: 15m`, ticket. Trim-fraction successor: `window_trades{stage=outlier} / window_trades{stage=class} > 0.2 for 30m`. Retire `outlier_storm` after one week of overlap. promtool cases: agreed drift silent; one venue +3% fires at 15m; single venue never fires.

## PRs
1 tests-red · 2 filter+config · 3 metrics · 4 rules/runbook/docs · 5 (optional) window-cap rework.

## Live verification still required
`window_truncated_total` and per-source slice depth on r1; the div/confidence trace around the 2026-08-28 flip.

## Open questions

- Where did the task's 'mean 0.18610, σ 0.001866, z≈−2.1' come from — the full 24h DB window or the newest-10k slice? The trim decision uses the slice and MAD, so those numbers do not by themselves explain the drops.
- Should api/v1/vwap.go and ohlc.go (per-request ?outlier_sigma) also move to the local filter, or stay legacy for API stability? Their trades carry timestamps so the switch is cheap, but the OpenAPI doc describes the current semantics.
- Bucket size: 1m matches the closed-bucket surface; for thin pairs (<3 trades/min) most buckets fall back to legacy. Is a volume-adaptive bucket (e.g. bucket = window/60) preferable?
- Does anyone consume stellarindex_aggregator_dropped_trades_total{reason=outlier} beyond the alert (Grafana dashboards, status page)? The semantics change with dedupe.
- Confirm on r1 whether the flip produced a divergence_warning / confidence dip on 2026-08-28 (Redis div: key, confidence table) to decide if PR3 must also feed the freeze path the pre-trim VWAP.

## Risks

- MinSources fallback: a single-source pair (SDEX-only) still gets the legacy whole-window filter, so the drift artifact persists there; conversely a multi-source bucket dominated by one spamming source (2026-08-14 token farm shape) could set the local reference — mitigated by MinSources>=2 and by the class filter, but needs the spam-bucket test.
- Always-on 0.5% RelFloor widens the band for very tight pairs (stablecoin pegs): a 1.9% print inside a peg's 0.01% cluster now survives at sigma 4. Consider RelFloor 1/400 or per-pair override; served_guard.go's ratio band still backstops the served price.
- The counter dedupe set is per-process memory; after a restart previously-dropped IDs are re-counted once (bounded, one-tick artifact). Document it.
- Retiring outlier_storm removes the only alert that caught the 2026-08-14 token farm; the trim-fraction rule must be proven on that fixture before the old rule is deleted (keep both for one week).
- MaxTradesPerWindow=10k per source pair silently shortens the '24h' window on CEX pairs; the local filter does not fix that, and the served window_seconds=86400 label is a claim not matched by the data (separate finding to file).
- Bias estimates use the synthetic ±0.15% majority dispersion; real XLM/USD MAD and tail fraction on 2026-08-28 are unverified (REQUIRES-LIVE-VERIFY) — the direction and the flip discontinuity are structural, magnitudes scale with d and f.
