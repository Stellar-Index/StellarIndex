# `internal/sources/rozo`

Decoder for the **Rozo intent-bridge protocol** on Stellar.

## Scope (2026-05-20)

Currently scoped to **v1 Payment** — the only mainnet-live Rozo
contract at the time of writing. v2 Forwarder + IntentBridge and
the newer rozo-intents schema are pre-mainnet and documented for
follow-up implementation in
[`docs/architecture/rozo-stellar-coverage.md`](../../../docs/architecture/rozo-stellar-coverage.md).

| Variant | Status | Decoder support |
|---|---|---|
| v1 Payment (`CAC5SKP5…IYRL`) | mainnet | this package |
| v2 Forwarder | design-stage | follow-up |
| v2 IntentBridge | design-stage | follow-up |
| rozo-intents (v2.x?) | status unclear | follow-up |

## What this package emits

Two canonical Go types — `Payment` and `Flush` — corresponding
1:1 to the two `#[contractevent]` types in
[`v1/stellar/payment/src/lib.rs`](https://github.com/RozoAI/rozo-intents-contracts/blob/main/v1/stellar/payment/src/lib.rs):

- **`Payment`** — emitted on `pay(from, amount, memo)`.
  Topic `(symbol_short!("payment"), from: Address)`. Body
  `{ from, destination, amount, memo }`.
- **`Flush`** — emitted on `flush(token)` (admin sweep).
  Topic `(symbol_short!("flush"),)`. Body
  `{ token, destination, amount }`.

## Wiring status

This package is **not yet registered** in
`internal/sources/external/registry.go`. Registration follows the
storage-shape decision documented in
[`docs/architecture/rozo-stellar-coverage.md`](../../../docs/architecture/rozo-stellar-coverage.md)
§Storage shape — `bridge_events` shared table with CCTP vs
`rozo_events` separate.

Once storage lands:

1. Add a `consumer.go` implementing `consumer.Source` that
   classifies events, decodes via `DecodePayment` / `DecodeFlush`,
   and writes to the chosen storage shape.
2. Add a `dispatcher_adapter.go` per the existing source
   convention (see `internal/sources/soroswap/dispatcher_adapter.go`).
3. Register `"rozo"` in `internal/sources/external/registry.go`:
   `Class: ClassBridge, IncludeInVWAP: false, DefaultWeight: 0,
   Paid: false, BackfillAvailable: true, BackfillSafe: false`
   — flip `BackfillSafe: true` only after the WASM-history audit
   lands at `docs/operations/wasm-audits/rozo.md`.
4. Add the source to `cmd/ratesengine-indexer/main.go`'s dispatch
   chain (mirrors how soroswap is wired).

## Tests

`decode_test.go` covers:

- `Classify` — payment / flush / unknown-symbol / empty-topic.
- `DecodePayment` — happy path with all four fields populated;
  ADR-0003 large-i128 round-trip (locks the *big.Int → string
  precision invariant against the int64-truncation bug-class);
  missing-field surfaces `ErrMalformedBody`; wrong top-level
  ScVal kind early-fails.
- `DecodeFlush` — happy path + missing-field.
- Topic-symbol encoding stability — re-encoded bytes match the
  package-init constants. Drift here means `scval.MustEncodeSymbol`
  changed and every `Classify` call would silently miss matches.

## References

- Architecture doc: [`docs/architecture/rozo-stellar-coverage.md`](../../../docs/architecture/rozo-stellar-coverage.md)
- Upstream source: https://github.com/RozoAI/rozo-intents-contracts
- v1 contract on StellarExpert:
  https://stellar.expert/explorer/public/contract/CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL
- Class taxonomy: `internal/sources/external/framework.go` `ClassBridge`
- Sister sources (event-based decoders): `internal/sources/soroswap/`,
  `internal/sources/aquarius/`, `internal/sources/phoenix/`
