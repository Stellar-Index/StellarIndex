package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stubFXHistory implements FXHistoryReader with a canned point list,
// recording call count + last ticker so tests can pin the per-ticker
// memoisation and the lookup routing.
type stubFXHistory struct {
	points []FXQuotePoint
	calls  int
	ticker string
}

func (s *stubFXHistory) ListFXHistory(_ context.Context, ticker string, _, _ time.Time) ([]FXQuotePoint, error) {
	s.calls++
	s.ticker = ticker
	return s.points, nil
}

const (
	pegTestAUDD = "AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"
	pegTestAUDR = "AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL"
)

func pegTestServer(t *testing.T, fx FXHistoryReader) *Server {
	t.Helper()
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		fxHistory: fx,
		fiatPeggedClassics: map[string]canonical.Asset{
			pegTestAUDD: aud,
			pegTestAUDR: aud,
		},
	}
}

// TestFillDeclaredPegPrices_AfterSubstanceGate — the core contract of
// the declared-peg fill (operator approval 2026-08-24): an asset whose
// dust-authored market price the substance gate correctly withholds
// gets its price filled from the declared fiat peg × the fresh AUD/USD
// rate, stamped price_basis="declared_peg". The gate-then-fill ORDER
// is what makes that possible — the fill only writes nil PriceUSD, so
// running it before the gate would fill nothing and the gate would
// strip a pre-existing fill.
func TestFillDeclaredPegPrices_AfterSubstanceGate(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSD: 1.5267, InverseUSD: 0.655},
	}}
	s := pegTestServer(t, fx)
	// Gate denies everything (no allow entries) — AUDD's USD books are
	// the inconsistent bot dust the 2026-08-24 census found.
	s.substance = &stubListingGate{allow: map[string]bool{}}

	dust, ch := "0.80", "+1.00"
	other := "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	rows := []AssetDetail{
		{AssetID: pegTestAUDD, Code: "AUDD", PriceUSD: &dust, Change24hPct: &ch},
		{AssetID: pegTestAUDR, Code: "AUDR"},
		{AssetID: other, Code: "SCAM", PriceUSD: &dust},
	}
	ctx := context.Background()
	s.applySubstanceGateToListing(ctx, rows)
	s.fillDeclaredPegPricesInListing(ctx, rows)

	if rows[0].PriceUSD == nil || *rows[0].PriceUSD != "0.655" {
		t.Fatalf("withheld peg row: price_usd = %v, want 0.655 (the fresh AUD/USD inverse rate)", strPtr(rows[0].PriceUSD))
	}
	if rows[0].PriceBasis != "declared_peg" {
		t.Errorf("withheld peg row: price_basis = %q, want declared_peg", rows[0].PriceBasis)
	}
	if rows[0].Change24hPct != nil {
		t.Error("peg-filled row must not carry change pills (no market-history claim)")
	}
	if rows[1].PriceUSD == nil || *rows[1].PriceUSD != "0.655" {
		t.Errorf("second peg row: price_usd = %v, want 0.655", strPtr(rows[1].PriceUSD))
	}
	if fx.calls != 1 {
		t.Errorf("fx lookups = %d, want 1 (AUD memoised across AUDD + AUDR)", fx.calls)
	}
	if fx.ticker != "AUD" {
		t.Errorf("fx lookup ticker = %q, want AUD", fx.ticker)
	}
	if rows[2].PriceUSD != nil || rows[2].PriceBasis != "" {
		t.Error("unconfigured withheld row must stay priceless with no basis")
	}
}

// TestFillDeclaredPegPrices_NeverOverwritesMarketPrice — a
// market-derived price that SURVIVED the gate always wins; the fill is
// nil-only and must not stamp a basis on a market observation.
func TestFillDeclaredPegPrices_NeverOverwritesMarketPrice(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSD: 1.5267, InverseUSD: 0.655},
	}}
	s := pegTestServer(t, fx)
	s.substance = &stubListingGate{allow: map[string]bool{
		pegTestAUDD + "|native": true, // a real market cleared the floor
	}}

	market := "0.652"
	rows := []AssetDetail{{AssetID: pegTestAUDD, Code: "AUDD", PriceUSD: &market}}
	ctx := context.Background()
	s.applySubstanceGateToListing(ctx, rows)
	s.fillDeclaredPegPricesInListing(ctx, rows)

	if rows[0].PriceUSD == nil || *rows[0].PriceUSD != "0.652" {
		t.Fatalf("market price overwritten: got %v, want 0.652", strPtr(rows[0].PriceUSD))
	}
	if rows[0].PriceBasis != "" {
		t.Errorf("market-priced row must carry no price_basis, got %q", rows[0].PriceBasis)
	}
	if fx.calls != 0 {
		t.Errorf("fx lookups = %d, want 0 (nothing to fill)", fx.calls)
	}
}

// TestFillDeclaredPegPrices_StaleFXDoesNotFill — the fx staleness
// discipline: a rate older than declaredPegFXMaxAge must NOT fill; the
// row degrades to a nil price rather than presenting a stale rate as
// current. (In production fiatUSDPriceFor's 7-day query window already
// filters this on the fx_quotes path; the guard also covers the
// PriceReader fallback tier, whose stale flag that resolver discards.)
func TestFillDeclaredPegPrices_StaleFXDoesNotFill(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-10 * 24 * time.Hour), RateUSD: 1.5267, InverseUSD: 0.655},
	}}
	s := pegTestServer(t, fx)

	rows := []AssetDetail{{AssetID: pegTestAUDD, Code: "AUDD"}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)

	if rows[0].PriceUSD != nil {
		t.Fatalf("stale FX filled a price: %v", strPtr(rows[0].PriceUSD))
	}
	if rows[0].PriceBasis != "" {
		t.Errorf("no fill must mean no basis, got %q", rows[0].PriceBasis)
	}
}

// TestFillDeclaredPegPrice_DetailPathSingleRow — the asset-detail call
// site uses the single-row helper with a nil memo; same fill + stamp,
// and a USD-declared peg resolves by identity (no FX lookup),
// mirroring fiatMarketCapUSD's special case.
func TestFillDeclaredPegPrice_DetailPathSingleRow(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSD: 1.5267, InverseUSD: 0.655},
	}}
	s := pegTestServer(t, fx)

	detail := AssetDetail{AssetID: pegTestAUDD, Code: "AUDD"}
	s.fillDeclaredPegPrice(context.Background(), &detail, nil)
	if detail.PriceUSD == nil || *detail.PriceUSD != "0.655" {
		t.Fatalf("detail fill: price_usd = %v, want 0.655", strPtr(detail.PriceUSD))
	}
	if detail.PriceBasis != "declared_peg" {
		t.Errorf("detail fill: price_basis = %q, want declared_peg", detail.PriceBasis)
	}

	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	s.fiatPeggedClassics["USDX-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"] = usd
	d2 := AssetDetail{AssetID: "USDX-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"}
	s.fillDeclaredPegPrice(context.Background(), &d2, nil)
	if d2.PriceUSD == nil || *d2.PriceUSD != "1.00000000000000" {
		t.Fatalf("USD peg identity: price_usd = %v, want 1.00000000000000", strPtr(d2.PriceUSD))
	}
	if d2.PriceBasis != "declared_peg" {
		t.Errorf("USD peg identity: price_basis = %q, want declared_peg", d2.PriceBasis)
	}
}

// strPtr renders a *string for failure messages ("<nil>" for nil).
func strPtr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
