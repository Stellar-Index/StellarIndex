package v1

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	fillTestSoroban = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	fillTestHop     = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
)

// fillTestTransitiveServer answers one clean one-hop route with both legs cleared by the substance gate.
func fillTestTransitiveServer() *Server {
	return &Server{
		Options: Options{
			TransitivePricer: &stubPricer{ok: true, tp: timescale.TransitivePrice{PriceUSD: "7934.40", Hop: fillTestHop, HopQuote: "native"}},
			Substance:        &stubListingGate{allow: map[string]bool{fillTestSoroban + "|" + fillTestHop: true, fillTestHop + "|native": true}},
		},
		logger: slog.New(slog.DiscardHandler),
	}
}

// TestPriceFills_HonourEveryWithholding drives each real producer that can set price_usd after the direct read, over every
// withheld reason, against one expected table. A refusal the fill may not answer must survive with price_usd null.
func TestPriceFills_HonourEveryWithholding(t *testing.T) {
	soroban, err := canonical.ParseAsset(fillTestSoroban)
	if err != nil {
		t.Fatal(err)
	}
	classic, err := canonical.ParseAsset("TKN-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fills := map[priceFill]func(t *testing.T, d *AssetDetail){
		fillCatalogueRow: func(_ *testing.T, d *AssetDetail) {
			price := "1.25"
			d.AssetID = classic.String()
			s := &Server{logger: slog.New(slog.DiscardHandler)}
			s.applyAssetRowToDetail(ctx, d, classic, timescale.AssetRow{PriceUSD: &price}, nil, classic.String())
		},
		fillTransitive: func(_ *testing.T, d *AssetDetail) {
			d.AssetID = fillTestSoroban
			fillTestTransitiveServer().applyAssetRowToDetail(ctx, d, soroban, timescale.AssetRow{}, sql.ErrNoRows, fillTestSoroban)
		},
		fillGlobalMarket: func(t *testing.T, d *AssetDetail) {
			s := globalMarketServer(t, refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
			d.AssetID = globalTestUSDC
			s.applyGlobalMarket(ctx, d, s.globalMarketRefs(ctx, []AssetDetail{*d}), time.Now())
		},
		fillDeclaredPeg: func(t *testing.T, d *AssetDetail) {
			d.AssetID = globalTestUSDC
			globalMarketServer(t).fillFixedPegPrice(ctx, d, nil)
		},
	}
	// The include_thin second pass over the transitive route, with a held thin leg measured on the asset's own market.
	fills[fillThinRelease] = func(_ *testing.T, d *AssetDetail) {
		d.AssetID = fillTestSoroban
		_, off := WithThinAdmission(ctx, soroban, defaultPriceQuote, true)
		off.Merge(false, &pricingguard.SubstanceEvidence{Base: soroban, Quote: defaultPriceQuote})
		fillTestTransitiveServer().thinDetailPricePass(ctx, d, soroban, off)
	}
	// Columns: catalogue row, transitive, global market, declared peg, thin release.
	want := map[PriceWithheldReason][5]bool{
		"":                             {true, true, true, true, true},
		PriceWithheldSubstance:         {true, true, true, true, true},
		PriceWithheldUpstreamLeg:       {false, false, true, true, true},
		PriceWithheldDecimalsMismatch:  {false, false, true, true, false},
		PriceWithheldManipulationGuard: {false, false, true, true, false},
		PriceWithheldFXLeg:             {false, false, true, false, false},
		PriceWithheldScamIssuer:        {false, false, false, false, false},
		PriceWithheldUnattributed:      {false, false, false, false, false},
		PriceWithheldSourceExcluded:    {false, false, false, false, false},
	}
	if len(want) != len(priceWithheldReasons)+1 {
		t.Errorf("want has %d rows, want one per priceWithheldReasons entry plus the empty reason (%d)", len(want), len(priceWithheldReasons)+1)
	}
	for _, reason := range priceWithheldReasons {
		if _, ok := want[reason]; !ok {
			t.Errorf("PriceWithheldReason %q has no row: classify it in fillMayReplace and here", reason)
		}
	}
	for reason, row := range want {
		for col, f := range []priceFill{fillCatalogueRow, fillTransitive, fillGlobalMarket, fillDeclaredPeg, fillThinRelease} {
			d := AssetDetail{PriceWithheldReason: reason}
			fills[f](t, &d)
			if filled := d.PriceUSD != nil; filled != row[col] {
				t.Errorf("reason %q fill %d: filled = %v, want %v", reason, f, filled, row[col])
			}
			if !row[col] && d.PriceWithheldReason != reason {
				t.Errorf("reason %q fill %d: the withholding was rewritten to %q", reason, f, d.PriceWithheldReason)
			}
			if got := fillMayReplace(reason, f); got != row[col] {
				t.Errorf("fillMayReplace(%q, %d) = %v, want %v", reason, f, got, row[col])
			}
		}
	}
	if fillMayReplace("a_reason_added_later", fillGlobalMarket) {
		t.Error("an unknown withholding must stand against every fill")
	}
}

// fillExclGate withholds one pair (either orientation) with a fixed verdict.
type fillExclGate struct {
	base, quote string
	verdict     pricingguard.Withholding
}

func (g fillExclGate) hit(b, q canonical.Asset) pricingguard.Withholding {
	if (b.String() == g.base && q.String() == g.quote) || (b.String() == g.quote && q.String() == g.base) {
		return g.verdict
	}
	return pricingguard.NotWithheld
}

func (g fillExclGate) Withholding(_ context.Context, b, q canonical.Asset, _ []string, _, _ time.Time) pricingguard.Withholding {
	return g.hit(b, q)
}

func (g fillExclGate) InstantWithholding(_ context.Context, b, q canonical.Asset, _ string, _ time.Time) pricingguard.Withholding {
	return g.hit(b, q)
}

func (g fillExclGate) UnattributedWithholding(_ context.Context, b, q canonical.Asset, _, _ time.Time) pricingguard.Withholding {
	return g.hit(b, q)
}

func (fillExclGate) MultiHopWithholding(context.Context, time.Time, time.Time) pricingguard.Withholding {
	return pricingguard.NotWithheld
}

// Snapshot cannot carry this fake's pair rule, so a set-judged read through it is refused rather than judged clean.
func (fillExclGate) Snapshot(context.Context) (pricingguard.ExclusionSet, pricingguard.Withholding) {
	return pricingguard.ExclusionSet{}, pricingguard.WithheldExclusionsUnread
}

// TestTransitiveFill_ExcludedLegIsNotServed: prices_1m pools every source, so a hop whose near or far leg an operator excluded
// cannot price the asset even when the direct read found nothing to withhold; an unreadable exclusion set is an unread fill.
func TestTransitiveFill_ExcludedLegIsNotServed(t *testing.T) {
	soroban, err := canonical.ParseAsset(fillTestSoroban)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		gate       PriceExclusionGate
		wantPrice  bool
		wantUnread bool
	}{
		{name: "no exclusion", gate: fillExclGate{verdict: pricingguard.WithheldSourceExcluded}, wantPrice: true},
		{name: "near leg excluded", gate: fillExclGate{base: fillTestSoroban, quote: fillTestHop, verdict: pricingguard.WithheldSourceExcluded}},
		{name: "far leg excluded", gate: fillExclGate{base: fillTestHop, quote: "native", verdict: pricingguard.WithheldSourceExcluded}},
		{name: "anchor leg excluded", gate: pricingguard.NewSourceExclusionGate(globalExclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "sdex", Base: "native", Quote: globalTestUSDC, From: time.Now().Add(-time.Hour),
		}}}, pricingguard.SourceExclusionGateOptions{})},
		{name: "exclusions unread", gate: fillExclGate{base: fillTestSoroban, quote: fillTestHop, verdict: pricingguard.WithheldExclusionsUnread}, wantUnread: true},
	} {
		s := fillTestTransitiveServer()
		s.Exclusions = tc.gate
		d := AssetDetail{AssetID: fillTestSoroban}
		s.fillTransitivePrice(context.Background(), &d, soroban, fillTestSoroban)
		if (d.PriceUSD != nil) != tc.wantPrice || d.extensionUnread != tc.wantUnread {
			t.Errorf("%s: price_usd=%s unread=%v, want price %v unread %v", tc.name, strOrEmpty(d.PriceUSD), d.extensionUnread, tc.wantPrice, tc.wantUnread)
		}
	}
}
