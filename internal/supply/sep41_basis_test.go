package supply_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// C1-041 (audit-2026-07-23). Algorithm 3 stamped
// `basis:"admin_exclusion"` on every default-path snapshot, including
// the ones where nothing at all was excluded — the production reader
// (StorageSEP41SupplyReader) hardcodes AdminBalance=0 because v1
// doesn't track `set_admin`, and with no per-asset locked-set
// configured circulating == total. A consumer reading
// `admin_exclusion` believes the issuer's own holdings were netted
// out of circulating supply (and therefore out of market cap); they
// were not.
//
// BasisXLMTotalOnly is the exact precedent (CS-010): Algorithm 1
// reports `xlm_total_only`, not `xlm_sdf_reserve_exclusion`, when the
// reserve-account list is empty.
//
// This table pins the full basis matrix so neither direction can
// regress: a real exclusion must still read `admin_exclusion`, and a
// no-op exclusion must read `sep41_total_only`.
func TestSEP41_Compute_BasisNeverClaimsAnExclusionThatDidNotHappen(t *testing.T) {
	asset := mustSoroban(t, validContractID)
	lockedHolder := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	cases := []struct {
		name string
		// comps the reader hands back.
		admin           int64
		lockedAccounts  int64
		lockedContracts int64
		// policy shape.
		lockedSet   bool
		maxOverride string
		wantBasis   supply.Basis
		// wantCirculating is total (1_000) minus what was excluded.
		wantCirculating int64
	}{
		{
			// THE C1-041 CASE — the production default path.
			name:            "nothing_excluded_is_total_only",
			admin:           0,
			wantBasis:       supply.BasisSEP41TotalOnly,
			wantCirculating: 1_000,
		},
		{
			// A reader that DOES resolve a real admin balance keeps
			// the honest admin_exclusion label.
			name:            "real_admin_balance_is_admin_exclusion",
			admin:           250,
			wantBasis:       supply.BasisAdminExclusion,
			wantCirculating: 750,
		},
		{
			// Operator-configured locked-set: the documented way to
			// exclude an admin today. Already BasisOverride; must not
			// be re-labelled by the new branch.
			name:            "configured_locked_set_is_override",
			admin:           0,
			lockedAccounts:  400,
			lockedSet:       true,
			wantBasis:       supply.BasisOverride,
			wantCirculating: 600,
		},
		{
			// A max_supply override wins regardless of admin balance.
			name:            "max_supply_override_is_override",
			admin:           0,
			maxOverride:     "5000",
			wantBasis:       supply.BasisOverride,
			wantCirculating: 1_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubSEP41Reader{
				comps: supply.SEP41SupplyComponents{
					MintTotal:              bigInt(1_000),
					BurnTotal:              bigInt(0),
					ClawbackTotal:          bigInt(0),
					AdminBalance:           bigInt(tc.admin),
					LockedAccountBalances:  bigInt(tc.lockedAccounts),
					LockedContractBalances: bigInt(tc.lockedContracts),
				},
			}

			policy := supply.Policy{}
			if tc.lockedSet {
				policy.PerAsset = map[string]supply.LockedSet{
					validContractID: {Accounts: []string{lockedHolder}},
				}
			}
			if tc.maxOverride != "" {
				policy.MaxSupplyOverrides = map[string]string{
					validContractID: tc.maxOverride,
				}
			}

			c, err := supply.NewSEP41Computer(policy, reader)
			if err != nil {
				t.Fatalf("NewSEP41Computer: %v", err)
			}
			got, err := c.Compute(context.Background(), asset, 100, time.Unix(0, 0).UTC())
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}

			if got.Basis != tc.wantBasis {
				t.Errorf("Basis = %q, want %q", got.Basis, tc.wantBasis)
			}
			if got.CirculatingSupply.Cmp(big.NewInt(tc.wantCirculating)) != 0 {
				t.Errorf("CirculatingSupply = %s, want %d", got.CirculatingSupply, tc.wantCirculating)
			}
			// The load-bearing invariant, stated directly: the basis
			// may only claim an exclusion when circulating actually
			// moved away from total.
			excluded := got.TotalSupply.Cmp(got.CirculatingSupply) != 0
			if got.Basis == supply.BasisAdminExclusion && !excluded {
				t.Errorf("basis claims admin_exclusion but circulating(%s) == total(%s)",
					got.CirculatingSupply, got.TotalSupply)
			}
			if got.Basis == supply.BasisSEP41TotalOnly && excluded {
				t.Errorf("basis claims sep41_total_only but circulating(%s) != total(%s)",
					got.CirculatingSupply, got.TotalSupply)
			}
		})
	}
}

// The two "total only" bases must stay distinct string values and
// distinct from the exclusion bases — they are wire values consumers
// switch on, and collapsing any pair would silently re-introduce the
// lie this fix removes.
func TestBasis_TotalOnlyValuesAreDistinct(t *testing.T) {
	all := map[supply.Basis]string{
		supply.BasisXLMSDFReserveExclusion: "xlm_sdf_reserve_exclusion",
		supply.BasisXLMTotalOnly:           "xlm_total_only",
		supply.BasisIssuerExclusion:        "issuer_exclusion",
		supply.BasisAdminExclusion:         "admin_exclusion",
		supply.BasisSEP41TotalOnly:         "sep41_total_only",
		supply.BasisOverride:               "override",
		supply.BasisSEP1DeclaredMax:        "sep1_declared_max",
		supply.BasisSEP41LakeFlows:         "sep41_lake_flows",
		supply.BasisNoMetadata:             "no_metadata",
	}
	if len(all) != 9 {
		t.Fatalf("basis values collided: %v", all)
	}
	for basis, want := range all {
		if basis.String() != want {
			t.Errorf("Basis %q renders as %q, want %q", basis, basis.String(), want)
		}
	}
}
