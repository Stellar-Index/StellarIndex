package config

import (
	"strings"
	"testing"
)

const (
	treasuryAccount = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	vestingContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// lockedSetCoverageConfig watches PHO (classic) and sep41Contract, with
// one locked set and the given sac_wrappers.
func lockedSetCoverageConfig(key string, ls SupplyLockedSetConfig, wrappers map[string]string) SupplyConfig {
	sc := staleComponentSupplyConfig(nil)
	sc.SACWrappers = wrappers
	sc.PerAssetLockedSets = map[string]SupplyLockedSetConfig{key: ls}
	return sc
}

// Every locked-set member reads as a zero balance when no observer records
// it, so a configuration with an unobserved member must fail at boot.
func TestSupplyValidate_LockedSetCoverageRejected(t *testing.T) {
	phoDash := "PHO-" + phoIssuerAccount
	accounts := SupplyLockedSetConfig{Accounts: []string{treasuryAccount}}
	contracts := SupplyLockedSetConfig{Contracts: []string{vestingContract}}
	cases := map[string]struct {
		sc   SupplyConfig
		want string
	}{
		"sep41 key not in sac_wrappers": {
			sc:   lockedSetCoverageConfig(sep41Contract, contracts, nil),
			want: "not a sac_wrappers key",
		},
		"sep41 key mapped to another contract": {
			sc:   lockedSetCoverageConfig(sep41Contract, contracts, map[string]string{sep41Contract: vestingContract}),
			want: "neither itself nor a classic asset",
		},
		"sac key accounts, classic asset unwatched": {
			sc:   lockedSetCoverageConfig(sep41Contract, accounts, map[string]string{sep41Contract: "USDC-" + phoIssuerAccount}),
			want: "observed only for watched_classic_assets",
		},
		"classic key contracts, no SAC wrapper": {
			sc:   lockedSetCoverageConfig(phoDash, contracts, nil),
			want: "no sac_wrappers entry maps a SAC",
		},
	}
	for name, tc := range cases {
		err := tc.sc.Validate()
		if err == nil || !strings.Contains(err.Error(), "per_asset_locked_sets") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want a per_asset_locked_sets error containing %q", name, err, tc.want)
		}
	}
}

// Each (asset, holder kind) an observer does record passes, including a
// member that may never have been funded: coverage is per kind, not per row.
func TestSupplyValidate_LockedSetCoverageAccepted(t *testing.T) {
	phoDash := "PHO-" + phoIssuerAccount
	both := SupplyLockedSetConfig{Accounts: []string{treasuryAccount}, Contracts: []string{vestingContract}}
	cases := map[string]SupplyConfig{
		"sep41 self-map":                  lockedSetCoverageConfig(sep41Contract, both, map[string]string{sep41Contract: sep41Contract}),
		"sac of a watched classic asset":  lockedSetCoverageConfig(sep41Contract, both, map[string]string{sep41Contract: phoDash}),
		"classic key with its SAC":        lockedSetCoverageConfig(phoDash, both, map[string]string{sep41Contract: "PHO:" + phoIssuerAccount}),
		"classic key accounts only":       lockedSetCoverageConfig(phoDash, SupplyLockedSetConfig{Accounts: []string{treasuryAccount}}, nil),
		"sep41 explicit empty locked set": lockedSetCoverageConfig(sep41Contract, SupplyLockedSetConfig{}, nil),
		"sac key contracts, classic unwatched": lockedSetCoverageConfig(sep41Contract, SupplyLockedSetConfig{Contracts: []string{vestingContract}},
			map[string]string{sep41Contract: "USDC-" + phoIssuerAccount}),
	}
	for name, sc := range cases {
		if err := sc.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil", name, err)
		}
	}
}
