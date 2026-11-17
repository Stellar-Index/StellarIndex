package v1

import (
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWACuratedClassicRowsMakeTheAdditionalFiguresALowerBound: a priced classic curator row the verified set does not carry has
// no contract path to be served, so additional_value_usd and combined_value_usd leave it out and must say lower bound.
func TestRWACuratedClassicRowsMakeTheAdditionalFiguresALowerBound(t *testing.T) {
	const contract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	snap := rwaCurated{available: true, wired: true, byAddress: map[string]timescale.CuratedRWAEntry{
		cetesAsset: {},
		contract:   {},
	}}

	members, classicSkipped := rwaCuratedMembership(snap, nil)
	if len(members) != 1 || classicSkipped != 1 {
		t.Fatalf("members = %d, classicSkipped = %d; want the contract served and the classic row counted", len(members), classicSkipped)
	}
	if _, carried := rwaCuratedMembership(snap, []RWAAsset{{Code: "CETES", Issuer: cetesIssuer}}); carried != 0 {
		t.Errorf("classicSkipped = %d with CETES in the verified set, want 0: a carried row is not excluded", carried)
	}

	value, verified := "100.00", "50.00"
	rows := []RWAAsset{{ContractID: contract, ReferenceValuation: RWAReferenceValuation{ValueUSD: &value}}}
	out := rwaCuratedSummarise(snap, rows, classicSkipped, false, &verified, false, nil, time.Now())

	if out.AdditionalValueUSD == nil || out.CombinedValueUSD == nil {
		t.Fatalf("additional = %v, combined = %v; want both served", out.AdditionalValueUSD, out.CombinedValueUSD)
	}
	if !out.AdditionalValueLowerBound || !out.CombinedValueLowerBound {
		t.Errorf("additional_value_lower_bound = %v, combined_value_lower_bound = %v; want both true with a classic row excluded",
			out.AdditionalValueLowerBound, out.CombinedValueLowerBound)
	}
	if !strings.Contains(out.Basis, "exclude 1 classic curator row(s)") {
		t.Errorf("basis does not name the excluded classic row: %q", out.Basis)
	}
	if strings.Contains(out.Basis, "catalogue row") {
		t.Errorf("basis names a contract-row exclusion no served path produces: %q", out.Basis)
	}

	// No additional figure is published, so no lower-bound sentence describes one.
	none := rwaCuratedSummarise(snap, nil, classicSkipped, false, &verified, false, nil, time.Now())
	if none.AdditionalValueUSD != nil || strings.Contains(none.Basis, "LOWER BOUND") {
		t.Errorf("additional = %v, basis = %q; want no figure and no lower-bound sentence", none.AdditionalValueUSD, none.Basis)
	}
}

// TestRWACuratedClassicRowWhoseSACTheCuratorAlsoListsIsCarried: a curator listing CODE-ISSUER and its SAC serves the SAC row,
// so the classic entry is not excluded and does not make the figures a lower bound.
func TestRWACuratedClassicRowWhoseSACTheCuratorAlsoListsIsCarried(t *testing.T) {
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatalf("derive CETES SAC: %v", err)
	}
	snap := rwaCurated{available: true, wired: true, byAddress: map[string]timescale.CuratedRWAEntry{cetesAsset: {}, sac: {}}}
	members, classicSkipped := rwaCuratedMembership(snap, nil)
	if len(members) != 1 || members[0].contractID != sac || classicSkipped != 0 {
		t.Fatalf("members = %+v, classicSkipped = %d; want the SAC served and the classic row carried by it", members, classicSkipped)
	}
	const other = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	snap.byAddress = map[string]timescale.CuratedRWAEntry{cetesAsset: {}, other: {}}
	if _, skipped := rwaCuratedMembership(snap, nil); skipped != 1 {
		t.Errorf("classicSkipped = %d with only an unrelated contract listed, want 1", skipped)
	}
}
