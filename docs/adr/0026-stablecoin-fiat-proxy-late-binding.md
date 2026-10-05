---
adr: 0026
title: Stablecoin → fiat proxy is late-binding aggregator policy, not eager ingest normalisation
status: Accepted
date: 2026-05-10
supersedes: []
superseded_by: null
---

# ADR-0026: Stablecoin → fiat proxy is late-binding aggregator policy, not eager ingest normalisation

## Context

Most trades quote a stablecoin (USDT, USDC, EUROC), but customers ask for `XLM/fiat:USD`. Rewriting `USDT` to `USD` at ingest would hide depegs, force a second storage path for per-stablecoin pairs, and make a wrong peg uncorrectable without re-ingest.

## Decision

Trades are stored at the pair the venue emitted. The stablecoin-to-fiat mapping is applied at compute time, in the aggregator and on API paths when a request for `X/fiat:<CCY>` misses its literal pair (`tryStablecoinFiatProxy`). The peg list for crypto tickers is the compiled Go map `stablecoinFiatProxy` in `internal/aggregate/stablecoin.go`: USDT, USDC, DAI, PYUSD, USDP to USD; EURC, EUROC, EUROB to EUR; MXNe to MXN. Changing it is a code change and a redeploy. USD pegs for issuer-keyed classic assets are operator config (`trades.usd_pegged_classic_assets`), and the aggregator-side expansion is gated by `enable_stablecoin_fiat_proxy`.

On `/v1/price` the fallback order is the Redis VWAP fallback (including triangulated prices, ADR-0019), then the stablecoin proxy, then fiat cross-rates; if none resolves the answer is no-data, never a fabricated price.

## Invariant

Decoders never rewrite a stablecoin code to fiat, and the crypto-ticker proxy map lives only in `internal/aggregate/stablecoin.go`. `TestStablecoinCodes_InSyncWithCanonical` pins that map to `canonical.StablecoinCodes` in both directions.

## Consequences

- Depegs stay visible as divergence between literal pairs, and `XLM/credit:USDC:…` returns exactly that pair.
- Fixing a peg policy corrects all history on the next response.
- Each API surface that resolves a `fiat:*` quote must call the proxy itself after its primary lookup misses; accepted over one middleware that would couple every response shape to it.
- `XLM/fiat:USD` is a pseudo-pair that matches no single set of trades; per-surface flags and OpenAPI say so.
- Operator response to a depeg is `docs/operations/runbooks/divergence.md`.

## Evidence

- `internal/aggregate/stablecoin.go`, `stablecoin_test.go`.
- Proxy callers: `internal/api/v1/price.go`, `price_tip.go`, `chart.go`, `twap.go`, `oracle_sep40.go`, `ohlc_fiat_combine.go`, among others; `internal/aggregate/orchestrator/orchestrator.go`.
