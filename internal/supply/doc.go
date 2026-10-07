// Package supply derives total, circulating and max supply for every
// indexed asset class (ADR-0011), one algorithm per class:
//
//   - [XLMComputer]: native XLM is fixed at 50,001,806,812 XLM (network
//     vote, 2019); only the SDF-reserve exclusion moves circulating.
//   - [ClassicComputer] (ADR-0022): Σ trustline + Σ claimable +
//     Σ LP reserve + Σ SAC-wrapped, from the per-class observers.
//   - [SEP41Computer] (ADR-0023): Σ mint − Σ burn − Σ clawback from the
//     sources/sep41_supply observer.
//
// Amounts are *big.Int end to end (ADR-0003); JSON encoding happens at
// the API boundary. [Policy] carries the SDF reserve accounts, per-asset
// [LockedSet] overrides and max-supply overrides; [Basis] names the
// policy behind a result. [Refresher] snapshots into
// `asset_supply_history`; [CrosscheckRefresher] compares SAC-wrapped
// classic against SEP-41 supply, and [WriteSnapshotTextfile] feeds the
// `supply-snapshot` timer.
package supply
