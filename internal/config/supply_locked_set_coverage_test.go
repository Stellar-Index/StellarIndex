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

const (
	phoIssuerAccount = "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	sep41Contract    = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
)

func staleComponentSupplyConfig(byAsset map[string]uint32) SupplyConfig {
	return SupplyConfig{
		WatchedClassicAssets:         []string{"PHO-" + phoIssuerAccount},
		WatchedSEP41Contracts:        []string{sep41Contract},
		StaleComponentLedgersByAsset: byAsset,
	}
}

// Every spelling the aggregator can resolve onto a watched asset's
// snapshot key must pass boot validation.
func TestSupplyValidate_StaleComponentKeysAccepted(t *testing.T) {
	for _, key := range []string{
		"PHO-" + phoIssuerAccount,
		"PHO:" + phoIssuerAccount,
		"XLM",
		"native",
		sep41Contract,
	} {
		sc := staleComponentSupplyConfig(map[string]uint32{key: 5000})
		if err := sc.Validate(); err != nil {
			t.Errorf("key %q: Validate() = %v, want nil", key, err)
		}
	}
}

// per_asset_locked_sets / max_supply_overrides keys are matched
// exactly by the classic and SEP-41 computers: every spelling of a
// watched asset passes, XLM (Algorithm 1 reads neither map) and any
// key naming no watched asset fail at boot.
func TestSupplyValidate_PolicyOverrideKeys(t *testing.T) {
	for _, key := range []string{"PHO-" + phoIssuerAccount, "PHO:" + phoIssuerAccount, sep41Contract} {
		sc := staleComponentSupplyConfig(nil)
		sc.PerAssetLockedSets = map[string]SupplyLockedSetConfig{key: {}}
		sc.MaxSupplyOverrides = map[string]string{key: "1000"}
		if err := sc.Validate(); err != nil {
			t.Errorf("key %q: Validate() = %v, want nil", key, err)
		}
	}
	rejected := map[string]string{
		"XLM":                      "native XLM",
		"native":                   "native XLM",
		"PHO_" + phoIssuerAccount:  "asset key",
		"USDC-" + phoIssuerAccount: "names no watched asset",
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "names no watched asset",
	}
	for key, want := range rejected {
		locked := staleComponentSupplyConfig(nil)
		locked.PerAssetLockedSets = map[string]SupplyLockedSetConfig{key: {}}
		maxSupply := staleComponentSupplyConfig(nil)
		maxSupply.MaxSupplyOverrides = map[string]string{key: "1000"}
		for field, sc := range map[string]SupplyConfig{"per_asset_locked_sets": locked, "max_supply_overrides": maxSupply} {
			err := sc.Validate()
			if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), want) {
				t.Errorf("%s key %q: Validate() = %v, want error naming %q and %q", field, key, err, field, want)
			}
		}
	}
}

// A key the per-asset gate can never match must fail at boot rather
// than silently leave the global threshold in force.
func TestSupplyValidate_StaleComponentKeysRejected(t *testing.T) {
	cases := map[string]struct {
		byAsset map[string]uint32
		want    string
	}{
		"typo": {
			byAsset: map[string]uint32{"PHO_" + phoIssuerAccount: 5000},
			want:    "stale_component_ledgers_by_asset",
		},
		"unwatched classic": {
			byAsset: map[string]uint32{"USDC-" + phoIssuerAccount: 5000},
			want:    "names no watched asset",
		},
		"unwatched contract": {
			byAsset: map[string]uint32{"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": 5000},
			want:    "names no watched asset",
		},
		"off-chain asset": {
			byAsset: map[string]uint32{"fiat:USD": 5000},
			want:    "no on-chain supply key",
		},
		"two spellings of one asset": {
			byAsset: map[string]uint32{"native": 5000, "XLM": 2000},
			want:    "both name",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := staleComponentSupplyConfig(tc.byAsset).Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
