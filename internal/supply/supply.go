package supply

import (
	"math/big"
	"time"
)

// Basis identifies which policy produced a [Supply] value. Stable
// string values appear on the API response and in metric labels;
// renaming is a wire break.
type Basis string

const (
	// BasisXLMSDFReserveExclusion — Algorithm 1: native XLM with
	// SDF reserve accounts subtracted from total to yield
	// circulating.
	BasisXLMSDFReserveExclusion Basis = "xlm_sdf_reserve_exclusion"

	// BasisXLMSDFReserveExclusionStatic — Algorithm 1 where the reserve
	// balances came from the operator's dated static map
	// ([ConfigReserveBalanceReader]) because the live account observer
	// could not answer. Distinct from BasisXLMSDFReserveExclusion so a
	// consumer can tell a hand-entered snapshot from a per-ledger read;
	// such a snapshot carries no freshness anchor, so
	// strict_freshness_required refuses it.
	BasisXLMSDFReserveExclusionStatic Basis = "xlm_sdf_reserve_exclusion_static"

	// BasisXLMTotalOnly — Algorithm 1 with NO reserve accounts
	// configured: circulating == total (nothing was excluded). Kept
	// distinct from BasisXLMSDFReserveExclusion so the wire never
	// claims an SDF exclusion that didn't happen — a
	// circulating==total XLM figure labelled "sdf_reserve_exclusion"
	// silently overstates circulating supply (and market cap) by the
	// unsubtracted ~18-19B SDF-held stroops. Configure
	// sdf_reserve_accounts + balances to get the real circulating.
	BasisXLMTotalOnly Basis = "xlm_total_only"

	// BasisIssuerExclusion — Algorithm 2 default: classic credit
	// assets where circulating excludes the issuer's own balance.
	BasisIssuerExclusion Basis = "issuer_exclusion"

	// BasisAdminExclusion — Algorithm 3 default: SEP-41 tokens
	// where circulating excludes the admin account/contract balance.
	// Only emitted when a non-zero admin balance was ACTUALLY
	// subtracted; see BasisSEP41TotalOnly for the other case.
	BasisAdminExclusion Basis = "admin_exclusion"

	// BasisSEP41TotalOnly — Algorithm 3 with NOTHING excluded:
	// circulating == total because the admin balance came back zero
	// and no per-asset locked-set was configured. Kept distinct from
	// BasisAdminExclusion so the wire never claims an admin exclusion
	// that didn't happen (the same reasoning as
	// BasisXLMTotalOnly) — a circulating==total SEP-41 figure
	// labelled "admin_exclusion" tells a consumer the issuer's own
	// holdings were netted out when they were not, overstating
	// circulating supply and market cap by whatever the admin holds.
	//
	// This is the DEFAULT reading today, not an edge case:
	// [StorageSEP41SupplyReader] hardcodes AdminBalance to zero
	// because v1 doesn't track `set_admin`, and its docstring
	// directs operators to put the admin's address in the per-asset
	// LockedSet instead. Doing so yields BasisOverride — so an
	// operator can tell a configured token from an unconfigured one
	// straight off the basis field.
	BasisSEP41TotalOnly Basis = "sep41_total_only"

	// BasisOverride — operator-configured override beat the
	// algorithm-default policy. Used for both circulating
	// (extended locked-set) and max_supply.
	BasisOverride Basis = "override"

	// BasisSEP1DeclaredMax — the SEP-1 [[CURRENCIES]] max_supply
	// overlay ([Overlay]) populated max_supply from the issuer's
	// stellar.toml (max_number, falling back to fixed_number). The
	// cap is issuer-SELF-DECLARED — a display value, not on-chain
	// enforced (ADR-0011 §Algorithm 2/3 max_supply precedence step
	// 2). Carried on [Supply.MaxSupplyBasis] only: total/circulating
	// still come from the snapshot's own algorithm, which [Supply.Basis]
	// keeps naming.
	BasisSEP1DeclaredMax Basis = "sep1_declared_max"

	// BasisSEP41LakeFlows — Algorithm 3, lake-derived: a SEP-41 token's raw
	// on-chain total (Σmint−Σburn−Σclawback) summed live over the certified
	// ClickHouse lake's stellar.supply_flows (no rollup refresh needed — see
	// internal/storage/clickhouse/supply_flows.go). Used when no LCM observer
	// snapshot exists — i.e. for SEP-41 tokens not on an operator watch-list,
	// which is the vast majority. NOT admin-excluded (that needs per-contract
	// admin tracking the flow sum doesn't carry); total == circulating here.
	BasisSEP41LakeFlows Basis = "sep41_lake_flows"

	// BasisClassicLakeFlows — a CLASSIC asset's raw on-chain total
	// (Σmint−Σburn−Σclawback) summed live over stellar.supply_flows for the
	// asset's derived Stellar Asset Contract. The classic sibling of
	// [BasisSEP41LakeFlows]; a flow does not know where the tokens came to rest,
	// so one sum covers trustlines, claimable balances, LP reserves and SAC-held
	// balances.
	//
	// It is an UPPER reading, not a certified one: the completeness check
	// (clickhouse.TokenSupply.Incomplete) fires only when the net goes NEGATIVE
	// (under-seeded mints). A replayed historical mint with no matching burn
	// yields a too-LARGE total nothing detects (measured against Horizon: PHO
	// +156.79% from one replayed issuance). So it ranks BELOW a direct supply
	// observation and above only the trustline sum.
	//
	// NOT exclusion-netted: total == circulating here, as for
	// [BasisSEP41LakeFlows]; an observation basis ([BasisIssuerExclusion],
	// [BasisOverride]) subtracts the excluded sets.
	BasisClassicLakeFlows Basis = "classic_lake_flows"

	// BasisClassicTrustlineSum — a CLASSIC asset's supply summed from
	// TRUSTLINE BALANCES ONLY. It is a LOWER BOUND and nothing more: the
	// lake's current-state projection stamps its `asset` column for
	// trustlines alone, so this reading is blind by construction to the
	// other three holding domains (claimable balances, liquidity-pool
	// reserves, SAC contract_data balances). Every trustline balance was
	// minted, which is what makes it a floor rather than an estimate, and
	// the floor is what keeps an under-seeded flow sum from being
	// published as a smaller truth.
	//
	// NOT exclusion-netted either: every holder's trustline balance is
	// summed, locked-set accounts included, so total == circulating here
	// and it floors the TOTAL, not an issuer/locked-excluded circulating.
	BasisClassicTrustlineSum Basis = "classic_trustline_sum"

	// BasisContractStorageBalances — a Soroban token's supply summed from the
	// per-holder BALANCE LEDGER ENTRIES in its contract storage
	// (`Balance(Address) → i128`), not its event log. Produced by
	// internal/storage/clickhouse.ContractStorageSupply.
	//
	// A DIFFERENT BASIS, not a better reading of the same one: every other basis
	// accumulates ISSUANCE, this one measures DISTRIBUTION. Where the event log
	// is complete they agree exactly (measured on pubnet for EUTBL, USTBL,
	// deJTRSY vs [BasisSEP41LakeFlows]). It exists for tokens that emit NO
	// SEP-41 events: they are ABSENT from stellar.supply_flows, which sums to a
	// confident zero rather than a visible gap.
	//
	// The two readings are NEVER SUMMED (double-count); where a token has both,
	// this basis supersedes the event reading, since a level cannot be made wrong
	// by missing history.
	//
	// A figure on this basis is a LOWER BOUND and carries the
	// circulating_supply_lower_bound flag: it misses entries outside the lake's
	// current-state projection. Archived (TTL-lapsed) balances are still summed,
	// since the lake never records an eviction.
	BasisContractStorageBalances Basis = "contract_storage_balances"

	// BasisNoMetadata — we don't have a defensible value for
	// at least one of total / circulating / max. Per ADR-0011
	// "we don't fabricate" — the corresponding field is nil.
	BasisNoMetadata Basis = "no_metadata"
)

// String returns the basis as a string for log lines + metric
// labels. Equivalent to a direct cast; provided for fluency.
func (b Basis) String() string { return string(b) }

// LowerBound reports whether a figure carrying this basis is a provable
// FLOOR rather than a complete reading of what exists.
//
// Two bases are floors, and they are blind in different ways.
// [BasisClassicTrustlineSum] misses holding DOMAINS: the lake's
// current-state projection stamps its asset column for trustlines alone,
// so claimable balances, liquidity-pool reserves and SAC-held contract
// balances are absent by construction. Measured across the served set,
// the omitted share was 89.5% of EURMTL, 73.3% of PYUSD, 64.5% of SHX
// and 15.4% of USDC. [BasisContractStorageBalances] misses balance entries the
// lake's current-state projection never captured, such as one dormant since
// before its coverage began.
//
// Every other basis in this vocabulary either covers all four holding
// domains at once (the flow sums, which do not know where a token came to
// rest) or is an observer's own certified reading, and marking those as
// floors would carry exactly as much information as marking none of them.
//
// The wire flag is DERIVED from the basis rather than stored beside it,
// so a row can never publish a basis that says one thing and a marker
// that says another.
func (b Basis) LowerBound() bool {
	switch b {
	case BasisClassicTrustlineSum, BasisContractStorageBalances:
		return true
	default:
		return false
	}
}

// Supply is the wire-shape result of a supply derivation. Every
// per-algorithm computer in this package returns one of these.
//
// Field semantics:
//
//   - AssetKey is "XLM" for native, "CODE:G..." for classic, or the
//     bare contract id ("C...") for SEP-41 Soroban tokens. Matches
//     the asset_supply_history primary-key column.
//   - TotalSupply / CirculatingSupply are never nil; the algorithms
//     always have a defensible value (zero is a valid answer for an
//     asset that has been fully burned).
//   - MaxSupply is nil when no defensible value exists — uncapped
//     classic issuers with no SEP-1 declaration and no operator
//     override produce nil here. Per ADR-0011 we don't fabricate;
//     consumers handle nil explicitly (the API layer marshals it as
//     JSON null).
//   - Basis identifies which policy produced this Supply. Surfaced
//     on API responses so consumers know whether to trust the
//     absolute number.
//   - MaxSupplyBasis is set only when MaxSupply came from somewhere
//     other than the policy Basis names — today the SEP-1 overlay.
//     Empty means MaxSupply (if any) is covered by Basis.
//   - LedgerSequence + ObservedAt mark the ledger this snapshot
//     reflects. UTC; ledger close time, not write time.
type Supply struct {
	AssetKey          string
	TotalSupply       *big.Int
	CirculatingSupply *big.Int
	MaxSupply         *big.Int
	Basis             Basis
	MaxSupplyBasis    Basis
	LedgerSequence    uint32
	ObservedAt        time.Time

	// MinComponentLedger is the oldest ledger any per-component
	// observation contributing to this snapshot was last updated
	// at. The refresher uses
	// this to detect "snapshot stamped at fresh ledger N but
	// constructed from per-component observations as old as M"
	// and reject snapshots where (N - M) exceeds the operator-
	// configured stale-component threshold.
	//
	// Zero = "computer didn't populate" (non-storage-backed
	// computers like the static-config XLM reader). The refresher
	// treats zero as "no freshness signal" and falls through to the
	// max-ledger semantics, as for deployments that haven't wired
	// storage-backed readers.
	MinComponentLedger uint32

	// SACWrappedStroops is Algorithm 2's SACWrapped component —
	// [ClassicSupplyComponents.SACWrapped], the amount of this classic
	// asset currently escrowed inside its Stellar-Asset-Contract
	// wrapper — broken out of TotalSupply (which still includes it as
	// one of its four addends) and persisted alongside it
	// (asset_supply_history.sac_wrapped_stroops, migration 0117).
	//
	// It exists so [CrossCheckSubsetBound] can run the REAL subset
	// compare: this component and the SAC's own Algorithm-3 total
	// measure the SAME quantity via independent data paths — a ledger-entry
	// snapshot sum here versus an event-flow sum there — so
	// SACWrapped > sac_total is impossible under correct accounting.
	// Comparing the folded TotalSupply against sac_total can only
	// catch the opposite direction.
	//
	// nil = "this snapshot recorded no SACWrapped component", the
	// unchecked state. Only [ClassicComputer.Compute] populates
	// it; Algorithm 1 (XLM), Algorithm 3 (SEP-41) and the static
	// text-file computer have no such component and leave it nil, as do
	// rows written before migration 0117. Consumers MUST NOT read nil
	// as zero — zero is a meaningful value (an asset with no SAC
	// deployment genuinely wraps nothing) and the subset bound
	// 0 <= sac_total holds vacuously, so treating nil as zero would
	// report a green check that verified nothing.
	SACWrappedStroops *big.Int
}
