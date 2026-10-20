// Package sac_balances is the Stellar Asset Contract balance observer (ADR-0022): a
// LedgerEntryChange decoder emitting one Observation per ContractData change on a watched SAC whose
// key is the SEP-41 balance shape `Vec(Symbol("Balance"), Address(holder))`.
//
// The entry names the SAC's contract id, not its classic asset, so the contract → asset_key map
// comes from `[supply.sac_wrappers]`. A balance is `Map({amount, authorized, clawback})` or a bare
// i128; scval.SEP41BalanceAmount takes either, and any other shape is dropped as a decode error.
// Because the asset comes from config, a Removed entry is decodable (unlike claimable_balances and
// liquidity_pools) and emits IsRemoval=true with Balance 0. An evicted persistent entry emits
// nothing: it is archived and restorable, so the balance stays served.
package sac_balances
