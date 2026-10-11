// Package trustlines is the TrustlineEntry observer (ADR-0022): a
// LedgerEntryChange decoder emitting one Observation per change to a
// trustline whose asset is in `[supply] watched_classic_assets`
// (CODE:ISSUER); others are skipped before decode. The sink writes
// `trustline_observations`, which supply.StorageClassicSupplyReader sums
// as Algorithm 2's trustline component.
//
// Classic credits only: native XLM is the accounts observer and pool shares the liquidity_pools
// LP-reserve observer.
package trustlines
