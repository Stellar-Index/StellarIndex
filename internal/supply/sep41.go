package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SEP41SupplyComponents is the per-token breakdown at one ledger (ADR-0011 Algorithm 3): total =
// Mint - Burn - Clawback over the contract's lifetime; circulating = total - admin balance -
// locked-set balances. All fields are non-nil *big.Int; zero is valid.
type SEP41SupplyComponents struct {
	// MintTotal is Σ mint.amount over the contract's lifetime.
	MintTotal *big.Int

	// BurnTotal is Σ burn.amount over the contract's lifetime.
	BurnTotal *big.Int

	// ClawbackTotal is Σ clawback.amount over the contract's
	// lifetime. Distinct from BurnTotal even though both reduce
	// total — operators want to see them separately for
	// compliance / forensic dashboards.
	ClawbackTotal *big.Int

	// AdminBalance is the token's admin account/contract balance
	// as observed at this ledger. The default locked-set member
	// per Algorithm 3; subtracted from circulating without operator
	// configuration.
	AdminBalance *big.Int

	// LockedAccountBalances is Σ token-balance across the
	// per-asset locked-set Accounts configured in
	// [Policy.PerAsset]. Returned by the reader so the computer
	// doesn't need a separate per-account query path.
	LockedAccountBalances *big.Int

	// LockedContractBalances is Σ token-balance across the
	// per-asset locked-set Contracts configured in
	// [Policy.PerAsset]. Same single-query rationale as
	// LockedAccountBalances.
	LockedContractBalances *big.Int

	// MinComponentLedger is the lowest ledger any contributing
	// observation was last updated at. Threaded into Supply for
	// the refresher freshness
	// gate. Zero = "reader didn't populate" — the gate skips.
	MinComponentLedger uint32

	// GenesisBaselineSeeded reports whether a pre-Soroban genesis baseline is seeded (migration
	// 0088). When false the totals cover only the Soroban era, so a pre-Soroban SAC wrapper reads
	// sum(burn) > sum(mint); [SEP41Computer.Compute] routes that to the benign `missing_baseline`
	// outcome instead of the paging `compute_error`.
	GenesisBaselineSeeded bool
}

// SEP41SupplyReader is the [SEP41Computer]'s read side (production: Postgres event-sum totals plus
// the contract_data store). It receives the contract id and the locked-set so it can produce the
// locked sums in a single query; it returns non-nil components, and storage errors propagate as-is.
type SEP41SupplyReader interface {
	SEP41SupplyAt(ctx context.Context, asset canonical.Asset, locked LockedSet, ledger uint32) (SEP41SupplyComponents, error)
}

// SEP41Computer derives Algorithm 3 supply for SEP-41 Soroban tokens from a [Policy] (locked-set,
// max_supply overrides) and a [SEP41SupplyReader]. Safe for concurrent Compute calls.
type SEP41Computer struct {
	policy Policy
	reader SEP41SupplyReader
}

// NewSEP41Computer constructs an Algorithm 3 computer. Returns
// ErrNilReader when reader is nil.
func NewSEP41Computer(policy Policy, reader SEP41SupplyReader) (*SEP41Computer, error) {
	if reader == nil {
		return nil, ErrNilReader
	}
	return &SEP41Computer{policy: policy, reader: reader}, nil
}

// ErrNotSoroban is returned by [SEP41Computer.Compute] when the
// supplied asset isn't a [canonical.AssetSoroban]. Routing bug
// safety net.
var ErrNotSoroban = errors.New("supply: asset is not a SEP-41 Soroban token")

// ErrNegativeTotalSupply is returned when Mint - Burn - Clawback is negative AND the genesis
// baseline HAS been seeded. That is physically impossible, so the running totals mis-summed
// upstream; surface it (the refresher maps it to the paging `compute_error`) rather than emit a
// negative supply.
var ErrNegativeTotalSupply = errors.New("supply: SEP-41 mint − burn − clawback went negative")

// ErrNegativeTotalMissingBaseline is returned when the total is negative and the genesis baseline
// is NOT seeded (migration 0088): a pre-Soroban SAC wrapper legitimately reads sum(burn) >
// sum(mint) until `stellarindex-ops supply seed-sep41-genesis` runs. Not corruption; the
// refresher maps it to the benign `missing_baseline` outcome.
var ErrNegativeTotalMissingBaseline = errors.New("supply: SEP-41 total negative and pre-Soroban genesis baseline not seeded — run `stellarindex-ops supply seed-sep41-genesis`")

// ErrGenesisBaselineNotSeeded is returned for a classic asset's SAC wrapper with no seeded
// pre-Soroban baseline, whatever the sign: the Soroban-era-only sum omits pre-Soroban mints and
// burns and would misstate lifetime supply. Benign like [ErrNegativeTotalMissingBaseline]
// (`missing_baseline`).
var ErrGenesisBaselineNotSeeded = errors.New("supply: SEP-41 SAC wrapper's pre-Soroban genesis baseline not seeded — run `stellarindex-ops supply seed-sep41-genesis`")

// Compute returns the [Supply] for a SEP-41 token (Algorithm 3): total = Mint - Burn - Clawback;
// circulating = total - AdminBalance - locked-set balances; max is the Policy override or nil. Basis is
// BasisOverride, else BasisAdminExclusion only if a non-zero AdminBalance was subtracted, else BasisSEP41TotalOnly.
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
		// A negative Soroban-era-only total with no seeded baseline is EXPECTED (benign
		// `missing_baseline`); a negative total with the baseline seeded is a genuine inconsistency
		// (`compute_error`, pages). See migration 0088.
		sentinel := ErrNegativeTotalSupply
		if !comps.GenesisBaselineSeeded {
			sentinel = ErrNegativeTotalMissingBaseline
		}
		return Supply{}, fmt.Errorf("%w: mint=%s burn=%s clawback=%s for %s at ledger %d",
			sentinel,
			comps.MintTotal, comps.BurnTotal, comps.ClawbackTotal,
			key, ledger)
	}
	// A native SEP-41 token has no pre-Soroban history and native XLM has no
	// issuer mints; only a classic credit asset's SAC needs the seeded baseline.
	if wraps := canonical.CanonicalAsset(asset); !comps.GenesisBaselineSeeded && wraps.Type == canonical.AssetClassic {
		return Supply{}, fmt.Errorf("%w: %s wraps %s at ledger %d",
			ErrGenesisBaselineNotSeeded, key, wraps, ledger)
	}

	// circulating = total − admin_balance − locked_account_balances − locked_contract_balances
	circulating := new(big.Int).Set(total)
	circulating.Sub(circulating, comps.AdminBalance)
	circulating.Sub(circulating, comps.LockedAccountBalances)
	circulating.Sub(circulating, comps.LockedContractBalances)
	// Clamp at zero. A locked-set exceeding total (operator
	// misconfig, or a locked-holder snapshot fresher than total's) would
	// otherwise publish a negative circulating supply → negative market cap.
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

	// As in xlm.go: claim an admin exclusion only when one happened. Here the only possible
	// subtraction is AdminBalance, which [StorageSEP41SupplyReader] hardcodes to zero (set_admin is
	// untracked), so "admin_exclusion" would tell consumers the admin's holdings were netted out
	// and overstate circulating supply and market cap.
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
