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
	out := rwaCuratedSummarise(snap, rows, rwaCuratedExcluded{classic: classicSkipped}, false, &verified, false, time.Now())

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
}
