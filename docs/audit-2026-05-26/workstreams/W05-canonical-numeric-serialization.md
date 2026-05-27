# W05 — Canonical identity, numeric safety, serialization

## Scope

Every type in `internal/canonical/`. ADR-0003 (i128/u128
NUMERIC). SCVal helpers in `internal/scval/`. JSON boundary.
NEW: sorobanevents Capture/Reconstruct round-trip and
scval.EncodeArgsAsScVec / DecodeScVecToArgs inverses.

## Inputs

- `internal/canonical/` (asset, asset_crypto, asset_fiat,
  asset_rwa, amount, pair, oracle, strkey, trade)
- `internal/scval/scval.go` + tests
- `internal/sources/sorobanevents/{events,reconstruct}.go`
- ADR-0003 invariant text

## Checks

| # | Check | Method |
| --- | --- | --- |
| W05.1 | `canonical.Amount` is `*big.Int`-backed; never int64 | type def |
| W05.2 | Every `xdr.Int128Parts` parse site uses scval helpers (no `int64(x.Lo)`) | grep |
| W05.3 | `canonical.Pair` parse + format round-trip | tests |
| W05.4 | `canonical.Asset` kind switch covers fiat / stablecoin / crypto / RWA / native / contract / fiat-proxy | code |
| W05.5 | `canonical.Strkey` validates G... / C... / M... / SEP-11 codes | tests |
| W05.6 | JSON boundary: amounts as strings, not numbers (no IEEE 754 loss) | handler audit |
| W05.7 | NEW: `sorobanevents.Capture` is total + tested | tests |
| W05.8 | NEW: `sorobanevents.Reconstruct` is loss-free for the fields decoders read | `reconstruct_test.go` |
| W05.9 | NEW: `scval.EncodeArgsAsScVec` ↔ `DecodeScVecToArgs` are inverses (byte-perfect within ScVal encoding) | round-trip test |
| W05.10 | Cache key construction in `internal/cachekeys/` is stable under quoting / casing / order | tests |
| W05.11 | `canonical/discovery` package writes only via a documented sink interface (no direct DB) | grep |
| W05.12 | Crypto-ticker representation per ADR-0014 (no name collisions) | tests + per-ticker check |

## Closure criteria

Every canonical type audited. Findings on any truncation, lossy
encoding, ambiguous parse, or cache-key drift.
