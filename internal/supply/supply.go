package supply

import (
	"math/big"
	"time"
)

// Basis identifies which policy produced a [Supply] value; the strings appear on the API and in metric labels, so renaming one is a wire break.
type Basis string

const (
	// BasisXLMSDFReserveExclusion is Algorithm 1: native XLM minus SDF reserve accounts.
	BasisXLMSDFReserveExclusion Basis = "xlm_sdf_reserve_exclusion"

	// BasisXLMSDFReserveExclusionStatic is Algorithm 1 with reserves from the operator's static map
	// ([ConfigReserveBalanceReader]). It has no freshness anchor, so strict_freshness_required refuses it.
	BasisXLMSDFReserveExclusionStatic Basis = "xlm_sdf_reserve_exclusion_static"

	// BasisXLMTotalOnly is Algorithm 1 with no reserve accounts configured (circulating == total). Kept
	// distinct so the wire never claims an SDF exclusion that did not happen.
	BasisXLMTotalOnly Basis = "xlm_total_only"

	// BasisIssuerExclusion is Algorithm 2: classic credit assets, circulating excludes the issuer balance.
	BasisIssuerExclusion Basis = "issuer_exclusion"

	// BasisAdminExclusion is Algorithm 3: SEP-41 tokens, circulating excludes the admin balance. Emitted
	// only when a non-zero admin balance was actually subtracted (else BasisSEP41TotalOnly).
	BasisAdminExclusion Basis = "admin_exclusion"

	// BasisSEP41TotalOnly is Algorithm 3 with nothing excluded, so the wire never claims an exclusion
	// that did not happen. [StorageSEP41SupplyReader] hardcodes AdminBalance to zero; operators put the
	// admin in the per-asset LockedSet instead, which yields BasisOverride.
	BasisSEP41TotalOnly Basis = "sep41_total_only"

	// BasisOverride means an operator override beat the default policy, for circulating (extended
	// locked-set) or max_supply.
	BasisOverride Basis = "override"

	// BasisSEP1DeclaredMax means the SEP-1 [[CURRENCIES]] overlay ([Overlay]) populated max_supply from
	// stellar.toml. The cap is issuer-SELF-DECLARED, a display value not enforced on-chain (ADR-0011).
	// Carried on [Supply.MaxSupplyBasis] only; [Supply.Basis] keeps naming the total's algorithm.
	BasisSEP1DeclaredMax Basis = "sep1_declared_max"

	// BasisSEP41LakeFlows is Algorithm 3 summed over the lake's stellar.supply_flows
	// (Σmint−Σburn−Σclawback) when no LCM observer snapshot exists. Not admin-excluded: total == circulating.
	BasisSEP41LakeFlows Basis = "sep41_lake_flows"

	// BasisClassicLakeFlows is a classic asset's Σmint−Σburn−Σclawback over stellar.supply_flows for its
	// SAC. An upper reading: a replayed mint with no burn inflates the total undetected, so it ranks
	// below a direct observation. Not exclusion-netted: total == circulating.
	BasisClassicLakeFlows Basis = "classic_lake_flows"

	// BasisClassicTrustlineSum is trustline balances only, a lower bound: claimables, LP reserves and SAC
	// balances are missed by construction. It floors the total, so total == circulating.
	BasisClassicTrustlineSum Basis = "classic_trustline_sum"

	// BasisContractStorageBalances sums per-holder BALANCE ledger entries (`Balance(Address) → i128`) for
	// tokens emitting no SEP-41 events, which supply_flows sums to a confident zero. It supersedes the
	// event reading and is never summed with it (double-count). A lower bound.
	BasisContractStorageBalances Basis = "contract_storage_balances"

	// BasisNoMetadata means there is no defensible value for at least one of total / circulating / max;
	// that field is nil rather than fabricated.
	BasisNoMetadata Basis = "no_metadata"
)

// String returns the basis as a string for log lines and metric labels.
func (b Basis) String() string { return string(b) }

// LowerBound reports whether a figure on this basis is only a provable floor (e.g. [BasisClassicTrustlineSum] misses whole holding domains), derived from the basis.
func (b Basis) LowerBound() bool {
	switch b {
	case BasisClassicTrustlineSum, BasisContractStorageBalances:
		return true
	default:
		return false
	}
}

// Supply is the wire-shape result of a supply derivation; TotalSupply and CirculatingSupply are never nil, MaxSupply is nil when no defensible value exists.
type Supply struct {
	AssetKey          string
	TotalSupply       *big.Int
	CirculatingSupply *big.Int
	MaxSupply         *big.Int
	Basis             Basis
	MaxSupplyBasis    Basis
	LedgerSequence    uint32
	ObservedAt        time.Time

	// MinComponentLedger is the oldest ledger of any component behind this snapshot; the refresher
	// rejects snapshots where (N - M) exceeds the stale-component threshold. Zero means unpopulated.
	MinComponentLedger uint32

	// SACWrappedStroops is Algorithm 2's SACWrapped component, broken out of TotalSupply so
	// [CrossCheckSubsetBound] can compare it with the SAC total. nil means none recorded and MUST NOT
	// be read as zero, which would be a vacuous pass.
	SACWrappedStroops *big.Int
}
