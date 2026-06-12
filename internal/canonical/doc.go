// Package canonical defines the core types every other package in
// Stellar Index depends on — Trade, Price, Asset, Pair, and the
// precision-safe Amount wrapper over *big.Int.
//
// # Why this is package zero
//
// Every ingestion source, every aggregation result, every API
// response passes through these types. They are the stability
// boundary of the project: if these shapes change, the whole
// repository reacts. They land in Week 1 Day 1 of the delivery
// per docs/discovery/delivery-plan.md §Week 1.
//
// # Invariants
//
//   - Amount values are ALWAYS *big.Int. They never truncate to
//     int64. See ADR-0003 and docs/discovery/decisions.md §i128.
//   - Asset identity is unambiguous, with five canonical shapes:
//     native (XLM), classic ((code, issuer) tuple), soroban
//     (single C-address SEP-41 contract), fiat (off-chain ISO-4217
//     code, wire form fiat:USD — ADR-0010), and crypto (off-chain
//     global ticker, wire form crypto:BTC — ADR-0014). The crypto
//     shape is distinct from soroban: soroban requires a real
//     on-chain C-address, crypto:BTC does not. Two different
//     representations of the same underlying asset MUST round-trip
//     through a single canonical form.
//   - Pair is a unidirectional (base, quote) ordering. Pair
//     equality is strict; Pair{A,B} != Pair{B,A}.
//   - Timestamps are UTC; storage is Unix seconds (u64) at ledger
//     granularity, higher precision where the upstream event
//     supplies it.
//
// # Extending
//
// Adding a field to any of these types is a load-bearing change.
// Propose an ADR (docs/adr/), co-ordinate with CODEOWNERS, land
// the field + its documentation + its fixture in the same PR.
// Never add a field and "document it later."
//
// # See also
//
//   - ADR-0003 (i128 no-truncation invariant)
//   - ADR-0005 (monorepo, single Go module)
//   - ADR-0010 (off-chain fiat representation)
//   - ADR-0014 (crypto-ticker representation)
//   - docs/discovery/decisions.md
//   - docs/discovery/notes/sep-41-token-events.md (amount shapes
//     from Soroban token events)
package canonical
