package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

const pegHandlerTestAUDD = "AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"

// pegHandlerTestServer wires the full handleAssetGet chain for the
// declared-peg detail-path tests: a substance gate with a blanket
// verdict (reusing price_withheld_test.go's stubSubstanceGate), an
// asset-catalogue overlay row carrying a (dust or real) market price +
// change pills, a fresh AUD FX point (chart_test.go's
// stubFXHistoryReader), and AUDD peg-configured.
func pegHandlerTestServer(t *testing.T, allow bool, row timescale.AssetRow) *v1.Server {
	t.Helper()
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			row.AssetID: {AssetID: row.AssetID, Type: "classic", Code: row.Code},
		},
	}
	return v1.New(v1.Options{
		Assets:       reader,
		AssetsReader: &stubAssetsReaderExt{row: row},
		Substance:    &stubSubstanceGate{allow: allow},
		FXHistory: &stubFXHistoryReader{points: []v1.FXQuotePoint{
			{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSD: 1.5267, InverseUSD: 0.655},
		}},
		FiatPeggedClassics: map[string]canonical.Asset{pegHandlerTestAUDD: aud},
	})
}

// derefOrNil renders a *string for failure messages.
func derefOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func getPegAssetDetail(t *testing.T, srv *v1.Server, assetID string) v1.AssetDetail {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// TestAssetGet_DeclaredPeg_DustCataloguePriceReplacedByPeg — the
// detail-path half of the 2026-08-24 AUDD fix, end-to-end through
// handleAssetGet: the asset-catalogue overlay carries the dust-authored
// $0.78 price the substance gate withholds on the listing, and pre-fix
// applyAssetRowToDetail copied it onto the detail UNGATED — so the
// listing served the $0.655 peg while the detail presented $0.78 as a
// market price, and the nil-only peg fill never ran. With the overlay
// gated by the same per-pair verdict, the detail now serves the
// declared-peg price with its provenance, matching the listing.
func TestAssetGet_DeclaredPeg_DustCataloguePriceReplacedByPeg(t *testing.T) {
	srv := pegHandlerTestServer(t,
		false, // gate denies every pair — AUDD's USD books are bot dust
		timescale.AssetRow{
			AssetID:     pegHandlerTestAUDD,
			Code:        "AUDD",
			Slug:        "AUDD",
			PriceUSD:    sptr("0.7763"),
			Change1hPct: sptr("+3.10"),
			Change7dPct: sptr("-9.90"),
		})
	d := getPegAssetDetail(t, srv, pegHandlerTestAUDD)
	if d.PriceUSD == nil || *d.PriceUSD != "0.655" {
		t.Fatalf("price_usd = %s, want 0.655 (declared AUD peg × fresh AUD/USD rate, not the $0.7763 dust)", derefOrNil(d.PriceUSD))
	}
	if d.PriceBasis != "declared_peg" {
		t.Errorf("price_basis = %q, want declared_peg", d.PriceBasis)
	}
	if d.Change1hPct != nil || d.Change7dPct != nil {
		t.Errorf("gated-out row must lose the overlay change pills (got 1h=%v 7d=%v) — dust pills must not outlive their price",
			d.Change1hPct, d.Change7dPct)
	}
}

// TestAssetGet_SubstanceGate_NonPeggedDustDetailWithheld — #28
// closure for every asset, not just pegged ones: a NON-configured
// asset whose only price is the ungated catalogue overlay now has that
// price (and the pills derived from it) withheld on the detail path,
// matching the listing's verdict for the same pair set.
func TestAssetGet_SubstanceGate_NonPeggedDustDetailWithheld(t *testing.T) {
	dustID := "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	srv := pegHandlerTestServer(t,
		false, // gate denies every pair
		timescale.AssetRow{
			AssetID:     dustID,
			Code:        "SCAM",
			Slug:        "SCAM",
			PriceUSD:    sptr("123.45"),
			Change1hPct: sptr("+99.00"),
			Change7dPct: sptr("+400.00"),
		})
	d := getPegAssetDetail(t, srv, dustID)
	if d.PriceUSD != nil {
		t.Fatalf("price_usd = %v, want null (substance-withheld, no peg configured)", *d.PriceUSD)
	}
	if d.PriceBasis != "" {
		t.Errorf("price_basis = %q, want absent (no fill happened)", d.PriceBasis)
	}
	if d.Change1hPct != nil || d.Change7dPct != nil {
		t.Errorf("withheld row must lose its change pills (got 1h=%v 7d=%v)", d.Change1hPct, d.Change7dPct)
	}
}

// TestAssetGet_SubstanceGate_MarketPriceSurvivesAndPegDoesNotFill —
// the floor-passing case: a real market keeps its catalogue price +
// pills on the detail, and the peg fill (nil-only) never overwrites it
// or stamps a basis, even for a peg-configured asset.
func TestAssetGet_SubstanceGate_MarketPriceSurvivesAndPegDoesNotFill(t *testing.T) {
	srv := pegHandlerTestServer(t,
		true, // a real market cleared the substance floor
		timescale.AssetRow{
			AssetID:     pegHandlerTestAUDD,
			Code:        "AUDD",
			Slug:        "AUDD",
			PriceUSD:    sptr("0.652"),
			Change1hPct: sptr("+0.11"),
			Change7dPct: sptr("-0.04"),
		})
	d := getPegAssetDetail(t, srv, pegHandlerTestAUDD)
	if d.PriceUSD == nil || *d.PriceUSD != "0.652" {
		t.Fatalf("price_usd = %s, want the surviving market price 0.652", derefOrNil(d.PriceUSD))
	}
	if d.PriceBasis != "" {
		t.Errorf("price_basis = %q, want absent (market-derived price must carry no basis)", d.PriceBasis)
	}
	if d.Change1hPct == nil || *d.Change1hPct != "+0.11" {
		t.Errorf("change_1h_pct = %v, want +0.11 (allowed row keeps its pills)", d.Change1hPct)
	}
	if d.Change7dPct == nil || *d.Change7dPct != "-0.04" {
		t.Errorf("change_7d_pct = %v, want -0.04", d.Change7dPct)
	}
}
