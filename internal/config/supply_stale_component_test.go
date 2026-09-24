package config

import (
	"strings"
	"testing"
)

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
