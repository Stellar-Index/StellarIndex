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

ADR-0051's derivation was ordered last, so any direct fiat book beat it. A single-venue book (one exchange's outage or thin hour sets the number, and only single-source buckets can freeze) is weaker evidence than a multi-venue USD VWAP converted at a vendor FX fixing.

## Decision

1. ADR-0051's composition and its flag, provenance and withheld-leg properties are unchanged. On `/v1/price`, `/v1/price/batch` and `/v1/oracle/x_last_price` the ordering is replaced by a basis rule, `Server.preferUSDAnchoredBasis`.
2. No direct fiat book: derive.
3. A direct book backed by two or more venues: serve it as observed.
4. A direct book backed by one venue: serve the derivation instead, when its USD leg has at least `basisMinUSDVenues` (2) venues and it is not stale (no fallback USD leg, no stale fixing). Otherwise serve the direct book. The rule only swaps one served value for a better-substantiated one; a derivation that misses, withholds or errors keeps the direct book.
5. A swapped row carries `usd_leg`, `fx_rate`, `fx_as_of` and `triangulated`, and no `confidence`, composite or divergence enrichment, because those score the displaced book.
6. `pricing_guard.disable_fiat_basis = true` restores direct-book-wins. It is a diagnostic kill switch, not a tuning knob.
7. `/v1/price/tip`, the series surfaces (`/v1/chart`, `/v1/price/at`), the price stream and the aggregator composite do not apply the rule (its only callers are `/v1/price`, its batch form and `/v1/oracle/x_last_price`); they keep ADR-0051 ordering. If any of them adopts a basis rule, it adopts this one, not a variant.

## Invariant

- A single-venue book yields to a multi-venue fresh derivation, and a multi-venue book does not: `TestPriceThinFiatBookYieldsToUSDAnchor`, `TestPriceMultiVenueFiatBookIsServedAsObserved`, `TestPriceThinUSDLegDoesNotDisplaceThinBook` in `internal/api/v1/usd_anchored_fiat_basis_test.go`.
- A derivation miss never costs a price: `TestPriceFiatBasisMissKeepsDirectBook`, same file.
- Closed surfaces agree: `TestPriceBatchThinFiatBookYieldsToUSDAnchor`, `TestOracleXLastPriceAppliesFiatBasis`, same file.
- A swapped row has no thin-book enrichment: `TestPriceBasisRowCarriesNoThinBookEnrichment`, same file.
- The kill switch restores the direct book: `TestPriceFiatBasisKillSwitchServesThinBook`, same file.

## Consequences

- A currency with one direct venue is priced from the aggregated USD market and says so on the wire; single-venue fiat freezes stop reaching customers when a deep USD leg exists.
- The served price carries FX fixing lag: hourly bars bound `FXFixingLag` (3h) behind the bucket (`internal/storage/timescale/fx_fixings.go`), so a real local move inside that lag is not reflected. Accepted for display pricing.
- Until tip and the series surfaces adopt the rule, a single-venue fiat pair can read differently there than on `/v1/price`.
- Costs one extra USD read and one fixing bind per request that hits a single-venue book.

## Evidence

`Server.preferUSDAnchoredBasis` in `internal/api/v1/price.go`, called from `price.go` and `oracle_sep40.go`; config `DisableFiatBasis` in `internal/config/config.go`; tests in `internal/api/v1/usd_anchored_fiat_basis_test.go`; `docs/methodology/local-currency-pricing.md`.
