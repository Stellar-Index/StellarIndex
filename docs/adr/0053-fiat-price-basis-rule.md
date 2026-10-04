---
adr: 0053
title: Fiat price basis rule — a single-venue fiat book yields to the USD-anchored derivation
status: Accepted
date: 2026-10-03
supersedes: [0051]
superseded_by: []
---

# ADR-0053: Fiat price basis rule

## Context

ADR-0051 composes `price(asset, CCY) = price(asset, USD) × rate_usd[CCY]`
and orders it last: **any** observed market for the pair beats the
derivation. That ordering treats every direct fiat book as better
evidence than the derivation, whatever stands behind it.

It is not. A direct `XLM/CHF` book can be one exchange's order book,
while `XLM/USD` is a VWAP across many venues and `USD/CHF` is a vendor
FX fixing. The single-venue book is the weaker claim: one venue's
outage, mispricing or thin hour sets the served number, and it is the
population the anomaly freeze keeps catching (a single-source bucket is
the only kind that can freeze). Serving it over a better-substantiated
derivation is the opposite of "never make a number less true".

## Decision

ADR-0051's composition and its three binding properties carry forward
unchanged: derived values are flagged `triangulated`, provenance names
both legs (`sources`, `usd_leg`, `fx_rate`, `fx_as_of`), and a withheld
USD leg is never laundered through FX. This ADR replaces its ordering
rule with a **basis rule** on the closed surfaces (`/v1/price`,
`/v1/price/batch`, `/v1/oracle/x_last_price`):

1. No direct fiat book: derive (unchanged).
2. A direct fiat book backed by **two or more venues**: serve it as
   observed (unchanged).
3. A direct fiat book backed by **one venue**: serve the USD-anchored
   derivation instead, when that derivation
   - rests on a USD leg backed by at least two venues,
   - is not stale (neither a fallback USD leg nor a stale fixing), and
   - binds its FX leg at the USD bucket's end.

   Otherwise serve the direct book. The rule only ever swaps one
   served value for a better-substantiated one; it never turns a 200
   into a 404, 503 or withheld answer that the direct book would not
   have produced. (A USD leg that is frozen is the one exception, and
   it is governed exactly as `/v1/price?quote=fiat:USD` would govern
   it.)

A row served by rule 3 carries no `confidence`, `confidence_factors`,
composite or divergence flags: those are keyed on the requested pair and
score the book that was displaced, not the value served.

`pricing_guard.disable_fiat_basis = true` restores ADR-0051 ordering.
It is a kill switch for diagnosis, not a tuning knob.

## Consequences

- **Positive:** a currency with one direct venue is priced from the
  aggregated USD market rather than one exchange, and says so on the
  wire. Freezes on single-venue fiat books stop reaching customers
  when a deep USD leg exists.
- **Negative:** the served CHF (or any single-venue fiat) price now
  carries FX-fixing lag — the fixing is hourly, bound three hours
  behind the bucket, and pauses over market closes. A real local
  market move inside that lag is not reflected. Accepted: for display
  pricing, an aggregated USD price converted at an authoritative
  fixing is the stronger claim than one venue's book.
- **Operational impact:** one extra USD price read and one fixing bind
  per request that hits a single-venue fiat book. No new storage, feed
  or alert.
- **Scope:** `/v1/price/tip` (live), the series surfaces
  (`/v1/chart`, `/v1/price/at`), the price stream and the aggregator's
  composite still follow ADR-0051 ordering. Until they adopt the same
  basis, a single-venue fiat pair can read differently on those
  surfaces than on `/v1/price`; each follow-up applies this rule, not a
  variant of it. `/v1/oracle/lastprice` is USD-quoted, so the rule
  never applies to it.

## Alternatives considered

1. **Keep direct-market-always-wins (ADR-0051)** — rejected: it ranks
   one venue above an aggregate of many plus an authoritative fixing.
2. **Always derive non-USD fiat, ignore direct books** — rejected: a
   multi-venue EUR or GBP book is real, deep evidence in that currency
   and carries no FX lag; discarding it makes the number less true.
3. **Blend the direct book with the derivation** — rejected: the
   result is neither an observation nor a disclosed derivation, and no
   `flags` value could describe it honestly.
4. **Gate on volume rather than venue count** — rejected for now: the
   substance gate already owns volume, and venue count is the property
   the freeze and `single_source` already use, so the three agree on
   what "one venue" means.

## References

- Supersedes: [0051](0051-usd-anchored-fiat-derivation.md).
- Related: [0015](0015-last-closed-bucket-rate-serving.md),
  [0019](0019-anomaly-response-and-confidence-scoring.md),
  [0003](0003-i128-no-truncation.md) (exact `big.Rat` composition).
- Implementation: `Server.preferUSDAnchoredBasis` in
  `internal/api/v1/price.go`.
- Guards: `internal/api/v1/usd_anchored_fiat_basis_test.go`.
- Methodology: `docs/methodology/local-currency-pricing.md`.
