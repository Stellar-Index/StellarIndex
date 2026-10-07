// Package canonical defines the types every other package depends on
// (Trade, Price, Asset, Pair, Amount); changing their shape ripples
// through the whole repository, so a new field needs an ADR.
//
// Invariants:
//   - Amounts are *big.Int and never truncate to int64 (ADR-0003).
//   - An asset has exactly one canonical form, one of seven shapes:
//     native; classic (code, issuer); soroban (an on-chain C-address);
//     fiat:USD (ADR-0010); crypto:BTC, a ticker with no contract
//     (ADR-0014); rwa:BENJI (ADR-0028); and raw:<symbol>, an unmapped
//     oracle symbol kept at the record layer only.
//   - Pair is directional: Pair{A,B} != Pair{B,A}.
//   - Timestamps are UTC; storage keeps Unix seconds at ledger
//     granularity, finer where the upstream event supplies it.
package canonical
