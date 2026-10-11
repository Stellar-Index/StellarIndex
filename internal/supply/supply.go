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

	// BasisXLMSDFReserveExclusionStatic is Algorithm 1 with reserve balances from the operator's dated
	// static map ([ConfigReserveBalanceReader]). Distinct so consumers can tell it from a per-ledger read;
	// it has no freshness anchor, so strict_freshness_required refuses it.
	BasisXLMSDFReserveExclusionStatic Basis = "xlm_sdf_reserve_exclusion_static"

	// BasisXLMTotalOnly is Algorithm 1 with NO reserve accounts configured (circulating == total). Kept
	// distinct so the wire never claims an SDF exclusion that did not happen, which would overstate
	// circulating supply by the unsubtracted ~18-19B SDF-held stroops.
	BasisXLMTotalOnly Basis = "xlm_total_only"

	// BasisIssuerExclusion — Algorithm 2 default: classic credit
	// assets where circulating excludes the issuer's own balance.
	BasisIssuerExclusion Basis = "issuer_exclusion"

	// BasisAdminExclusion — Algorithm 3 default: SEP-41 tokens
	// where circulating excludes the admin account/contract balance.
	// Only emitted when a non-zero admin balance was ACTUALLY
	// subtracted; see BasisSEP41TotalOnly for the other case.
	BasisAdminExclusion Basis = "admin_exclusion"

	// BasisSEP41TotalOnly is Algorithm 3 with NOTHING excluded; kept distinct so the wire never claims an
	// exclusion that did not happen. The DEFAULT today: [StorageSEP41SupplyReader] hardcodes AdminBalance
	// to zero, so operators put the admin in the per-asset LockedSet, which yields BasisOverride.
	BasisSEP41TotalOnly Basis = "sep41_total_only"

	// BasisOverride — operator-configured override beat the
	// algorithm-default policy. Used for both circulating
	// (extended locked-set) and max_supply.
	BasisOverride Basis = "override"

	// BasisSEP1DeclaredMax means the SEP-1 [[CURRENCIES]] overlay ([Overlay]) populated max_supply from
	// stellar.toml. The cap is issuer-SELF-DECLARED, a display value not enforced on-chain (ADR-0011).
	// Carried on [Supply.MaxSupplyBasis] only; [Supply.Basis] keeps naming the total's algorithm.
	BasisSEP1DeclaredMax Basis = "sep1_declared_max"

	// BasisSEP41LakeFlows is Algorithm 3 summed live over the lake's stellar.supply_flows
	// (Σmint−Σburn−Σclawback), used when no LCM observer snapshot exists. NOT admin-excluded (no admin
	// tracking in the flow sum): total == circulating.
	BasisSEP41LakeFlows Basis = "sep41_lake_flows"

	// BasisClassicLakeFlows is a classic asset's Σmint−Σburn−Σclawback over stellar.supply_flows for its
	// SAC. An UPPER reading: a replayed mint with no burn yields a too-LARGE total nothing detects (PHO
	// +156.79% vs Horizon). Ranks below a direct observation. NOT exclusion-netted: total == circulating.
	BasisClassicLakeFlows Basis = "classic_lake_flows"

	// BasisClassicTrustlineSum is TRUSTLINE BALANCES ONLY, a LOWER BOUND: claimables, LP reserves and
	// SAC balances are missed by construction. NOT exclusion-netted: it floors the TOTAL, total == circulating.
	BasisClassicTrustlineSum Basis = "classic_trustline_sum"

	// BasisContractStorageBalances sums per-holder BALANCE ledger entries (`Balance(Address) → i128`)
	// for tokens emitting NO SEP-41 events, which supply_flows sums to a confident zero. NEVER summed
	// with the event reading (double-count); it supersedes it. A LOWER BOUND (circulating_supply_lower_bound).
	BasisContractStorageBalances Basis = "contract_storage_balances"

	// BasisNoMetadata — we don't have a defensible value for
	// at least one of total / circulating / max. Per ADR-0011
	// "we don't fabricate" — the corresponding field is nil.
	BasisNoMetadata Basis = "no_metadata"
)

// String returns the basis as a string for log lines + metric
// labels. Equivalent to a direct cast; provided for fluency.
func (b Basis) String() string { return string(b) }

// LowerBound reports whether a figure on this basis is a provable FLOOR. [BasisClassicTrustlineSum]
// misses holding DOMAINS (89.5% of EURMTL, 73.3% of PYUSD, 15.4% of USDC omitted);
// [BasisContractStorageBalances] misses entries never projected. The flag is DERIVED from the basis.
func (b Basis) LowerBound() bool {
	switch b {
	case BasisClassicTrustlineSum, BasisContractStorageBalances:
		return true
	default:
		return false
	}
}

// Supply is the wire-shape result of a supply derivation. AssetKey is "XLM", "CODE:G...", or a bare
// contract id. TotalSupply / CirculatingSupply are never nil; MaxSupply is nil when no defensible value
// exists (ADR-0011: we don't fabricate). MaxSupplyBasis is set only when MaxSupply is not from Basis.
type Supply struct {
	AssetKey          string
	TotalSupply       *big.Int
	CirculatingSupply *big.Int
	MaxSupply         *big.Int
	Basis             Basis
	MaxSupplyBasis    Basis
	LedgerSequence    uint32
	ObservedAt        time.Time

	// MinComponentLedger is the oldest ledger of any component observation behind this snapshot; the
	// refresher rejects snapshots where (N - M) exceeds the stale-component threshold. Zero means the
	// computer did not populate it: no freshness signal, max-ledger semantics.
	MinComponentLedger uint32

	// SACWrappedStroops is Algorithm 2's SACWrapped component, broken out of TotalSupply and persisted
	// (sac_wrapped_stroops, migration 0117) so [CrossCheckSubsetBound] can compare it with the SAC total.
	// nil means none recorded (Algorithms 1/3, pre-0117 rows); MUST NOT be read as zero, a vacuous pass.
	SACWrappedStroops *big.Int
}
