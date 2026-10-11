package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ClassicSupplyComponents is the per-asset Algorithm 2 breakdown (ADR-0011) at one ledger: total is
// the sum of the four holding components, circulating is total minus the locked-set. All are non-nil
// *big.Int (zero is valid); IssuerBalance is separate so the default locked-set member needs no re-query.
type ClassicSupplyComponents struct {
	// Trustline is Σ trustline_balance across every classic holder. No issuer filter is needed: the
	// protocol refuses a trustline to one's own asset, so no issuer row exists.
	Trustline *big.Int

	// Claimable is Σ claimable_balance amount for the asset across
	// every open claimable balance entry.
	Claimable *big.Int

	// LPReserve is Σ liquidity_pool reserve for the asset across every classic LP: the pool's
	// reserve of this asset, not the LP-token holding side.
	LPReserve *big.Int

	// SACWrapped is Σ contract_data balance for the asset's SAC wrapper. Zero when no SAC is deployed
	// OR SACObserved is false (the reader's SQL COALESCEs a missing sum to zero).
	SACWrapped *big.Int

	// SACObserved is true when the reader found a real SAC-balance observation at-or-before the read
	// ledger. [ClassicComputer.Compute] uses it to populate [Supply.SACWrappedStroops]; without it a
	// COALESCE-to-zero sum would run the [CrossCheckSubsetBound] escrow gate on nothing.
	SACObserved bool

	// IssuerBalance is the amount the issuer holds (usually zero), subtracted from circulating per the
	// default locked-set policy.
	IssuerBalance *big.Int

	// LockedAccountBalances is Σ trustline_balance across the per-asset locked-set Accounts in
	// [Policy.PerAsset], returned by the reader to avoid a per-account query path.
	LockedAccountBalances *big.Int

	// LockedContractBalances is Σ contract_data balance across the per-asset locked-set Contracts
	// in [Policy.PerAsset], fetched in one query like LockedAccountBalances.
	LockedContractBalances *big.Int

	// MinComponentLedger is the lowest ledger of any contributing component observation; the [Refresher]
	// uses it to detect lagging snapshots. Zero means no freshness signal and skips the check.
	MinComponentLedger uint32
}

// ClassicSupplyReader is the read-side interface the [ClassicComputer] needs. It takes a
// [canonical.Asset] so a caller cannot pass an asset_key mismatching the indexer's storage shape.
type ClassicSupplyReader interface {
	ClassicSupplyAt(ctx context.Context, asset canonical.Asset, locked LockedSet, ledger uint32) (ClassicSupplyComponents, error)
}

// ClassicComputer derives Algorithm 2 supply for classic credit assets from a [Policy] and a
// [ClassicSupplyReader]. Safe for concurrent Compute() calls: fields are read-only after construction.
type ClassicComputer struct {
	policy Policy
	reader ClassicSupplyReader
}

// NewClassicComputer constructs an Algorithm 2 computer; it returns ErrNilReader for a nil reader.
func NewClassicComputer(policy Policy, reader ClassicSupplyReader) (*ClassicComputer, error) {
	if reader == nil {
		return nil, ErrNilReader
	}
	return &ClassicComputer{policy: policy, reader: reader}, nil
}

// ErrNotClassic is returned by [ClassicComputer.Compute] for a non-[canonical.AssetClassic] asset,
// so callers route it to the right computer instead of getting a silently wrong supply.
var ErrNotClassic = errors.New("supply: asset is not a classic credit asset")

// Compute returns the [Supply] for a classic asset (Algorithm 2): total is trustline + claimable +
// LP-reserve + SAC-wrapped; circulating subtracts issuer, locked-account and locked-contract balances;
// max_supply is the Policy override or nil (SEP-1 overlay applies at the API layer).
func (c *ClassicComputer) Compute(ctx context.Context, asset canonical.Asset, ledger uint32, observedAt time.Time) (Supply, error) {
	if asset.Type != canonical.AssetClassic {
		return Supply{}, fmt.Errorf("%w: got type %q", ErrNotClassic, asset.Type)
	}

	key, err := AssetKey(asset)
	if err != nil {
		return Supply{}, fmt.Errorf("supply: derive asset key: %w", err)
	}

	locked := c.policy.PerAsset[key] // zero value when missing — that's OK, falls back to issuer-only

	comps, err := c.reader.ClassicSupplyAt(ctx, asset, locked, ledger)
	if err != nil {
		return Supply{}, fmt.Errorf("supply: classic component read for %s at ledger %d: %w", key, ledger, err)
	}
	if err := validateClassicComponents(comps); err != nil {
		return Supply{}, fmt.Errorf("supply: classic reader returned invalid components for %s at ledger %d: %w", key, ledger, err)
	}

	total := new(big.Int)
	total.Add(total, comps.Trustline)
	total.Add(total, comps.Claimable)
	total.Add(total, comps.LPReserve)
	total.Add(total, comps.SACWrapped)

	// circulating = total − issuer_balance − locked_account_balances − locked_contract_balances
	circulating := new(big.Int).Set(total)
	circulating.Sub(circulating, comps.IssuerBalance)
	circulating.Sub(circulating, comps.LockedAccountBalances)
	circulating.Sub(circulating, comps.LockedContractBalances)
	// Clamp at zero: a locked-set exceeding total (misconfig or
	// snapshot-freshness skew) must not publish negative circulating/market-cap.
	if circulating.Sign() < 0 {
		circulating.SetInt64(0)
	}

	// max_supply: an operator override beats the default nil; the SEP-1 overlay applies later, at
	// the API serving layer.
	var maxSupply *big.Int
	basis := BasisIssuerExclusion
	if override, ok, err := c.policy.MaxSupplyOverride(key); err != nil {
		return Supply{}, fmt.Errorf("supply: max_supply override for %s: %w", key, err)
	} else if ok {
		maxSupply = override
		basis = BasisOverride
	}

	// A non-empty per-asset locked-set with no max-supply override still upgrades basis to
	// Override: the operator set a non-default circulating policy.
	if !locked.IsEmpty() && basis == BasisIssuerExclusion {
		basis = BasisOverride
	}

	return Supply{
		AssetKey:          key,
		TotalSupply:       total,
		CirculatingSupply: circulating,
		MaxSupply:         maxSupply,
		Basis:             basis,
		LedgerSequence:    ledger,
		ObservedAt:        observedAt.UTC(),
		// SACWrapped is carried OUT of the fold as well as into it so the cross-check can compare it with
		// the SAC's Algorithm-3 total. Defensive copy (no aliasing the reader's component). nil unless
		// SACObserved: a COALESCE-to-zero sum must read "unchecked", not a false green.
		SACWrappedStroops:  sacWrappedStroops(comps),
		MinComponentLedger: comps.MinComponentLedger,
	}, nil
}

// sacWrappedStroops derives [Supply.SACWrappedStroops]: a copy of SACWrapped when a SAC balance
// was observed, nil otherwise (see [ClassicSupplyComponents.SACObserved]).
func sacWrappedStroops(comps ClassicSupplyComponents) *big.Int {
	if !comps.SACObserved {
		return nil
	}
	return new(big.Int).Set(comps.SACWrapped)
}

// validateClassicComponents rejects nil or negative components: nil would panic the Add/Sub chain,
// and classic balances are non-negative so a negative reading means the indexer mis-summed.
func validateClassicComponents(c ClassicSupplyComponents) error {
	type field struct {
		name string
		val  *big.Int
	}
	for _, f := range []field{
		{"Trustline", c.Trustline},
		{"Claimable", c.Claimable},
		{"LPReserve", c.LPReserve},
		{"SACWrapped", c.SACWrapped},
		{"IssuerBalance", c.IssuerBalance},
		{"LockedAccountBalances", c.LockedAccountBalances},
		{"LockedContractBalances", c.LockedContractBalances},
	} {
		if f.val == nil {
			return fmt.Errorf("%s is nil", f.name)
		}
		if f.val.Sign() < 0 {
			return fmt.Errorf("%s is negative (%s)", f.name, f.val.String())
		}
	}
	return nil
}
