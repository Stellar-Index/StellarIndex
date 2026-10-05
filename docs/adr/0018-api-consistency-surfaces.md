---
adr: 0018
title: API consistency surfaces — closed-bucket, tip, and observations
status: Accepted
date: 2026-04-28
supersedes: []
superseded_by: null
---

# ADR-0018: API consistency surfaces — closed-bucket, tip, and observations

## Context

ADR-0015 makes `/v1/price` closed-bucket only, which gives provable cross-region consistency but 30-120 s staleness that ticker UIs and dashboards will not accept.
Consistency is binary and wire shapes differ, so a query parameter on one endpoint is the wrong switch; each contract needs its own URL.

## Decision

Three API surfaces, three URLs, three explicit consistency contracts.

**`/v1/price`, closed-bucket VWAP.** The ADR-0015 contract holds unchanged.
- It serves VWAP from a closed bucket only and cross-region consistency is provable.
- `flags.stale = true` means the closed bucket was missing and the API degraded to a last-trade fallback.

**`/v1/price/tip`, rolling window plus last-good price.**
- It serves VWAP over a rolling window (default 5 s, `?window_seconds=N` clamped to 1-60).
- With no trades in the window it returns the most recent trade with `price_type="last_trade"` and `window_seconds=0`, with no upper bound on that trade's age; the client reads `observed_at`.
- Cross-region consistency is not provable. `flags.stale` is always false, because both branches are in contract.

**`/v1/observations`, raw per-source.**
- It returns the latest trade per source as an array, `?source=X` narrows to one source, and `?aggregate=latest` collapses to the single latest trade.
- There is no aggregation, no cross-region guarantee, and `flags.stale` is always false.

**`flags.stale`** means the response is below its surface's baseline contract, and only `/v1/price` can fire it.

**Forex in chained rates.** `/v1/price`, `/v1/price/batch` and the SEP-40 reads bind a fiat cross to the vendor's time series, never the live rate:
- The bound bar is the one in `fx_fixings` (migration 0193) with the greatest `bar_end <= E - 3h` (hourly over daily, then highest generation) within `pricing_guard.fx_cross_max_age_hours`.
- E is the USD leg's bucket end; for fiat/fiat it is the current minute, and for a frozen leg it is the held value's bucket end.
- Before a currency's hourly series begins, the daily `fx_quotes` close at `E - 3h - 24h` binds.
- With no binding, the response is withheld with reason `fx_leg_unavailable`.
- The fixing is on the wire as `fx_rate`, `fx_as_of`, `fx_source`, `fx_resolution` and `usd_leg`; `observed_at` is the USD leg's bucket end.
- A derived fiat cross is not a last-trade fallback: its `flags.stale` is the USD leg's stale OR the FX fixing's.
- An hourly fixing is stale when it trails `E - 3h` by more than 4 h outside the weekend close `[Fri 22:00Z, Mon 02:00Z)`, and a daily fixing carries only the USD leg's stale.
- The orchestrator's triangulation (`internal/aggregate/orchestrator/triangulate.go`) reads `FXQuoteAtOrBefore`, which reads the same fixings first; a miss falls back to cached VWAP and increments `stellarindex_aggregator_fx_snap_fallback_total{leg=...}` (its alert in `deploy/monitoring/rules/aggregator.yml` fires at 30 minutes of sustained fallback dominance).
- `/v1/price/tip` uses the freshest live FX rate, and `/v1/observations` does no conversion.

**URL discipline.**
- A query parameter never changes a surface's consistency contract: a parameter that only narrows a closed-bucket read is fine, while `?freshness=tip` is prohibited.
- A request whose intent does not match the URL's contract returns 400; for example a closed-bucket-only parameter on `/v1/price/tip`.
- `/v1/price` returns one `data` object and `/v1/observations` a `data` array.

Rejected: one endpoint with a `?freshness=` parameter, loosening ADR-0015, SSE only, closed-bucket only forever, and capping the tip fallback's age.

## Invariant

- No query parameter or enum selects between consistency surfaces; `scripts/ci/lint-openapi-urls/` enforces it.
- `/v1/price` never serves an in-progress bucket and never uses a live FX rate for a fiat cross.
- A closed-surface fiat cross with no bound fixing is withheld, never served from the live rate.
- `/v1/price/tip` and `/v1/observations` never set `flags.stale`.

## Consequences

Customers choose their consistency tier by URL, which is greppable, at the price of three endpoints to document and test and one rate-looks-wrong runbook entry point per surface.
`/v1/price/tip/stream` carries the tip surface's contract and shape over SSE; `/v1/price/stream` streams closed buckets.

## Evidence

`scripts/ci/lint-openapi-urls/`, `internal/api/v1/price.go`, `price_tip.go`, `observations.go`, their tests, and `docs/reference/api-design.md` section 5.
