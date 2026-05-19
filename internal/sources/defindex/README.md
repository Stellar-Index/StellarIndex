# defindex source

Decoder for the **Blend autocompound strategy** contracts that back
paltalabs' [DeFindex](https://github.com/paltalabs/defindex) vaults
on Stellar mainnet.

> **Corrected 2026-05-19.** This package previously targeted a
> fictional `("DeFindexVault", …){depositor, amounts:Vec<i128>,
> df_tokens_minted}` schema taken from paltalabs/defindex tag
> `1.0.0`. **Mainnet never deployed that.** The contract addresses
> we watch run Blend *strategy* code (deployed WASM `11329c24…988`)
> and emit a much simpler schema, verified from real on-chain LCM
> via `ratesengine-ops scan-soroban-events`. See
> `docs/operations/wasm-audits/defindex.md` "Audit result" for the
> full evidence trail (#28).

## What it decodes (verified on-chain)

| topic | body | example |
|---|---|---|
| `("BlendStrategy","deposit")` | `ScvMap{ from: Address, amount: i128 }` | L57,056,389 |
| `("BlendStrategy","withdraw")` | `ScvMap{ from: Address, amount: i128 }` | recent live |

- `topic[0]` is `ScvString("BlendStrategy")` (13 chars > the 9-char
  `symbol_short!` cap → String, same pattern as `"SoroswapPair"`).
- `from` is the caller moving capital — usually the vault/router
  **contract** (a C-strkey), occasionally a plain account
  (G-strkey). `scval.AsAddressStrkey` renders both.
- `amount` is the underlying-asset delta (`i128`, never truncated —
  ADR-0003).

These are **flow-attribution** events, not price discovery — a
strategy deposit/withdraw moves capital at NAV, it doesn't set a
market price. Registered `Class: ClassRouter`; never a VWAP
contributor.

## Why a separate decoder

Standard event-based dispatcher (`Decoder` interface, like
`soroswap` / `aquarius` / `comet`). Dispatch is **by topic**: any
contract emitting the `("BlendStrategy",deposit|withdraw)` topic is
matched — the comet/aquarius shared-emitter topology — so we cover
*every* Blend autocompound strategy instance, not a hand-curated
set. (The previous revision filtered on a mislabeled 3-contract
"vault" set.)

## Files

```
events.go              — source name, topic prefix bytes, event symbols, StrategyFlow
decode.go              — classify() + decodeFlow() → StrategyFlow
dispatcher_adapter.go  — implements dispatcher.Decoder (topic-matched)
consumer.go            — Sink for the pipeline-side log emit
README.md              — this file
```

## Phase A scope (what ships now)

- Decode `("BlendStrategy","deposit"|"withdraw")` across all
  emitters; emit a structured log line per event
  (contract / direction / from / amount).
- No persist yet — no `routed_via` tag, no `aggregator_exposures`
  rows. `BackfillSafe` stays `false` until live decoding is
  verified producing rows on r1 and the WASM is re-audited against
  the real deployed hash `11329c24…988`.

## Phase B follow-ups

1. **`trades.routed_via` tagging.** When the same tx contains a
   Blend (`("Pool","supply")`) or Soroswap leg, tag those trades
   with the strategy attribution.
2. **Exposure ticker.** Periodic worker writing
   `aggregator_exposures` rows from per-strategy on-chain state.
3. **`harvest` / keeper-admin events.** Decode the autocompound
   `harvest` event (yield realisation) for APY attribution.
4. **End-user attribution.** When `from` is a vault contract,
   correlate with the same-tx vault event to recover the end-user.
5. **Source rename.** `defindex` → e.g. `blend-strategy` (more
   honest; distinct from the existing `blend` pool source). A
   product-taxonomy decision deferred so registry / genesis /
   status-page keys stay stable for now.

## Sources

- Event shapes: **real mainnet LCM**, captured via
  `ratesengine-ops scan-soroban-events` (2026-05-19).
- Deployed WASM: `11329c2469455f5a3815af1383c0cdddb69215b1668a17ef097516cde85da988`
  (Blend strategy code; walk-confirmed single hash, zero upgrades).
- WASM audit: `docs/operations/wasm-audits/defindex.md`.
