package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SEP41SupplyComponents is the per-token Algorithm 3 breakdown at one ledger: total = Mint - Burn - Clawback, circulating = total - admin balance - locked-set balances, all non-nil *big.Int.
type SEP41SupplyComponents struct {
	// MintTotal is Σ mint.amount over the contract's lifetime.
	MintTotal *big.Int

	// BurnTotal is Σ burn.amount over the contract's lifetime.
	BurnTotal *big.Int

	// ClawbackTotal is Σ clawback.amount over the contract's lifetime, kept apart from BurnTotal
	// for compliance dashboards.
	ClawbackTotal *big.Int

	// AdminBalance is the admin account/contract balance at this ledger, the default locked-set
	// member subtracted from circulating without operator configuration.
	AdminBalance *big.Int

	// LockedAccountBalances is Σ balance across the [Policy.PerAsset] locked Accounts, summed by
	// the reader so the computer needs no per-account query path.
	LockedAccountBalances *big.Int

	// LockedContractBalances is the same sum across the locked Contracts.
	LockedContractBalances *big.Int

	// MinComponentLedger is the lowest ledger any contributing observation was last updated at,
	// threaded into Supply for the refresher freshness gate. Zero means unpopulated: the gate skips.
	MinComponentLedger uint32

	// GenesisBaselineSeeded reports whether a pre-Soroban genesis baseline is seeded (migration
	// 0088). When false the totals cover only the Soroban era, so a pre-Soroban SAC wrapper reads
	// sum(burn) > sum(mint); [SEP41Computer.Compute] routes that to the benign `missing_baseline`
	// outcome instead of the paging `compute_error`.
	GenesisBaselineSeeded bool
}

// SEP41SupplyReader is the [SEP41Computer]'s read side; it returns non-nil components given the contract id and locked-set, and storage errors propagate as-is.
type SEP41SupplyReader interface {
	SEP41SupplyAt(ctx context.Context, asset canonical.Asset, locked LockedSet, ledger uint32) (SEP41SupplyComponents, error)
}

// SEP41Computer derives Algorithm 3 supply for SEP-41 Soroban tokens from a [Policy] and a [SEP41SupplyReader]; safe for concurrent Compute calls.
type SEP41Computer struct {
	policy Policy
	reader SEP41SupplyReader
}

// NewSEP41Computer constructs an Algorithm 3 computer, returning ErrNilReader when reader is nil.
func NewSEP41Computer(policy Policy, reader SEP41SupplyReader) (*SEP41Computer, error) {
	if reader == nil {
		return nil, ErrNilReader
	}
	return &SEP41Computer{policy: policy, reader: reader}, nil
}

// ErrNotSoroban is returned by [SEP41Computer.Compute] for a non-Soroban asset (routing safety net).
var ErrNotSoroban = errors.New("supply: asset is not a SEP-41 Soroban token")

// ErrNegativeTotalSupply is returned when Mint - Burn - Clawback is negative AND the genesis baseline is seeded (impossible, so mis-summed upstream); the refresher maps it to the paging `compute_error`.
var ErrNegativeTotalSupply = errors.New("supply: SEP-41 mint − burn − clawback went negative")

// ErrNegativeTotalMissingBaseline is returned when the total is negative and the genesis baseline is NOT seeded (a pre-Soroban SAC wrapper legitimately reads burn > mint); benign `missing_baseline`, not corruption.
var ErrNegativeTotalMissingBaseline = errors.New("supply: SEP-41 total negative and pre-Soroban genesis baseline not seeded — run `stellarindex-ops supply seed-sep41-genesis`")

// ErrGenesisBaselineNotSeeded is returned for a classic asset's SAC wrapper with no seeded baseline, whatever the sign, since the Soroban-era sum omits earlier mints and burns; benign (`missing_baseline`).
var ErrGenesisBaselineNotSeeded = errors.New("supply: SEP-41 SAC wrapper's pre-Soroban genesis baseline not seeded — run `stellarindex-ops supply seed-sep41-genesis`")

// Compute returns the [Supply] for a SEP-41 token (Algorithm 3): total = Mint - Burn - Clawback, circulating = total - AdminBalance - locked-set balances, max is the Policy override or nil.
func (c *SEP41Computer) Compute(ctx context.Context, asset canonical.Asset, ledger uint32, observedAt time.Time) (Supply, error) {
	if asset.Type != canonical.AssetSoroban {
		return Supply{}, fmt.Errorf("%w: got type %q", ErrNotSoroban, asset.Type)
	}

	key, err := AssetKey(asset)
	if err != nil {
		return Supply{}, fmt.Errorf("supply: derive asset key: %w", err)
	}

	locked := c.policy.PerAsset[key]

	comps, err := c.reader.SEP41SupplyAt(ctx, asset, locked, ledger)
	if err != nil {
		return Supply{}, fmt.Errorf("supply: SEP-41 component read for %s at ledger %d: %w", key, ledger, err)
	}
	if err := validateSEP41Components(comps); err != nil {
		return Supply{}, fmt.Errorf("supply: SEP-41 reader returned invalid components for %s at ledger %d: %w", key, ledger, err)
	}

	// total = mint − burn − clawback
	total := new(big.Int).Set(comps.MintTotal)
	total.Sub(total, comps.BurnTotal)
	total.Sub(total, comps.ClawbackTotal)

	if total.Sign() < 0 {
		// Negative with no seeded baseline is expected (`missing_baseline`); with the baseline
		// seeded it is a real inconsistency (`compute_error`, pages).
		sentinel := ErrNegativeTotalSupply
		if !comps.GenesisBaselineSeeded {
			sentinel = ErrNegativeTotalMissingBaseline
		}
		return Supply{}, fmt.Errorf("%w: mint=%s burn=%s clawback=%s for %s at ledger %d",
			sentinel,
			comps.MintTotal, comps.BurnTotal, comps.ClawbackTotal,
			key, ledger)
	}
	// Only a classic credit asset's SAC has pre-Soroban history that needs the seeded baseline.
	if wraps := canonical.CanonicalAsset(asset); !comps.GenesisBaselineSeeded && wraps.Type == canonical.AssetClassic {
		return Supply{}, fmt.Errorf("%w: %s wraps %s at ledger %d",
			ErrGenesisBaselineNotSeeded, key, wraps, ledger)
	}

	// circulating = total − admin_balance − locked_account_balances − locked_contract_balances
	circulating := new(big.Int).Set(total)
	circulating.Sub(circulating, comps.AdminBalance)
	circulating.Sub(circulating, comps.LockedAccountBalances)
	circulating.Sub(circulating, comps.LockedContractBalances)
	// Clamp at zero: a locked set exceeding total (misconfig, or a fresher locked snapshot) would
	// otherwise publish a negative circulating supply and market cap.
	if circulating.Sign() < 0 {
		circulating.SetInt64(0)
	}

	// max_supply: operator override beats nil (the SEP-1 overlay
	// applies downstream at the API serving layer).
	var maxSupply *big.Int
	basis := BasisAdminExclusion
	if override, ok, err := c.policy.MaxSupplyOverride(key); err != nil {
		return Supply{}, fmt.Errorf("supply: max_supply override for %s: %w", key, err)
	} else if ok {
		maxSupply = override
		basis = BasisOverride
	}

	if !locked.IsEmpty() && basis == BasisAdminExclusion {
		basis = BasisOverride
	}

	// Claim an admin exclusion only when one happened: [StorageSEP41SupplyReader] hardcodes
	// AdminBalance to zero (set_admin is untracked), so "admin_exclusion" would falsely say the
	// admin's holdings were netted out.
	if basis == BasisAdminExclusion && comps.AdminBalance.Sign() == 0 {
		basis = BasisSEP41TotalOnly
	}

	return Supply{
		AssetKey:           key,
		TotalSupply:        total,
		CirculatingSupply:  circulating,
		MaxSupply:          maxSupply,
		Basis:              basis,
		LedgerSequence:     ledger,
		ObservedAt:         observedAt.UTC(),
		MinComponentLedger: comps.MinComponentLedger,
	}, nil
}

// validateSEP41Components rejects nil pointers and negative sums (running totals are non-negative
// by definition; negative means upstream indexer corruption), refusing to publish them.
func validateSEP41Components(c SEP41SupplyComponents) error {
	type field struct {
		name string
		val  *big.Int
	}
	for _, f := range []field{
		{"MintTotal", c.MintTotal},
		{"BurnTotal", c.BurnTotal},
		{"ClawbackTotal", c.ClawbackTotal},
		{"AdminBalance", c.AdminBalance},
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
