package supply

import (
	"errors"
	"fmt"
	"math/big"
)

// LockedSet is the operator-configurable list of accounts/contracts
// whose holdings should be excluded from circulating_supply for one
// asset. Per ADR-0011:
//
//   - Algorithm 2 default locked-set is just the issuer's own
//     balance.
//   - Algorithm 3 default locked-set is the token's admin
//     account/contract balance.
//   - Operators extend per-asset via the supply-policy YAML to
//     include known reserve / treasury multisigs and vesting
//     contracts.
//
// LP-reserve balances are NEVER in the locked-set — the underlying
// asset is still circulating; LP-token holders own it pro-rata.
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

// IsEmpty reports whether this set excludes nothing — used by the
// per-algorithm computers to short-circuit the locked-balance
// lookup. (An empty LockedSet is meaningfully different from "no
// override" in YAML — the operator may have explicitly opted in to
// the per-algorithm default by leaving the entry off.)
func (l LockedSet) IsEmpty() bool {
	return len(l.Accounts) == 0 && len(l.Contracts) == 0
}

// Policy is the operator configuration for the supply package,
// loaded from YAML at process start. Per-algorithm computers consult
// it for SDF-reserve accounts (XLM), per-asset locked-set overrides
// (classic + SEP-41), and max_supply overrides.
//
// Defaults — when a field is missing from YAML — preserve the
// per-algorithm default behaviour: empty SDFReserveAccounts means
// "no exclusion" (the cluster is running without curated SDF
// account list); missing PerAsset entries fall back to issuer-only /
// admin-only locked-set; missing MaxSupplyOverrides leaves the
// max_supply field at the SEP-1 declaration if present, nil
// otherwise.
//
// Policy is read-only after load; concurrent reads are safe.
type Policy struct {
	// SDFReserveAccounts is the G-address list whose XLM balances
	// are excluded from XLM circulating_supply per Algorithm 1.
	// Source: SDF publishes the list; we maintain a YAML version-
	// controlled in the deployment repo.
	SDFReserveAccounts []string

	// PerAsset overrides the per-algorithm default locked-set for
	// specific assets. Key shape: "XLM" for native, "CODE:G..." for
	// classic, "C..." for SEP-41 Soroban tokens.
	//
	// A present-but-empty entry (LockedSet{} with both slices nil)
	// means "no exclusions for this asset" — overrides the default
	// issuer-only / admin-only locked-set with explicit nothing.
	// Missing key falls back to the per-algorithm default.
	PerAsset map[string]LockedSet

	// MaxSupplyOverrides force a max_supply for a specific asset,
	// beating both the SEP-1 declaration and the per-algorithm
	// default (nil for classic, nil for SEP-41 without
	// declaration). Value is a decimal string in the asset's base
	// unit (stroops for XLM / classic, contract-defined units for
	// SEP-41). Empty string means "fall through to next source"
	// (equivalent to omitting the key).
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
	// SetString happily parses a leading '-', so a sign typo used to pass
	// Validate() and then fail forever at the write boundary: the computers
	// set it as MaxSupply with BasisOverride, and asset_supply_history's
	// `CHECK (max_supply IS NULL OR max_supply >= 0)` rejects the row on
	// EVERY tick. The asset's last good snapshot is then served
	// indefinitely while the operator's first stop — config validation —
	// reports the config is fine (cold audit 2026-08-04).
	if n.Sign() < 0 {
		return nil, false, fmt.Errorf(
			"supply: max_supply override for %q is negative (got %q) — supply is unsigned and the store rejects it",
			assetKey, raw)
	}
	return n, true, nil
}

// validateDistinctNonEmpty reports one error per empty entry and one per
// duplicate, naming the index it first appeared at.
//
// Duplicates are REJECTED, not deduped. Every reserve/locked-set reader
// sums per slice element with no dedup, so a repeated address is
// subtracted twice from circulating supply — one duplicate of a real r1
// reserve account (4.108B XLM) drops served circulating supply by 12% and
// market cap by ~$699M, with supply_basis unchanged and no alert anywhere.
// Silently deduping would hide an operator error in the file that IS the
// source of truth (cold audit 2026-08-04).
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

// Validate checks the policy for structural problems. Returns
// joined errors when multiple problems exist. Cheap; runs at
// process start after YAML load.
func (p Policy) Validate() error {
	var errs []error

	errs = append(errs, validateDistinctNonEmpty("SDFReserveAccounts", p.SDFReserveAccounts)...)

	for assetKey, override := range p.MaxSupplyOverrides {
		if override == "" {
			continue // sentinel for "fall through"
		}
		if _, _, err := p.MaxSupplyOverride(assetKey); err != nil {
			errs = append(errs, err)
		}
	}

	for assetKey, locked := range p.PerAsset {
		errs = append(errs, validateDistinctNonEmpty(
			fmt.Sprintf("PerAsset[%q].Accounts", assetKey), locked.Accounts)...)
		errs = append(errs, validateDistinctNonEmpty(
			fmt.Sprintf("PerAsset[%q].Contracts", assetKey), locked.Contracts)...)
	}

	return errors.Join(errs...)
}
