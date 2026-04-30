// Package sac_balances is the canonical Stellar-Asset-Contract
// (SAC) balance observer per ADR-0022. Plugs into the
// dispatcher's LedgerEntryChange hook (#297) and emits one
// Observation per ContractData-delta on a watched SAC wrapper
// contract whose Key matches the SEP-41 balance shape
// `Vec(Symbol("Balance"), Address(holder))`.
//
// # Operator config
//
// SAC observation needs a contract → asset_key map: the on-chain
// entry carries the SAC's contract id (C-strkey) but not the
// underlying classic asset's identity. Operators supply that
// mapping, typically derived from the SAC contract's
// deterministic deployment (each classic asset has exactly one
// SAC wrapper per network). PR 5/5 surfaces this as
// `[supply.sac_wrappers]` in operator TOML.
//
// # Balance value variants
//
// The wire shape of a SEP-41 balance value varies by contract:
//
//   - Native SAC (host-implemented) stores
//     `Map({amount: i128, authorized: bool, clawback: bool})`.
//   - Some custom token contracts store a bare `i128`.
//
// extractBalanceAmount tries both — i128 first, then map-with-
// amount-field. Other shapes return an error and the observation
// is dropped (counted as a decode error in dispatcher Stats).
//
// # Removed-variant
//
// SEP-41 contracts typically don't remove balance entries (they
// set the balance to zero instead). When a contract DOES remove
// the entry, the LedgerKey carries only contract+key — the
// asset_key still derives from the operator's contract→asset
// map, so unlike claimable_balances + liquidity_pools, Removed
// is decodable. Match returns true for Removed; Decode emits an
// IsRemoval=true observation with Balance=0.
package sac_balances
