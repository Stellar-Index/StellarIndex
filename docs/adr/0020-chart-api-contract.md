---
adr: 0020
title: Chart API contract — timeframe + granularity + price_type
status: Accepted
date: 2026-04-30
supersedes: []
superseded_by: null
---

# ADR-0020: Chart API contract — timeframe + granularity + price_type

## Context

A price chart needs `(timeframe, granularity, price_type) → points[]`. The existing surfaces do not fit: `/v1/history` is raw trades, `/v1/history/since-inception` is one unbounded series with no timeframe, and `/v1/ohlc`, `/v1/vwap`, `/v1/twap` are single-bar aggregates.

## Decision

Add `GET /v1/chart` as its own endpoint; do not extend `since-inception`, whose unbounded window is its contract and whose latency and cap profile differ from a rolling-window chart.

```
GET /v1/chart?asset=<id>&quote=<id>&timeframe=<tf>&granularity=<g>&price_type=<pt>
  quote        default USD
  timeframe    1h | 24h | 1w | 1mo | 1y | all          default 24h
  granularity  1m | 15m | 1h | 4h | 1d | 1w | 1mo      default per timeframe
  price_type   vwap | twap | market_cap                              default vwap
```

The response mirrors `/v1/history/since-inception`: `data` carries `asset_id`, `quote`, `timeframe`, `granularity`, `price_type` and `points[]` of `{t, p, v_usd}`, plus `flags`.

| Timeframe | Default granularity | Approx points |
|---|---|---|
| `1h` | `1m` | 60 |
| `24h` | `15m` | 96 |
| `1w` | `1h` | 168 |
| `1mo` | `4h` | 180 |
| `1y` | `1d` | 365 |
| `all` | `1d` | variable |

The table is a default, not a constraint; an explicit `granularity` overrides it.

- **`vwap`** is served from the `prices_<gran>` continuous aggregates.
- **`twap`** is served from the `twap_1h` and `twap_1d` aggregates (migration 0081) for every non-fiat base. The requested `granularity` is snapped to the grain a TWAP aggregate backs (`1d`, `1w`, `1mo` to `1d`; anything finer to `1h`), and the response's own `granularity` reports the grain actually served. A fiat:fiat pair is served from `fx_quotes` for every `price_type`, because the daily reference rate is the series. A request is never silently answered with a different `price_type`.
- **Closed buckets only** (ADR-0015): `HistoryPointsInRange` applies `bucket + interval <= now()`. The in-progress bucket is absent; sub-bucket freshness comes from `/v1/price` or `/v1/oracle/latest`.
- **Cap:** `historyMaxPoints = 50_000`, as for `since-inception`. When the requested `(timeframe, granularity)` grid exceeds the cap, the series is served at the finest granularity whose grid fits, and the response's `granularity` reports it (today only `1y` + `1m`, served at `15m`). `timeframe=all` is never coarsened, because its point count is a property of the data. When the cap does cut a series (`timeframe=all`), it keeps the earliest buckets and drops the most recent.
- **`flags.truncated`** is the RETENTION signal, computed from `points[0]` against the window start; it is not a statement about the cap.
- **Retention of `1m`:** migration 0156 attaches a 90-day retention policy to `prices_1m` only, shipped disabled and armed by a deliberate operator act. Coarser rungs keep full history and raw trades are retained forever, so a dropped range is recomputable with a forced `refresh_continuous_aggregate`. The response cap and stored history are independent bounds.

## Invariant

- `/v1/chart` serves only closed buckets, never the in-progress one. Enforced by the `HistoryPointsInRange` closed-bucket filter.
- A response always names the `granularity` and `price_type` actually served, and `twap` never falls back to VWAP. Enforced by `TestChart_TWAP_ServesTimeWeightedSeries`, `TestChart_TWAP_GranularitySnapping` and `TestChart_TWAP_StablecoinFallback` in `internal/api/v1/chart_test.go`.
- A bounded-timeframe request over the point cap is coarsened to a granularity that fits, not cut; the response names the granularity served.

## Consequences

- One endpoint, one storage method (`HistoryPointsInRange` on `HistoryReader`) and one OpenAPI operation; no change to `/v1/history/since-inception`.
- Clients that need the full series (regulators, CSV export, audit) and clients that need a rolling chart evolve independently.
- A consumer can never mistake a snapped or coarsened series for the one it asked for, at the cost of reading `granularity` from the response.

## Evidence

- `internal/api/v1/chart.go`, `internal/api/v1/chart_test.go`; `historyMaxPoints` in `internal/api/v1/history.go`.
- Migrations 0081 (TWAP aggregates) and 0156 (`prices_1m` retention).
