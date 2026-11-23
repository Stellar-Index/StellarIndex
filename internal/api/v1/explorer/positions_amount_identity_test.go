package explorer

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Two reserves sharing a code but not an issuer must be told apart on the wire: each leg carries its asset's
// identity beside the code-only label, and a leg whose label has no contract behind it carries none.
func TestLendingLegEntries_CarryAssetIdentity(t *testing.T) {
	const circle, fake = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	ids := map[string]string{circle: "USDC-GCIRCLE", fake: "USDC-GATTACKER"}
	resolve := func(c string) positionAsset { return positionAsset{ID: ids[c], Label: assetDisplayLabel(ids[c])} }
	reader := &blendRowsReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.BlendPositionFold{
		{Pool: "P1", Asset: circle, HasSupplyLeg: true, SupplyNet: "5"},
		{Pool: "P1", Asset: fake, HasSupplyLeg: true, SupplyNet: "5"},
	}}
	h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, reader)
	var got []string
	for _, e := range h.buildBlendPositions(context.Background(), validTestAccount, resolve, &positionsCoverage{}) {
		got = append(got, fmt.Sprint(e.Assets, e.AssetIDs))
	}
	if want := "[[USDC] [USDC-GCIRCLE] [USDC] [USDC-GATTACKER]]"; fmt.Sprint(got) != want {
		t.Errorf("assets/asset_ids = %v, want %s", got, want)
	}
}

// An open lending leg whose repayments or withdrawals exceed its principal (interest paid at the
// b/d-token rate) has a negative underlying net; that is not the position, so amount is unknown ("").
// A leg whose b/d-token balance is exactly zero is closed, a proven zero whatever its net.
func TestBlendLegEntry_NegativeNetIsUnknownNotServed(t *testing.T) {
	for _, tc := range []struct {
		name, net, tokens, wantAmount string
		wantClosed                    bool
	}{
		{"open borrow repaid past principal", "-500000000", "123", "", false},
		{"open supply withdrawn past principal", "-1", "5", "", false},
		{"open leg with unread balance", "-1", "", "", false},
		{"closed leg with interest earned", "-80", "0", "0", true},
		{"closed leg with a positive residue", "15", "0", "0", true},
		{"open positive net", "1000", "9", "1000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := blendLegEntry(PositionEntry{}, tc.net, false, tc.tokens)
			if got.Amount != tc.wantAmount || got.closed != tc.wantClosed {
				t.Errorf("amount=%q closed=%v, want amount=%q closed=%v", got.Amount, got.closed, tc.wantAmount, tc.wantClosed)
			}
			if got.AmountSemantics != AmountSemanticsNetUnderlying {
				t.Errorf("amount_semantics = %q, want %q", got.AmountSemantics, AmountSemanticsNetUnderlying)
			}
		})
	}
}
