# W10 — Aggregation, divergence, freeze, confidence, anomaly

## Scope

From raw trades to served price + confidence.
`internal/aggregate/`, `internal/divergence/`,
`cmd/ratesengine-aggregator/main.go`.

## Inputs

- `internal/aggregate/{vwap,twap,ohlc,outliers,triangulate,stablecoin,global}.go`
- `internal/aggregate/{anomaly,baseline,changesummary,confidence,freeze,orchestrator}/`
- `internal/divergence/{chainlink,coingecko,compare,reference,worker,depeg_test}.go`
- ADR-0019 (anomaly + confidence), ADR-0026 (stablecoin late
  binding)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W10.1 | VWAP: per-source weight derivation; min-trade-count gating | `vwap.go` + tests |
| W10.2 | TWAP: time-weighted slope; window boundaries | `twap.go` |
| W10.3 | OHLC: candle close at bucket boundary; partial-candle handling | `ohlc.go` |
| W10.4 | Outliers: detection threshold; per-source exclusion vs reweight | `outliers.go` |
| W10.5 | Triangulation: provenance markers; transitive divergence | `triangulate.go` |
| W10.6 | Stablecoin: ADR-0026 late binding; depeg test | `stablecoin.go`, `depeg_test.go` |
| W10.7 | Global: aggregate-of-aggregates handling | `global.go` |
| W10.8 | Anomaly: trigger thresholds, freeze hand-off | `anomaly/` |
| W10.9 | Baseline: multi-window volatility (migrations 0007, 0008) | `baseline/` |
| W10.10 | ChangeSummary: % change derivation; migration 0022 backing table | `changesummary/` |
| W10.11 | Confidence: ADR-0019 scoring; reduced_redundancy flag | `confidence/` |
| W10.12 | Freeze: events table (migration 0018); recovery sweep | `freeze/` |
| W10.13 | Orchestrator: wiring + scheduler | `orchestrator/` |
| W10.14 | Divergence: CoinGecko reference path | `divergence/coingecko.go` |
| W10.15 | NEW: Divergence: Chainlink HTTP RPC reference | `divergence/chainlink.go` |
| W10.16 | Divergence: compare + worker | `compare.go`, `worker.go` |
| W10.17 | Aggregator class policy: exchange contributes by default; aggregators + oracles excluded; FX snap fallback rules | `external.Registry.Lookup` |
| W10.18 | NEW: Stablecoin depeg detection: USDT/USDC/PYUSD/EUROC/EUROB/MXNe — divergence_warning fires on simulated depeg | `depeg_test.go` |
| W10.19 | Anomaly → Freeze hand-off: freeze flag in API envelopes during freeze window | end-to-end test |
| W10.20 | Freeze recovery: sweep clears the flag when condition resolves | code + metric |

## Adversarial checks (cross-ref attack tree)

| # | Check | Method |
| --- | --- | --- |
| W10.A.1 | Hostile source feeding 10x-off prices: VWAP outlier-detected and dropped (J34) | journey test |
| W10.A.2 | Single class drop: aggregator-class fallback documented + alerted (C3.2) | rule + runbook |
| W10.A.3 | Triangulation via stale FX: triangulated flag + stale-FX flag | code |

## Closure criteria

All checks terminal. Findings on any silent-bad-data path
(consumer reads price that's wrong without `divergence_warning`,
`stale`, or `reduced_redundancy` flag).
