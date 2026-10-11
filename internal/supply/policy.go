package supply

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// LockedSet lists accounts/contracts whose holdings are excluded from circulating_supply for one
// asset (ADR-0011). Defaults: issuer balance (algorithm 2), admin balance (algorithm 3); operators
// extend via the supply-policy YAML. LP-reserve balances are NEVER locked: the asset still circulates.
type LockedSet struct {
	// Accounts are G-strkey addresses (classic accounts, SAC admin
	// G-addresses, treasury multisigs) whose XLM / classic-asset
	// balances should be subtracted from circulating.
	Accounts []string

	// Contracts are C-strkey contract ids (SEP-41 admin contracts,
	// vesting contracts, treasury contracts) whose token balances
	// should be subtracted from circulating.
	Contracts []string
}

// IsEmpty reports whether this set excludes nothing, letting computers skip the locked-balance
// lookup. Not the same as "no override" in YAML, where the per-algorithm default applies.
func (l LockedSet) IsEmpty() bool {
	return len(l.Accounts) == 0 && len(l.Contracts) == 0
}

// Policy is the operator configuration loaded from YAML at start: SDF-reserve accounts (XLM),
// per-asset locked-set overrides, and max_supply overrides. Missing fields keep the per-algorithm
// defaults (no SDF exclusion; issuer/admin-only locked-set; SEP-1 max_supply or nil).
// Read-only after load; concurrent reads are safe.
type Policy struct {
	// SDFReserveAccounts is the G-address list whose XLM balances
	// are excluded from XLM circulating_supply per Algorithm 1.
	// Source: SDF publishes the list; we maintain a YAML version-
	// controlled in the deployment repo.
	SDFReserveAccounts []string

	// PerAsset overrides the default locked-set per asset, keyed by [AssetKey] ("CODE:G...", "C...");
	// native XLM is not a valid key. A present-but-empty entry means explicitly no exclusions; a
	// missing key falls back to the per-algorithm default.
	PerAsset map[string]LockedSet

	// MaxSupplyOverrides force a max_supply per asset (same keys as PerAsset), beating the SEP-1
	// declaration. Values are decimal strings in base units; empty falls through to the next source.
	MaxSupplyOverrides map[string]string
}

// MaxSupplyOverride looks up the operator override for assetKey.
// Returns the override as a *big.Int when set, nil + ok=false when
// no override exists, error when the YAML value isn't a parseable
// decimal integer.
func (p Policy) MaxSupplyOverride(assetKey string) (*big.Int, bool, error) {
	raw, ok := p.MaxSupplyOverrides[assetKey]
	if !ok || raw == "" {
		return nil, false, nil
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, false, fmt.Errorf("supply: max_supply override for %q is not a decimal integer (got %q)", assetKey, raw)
	}
	// SetString accepts a leading '-', and a negative override would pass Validate() then be rejected
	// by asset_supply_history's `CHECK (max_supply >= 0)` on every tick, serving the last good
	// snapshot forever while config validation reports fine.
	if n.Sign() < 0 {
		return nil, false, fmt.Errorf(
			"supply: max_supply override for %q is negative (got %q) — supply is unsigned and the store rejects it",
			assetKey, raw)
	}
	return n, true, nil
}

// validateDistinctNonEmpty reports one error per empty entry and per duplicate. Duplicates are
// REJECTED, not deduped: readers sum per element, so a repeated address is subtracted twice from
// circulating supply (one duplicate reserve account cut it 12%) with no alert.
func validateDistinctNonEmpty(label string, items []string) []error {
	var errs []error
	seen := make(map[string]int, len(items))
	for i, v := range items {
		if v == "" {
			errs = append(errs, fmt.Errorf("supply: %s[%d] is empty", label, i))
			continue
		}
		if first, dup := seen[v]; dup {
			errs = append(errs, fmt.Errorf(
				"supply: %s[%d] duplicates [%d] (%s) — its balance would be counted twice",
				label, i, first, v))
			continue
		}
		seen[v] = i
	}
	return errs
}

// validateStrkeys also rejects entries that are not CRC-valid strkeys of the expected kind: a typo
// matches no holder, contributes 0 to the exclusion, and overstates circulating supply silently.
func validateStrkeys(label, kind string, valid func(string) bool, items []string) []error {
	errs := validateDistinctNonEmpty(label, items)
	for i, v := range items {
		if v != "" && !valid(v) {
			errs = append(errs, fmt.Errorf(
				"supply: %s[%d] %q is not a valid %s strkey — it would match no holder and exclude nothing",
				label, i, v, kind))
		}
	}
	return errs
}

// Validate checks the policy for structural problems. Returns
// joined errors when multiple problems exist. Cheap; runs at
// process start after YAML load.
func (p Policy) Validate() error {
	var errs []error

	errs = append(errs, validateStrkeys("SDFReserveAccounts", "G", canonical.IsAccountID, p.SDFReserveAccounts)...)

	for assetKey, override := range p.MaxSupplyOverrides {
		if err := validatePolicyKey("MaxSupplyOverrides", assetKey); err != nil {
			errs = append(errs, err)
		}
		if override == "" {
			continue // sentinel for "fall through"
		}
		if _, _, err := p.MaxSupplyOverride(assetKey); err != nil {
			errs = append(errs, err)
		}
	}

	for assetKey, locked := range p.PerAsset {
		if err := validatePolicyKey("PerAsset", assetKey); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, validateStrkeys(
			fmt.Sprintf("PerAsset[%q].Accounts", assetKey), "G", canonical.IsAccountID, locked.Accounts)...)
		errs = append(errs, validateStrkeys(
			fmt.Sprintf("PerAsset[%q].Contracts", assetKey), "C", canonical.IsContractID, locked.Contracts)...)
	}

	return errors.Join(errs...)
}
