// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func thinEvidence(base, quote canonical.Asset, buckets int64) *pricingguard.SubstanceEvidence {
	return &pricingguard.SubstanceEvidence{Base: base, Quote: quote, MeasuredAt: time.Now(), VolumeUSD: "8.57", Buckets: buckets}
}

func TestThinAdmissionCovers(t *testing.T) {
	thin := mustAsset(t, "THIN-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	other := mustAsset(t, "AQUA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	usd := mustAsset(t, "fiat:USD")
	xlm := canonical.NativeAsset()

	_, adm := WithThinAdmission(context.Background(), thin, usd, true)
	if !adm.Covers(thin, xlm) || !adm.Covers(xlm, thin) {
		t.Error("the request base on either leg must be covered")
	}
	if adm.Covers(other, usd) {
		t.Error("the request quote alone must not cover another asset's market")
	}
	_, xlmAdm := WithThinAdmission(context.Background(), xlm, usd, true)
	if !xlmAdm.Covers(mustAsset(t, "crypto:XLM"), usd) {
		t.Error("an alias of the request base must be covered")
	}
	var nilAdm *ThinAdmission
	if nilAdm.Requested() || nilAdm.Covers(thin, usd) || nilAdm.Admitted() || nilAdm.Evidence() != nil {
		t.Error("a nil record must answer as no admission")
	}

	if adm.ThinWithheld(PriceWithheldSubstance) {
		t.Error("no evidence recorded: the second pass must not run")
	}
	adm.Record(other, usd, pricingguard.Verdict{Withholding: pricingguard.WithheldThinMarket, Substance: thinEvidence(other, usd, 1)})
	if adm.Evidence() != nil {
		t.Error("an uncovered verdict's evidence was recorded")
	}
	adm.Record(usd, thin, pricingguard.Verdict{Withholding: pricingguard.WithheldThinMarket, Substance: thinEvidence(usd, thin, 0)})
	adm.Record(thin, xlm, pricingguard.Verdict{Withholding: pricingguard.WithheldThinMarket, Substance: thinEvidence(thin, xlm, 3)})
	adm.Record(thin, usd, pricingguard.Verdict{Withholding: pricingguard.WithheldThinMarket, Substance: thinEvidence(thin, usd, 5)})
	ev := adm.Evidence()
	if ev == nil || ev.Buckets != 3 || !ev.Base.Equal(thin) {
		t.Fatalf("withheld evidence = %+v, want the first non-empty measurement, oriented to the request base", ev)
	}
	for _, r := range []PriceWithheldReason{PriceWithheldSubstance, PriceWithheldUpstreamLeg, PriceWithheldUnattributed} {
		if !adm.ThinWithheld(r) {
			t.Errorf("ThinWithheld(%s) = false with evidence recorded", r)
		}
	}
	for _, r := range []PriceWithheldReason{PriceWithheldScamIssuer, PriceWithheldManipulationGuard, PriceWithheldFXLeg, ""} {
		if adm.ThinWithheld(r) {
			t.Errorf("ThinWithheld(%q) = true: only a substance-shaped withholding may be released", r)
		}
	}

	adm.Record(xlm, thin, pricingguard.Verdict{ThinAdmitted: true, Substance: thinEvidence(xlm, thin, 2)})
	if !adm.Admitted() {
		t.Fatal("an admitted covered verdict did not mark the record")
	}
	if ev := adm.Evidence(); ev.Buckets != 2 || !ev.Base.Equal(thin) {
		t.Errorf("evidence = %+v, want the admitted route's, oriented", ev)
	}
}

func TestThinAdmissionConcurrentRecord(t *testing.T) {
	thin := mustAsset(t, "THIN-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	usd := mustAsset(t, "fiat:USD")
	_, adm := WithThinAdmission(context.Background(), thin, usd, true)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := pricingguard.Verdict{Withholding: pricingguard.WithheldThinMarket, Substance: thinEvidence(thin, usd, int64(i))}
			if i%2 == 0 {
				v = pricingguard.Verdict{ThinAdmitted: true, Substance: thinEvidence(thin, usd, int64(i))}
			}
			adm.Record(thin, usd, v)
			_ = adm.Evidence()
		}()
	}
	wg.Wait()
	if !adm.Admitted() || adm.Evidence() == nil {
		t.Error("concurrent records lost the admission")
	}
}

// flightStore is thin for every market of a listed asset.
type flightStore map[string]bool

func (s flightStore) measure(bases, quotes []canonical.Asset) timescale.MarketSubstance {
	for _, a := range append(append([]canonical.Asset{}, bases...), quotes...) {
		if s[a.String()] {
			return timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1, ValuedBuckets: 1}
		}
	}
	return timescale.MarketSubstance{VolumeUSD: "5000000.00", Buckets: 1440, ValuedBuckets: 1440, SpanSeconds: 86000}
}

func (s flightStore) PairMarketSubstance(_ context.Context, bases, quotes []canonical.Asset, _ time.Duration) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

func (s flightStore) PairMarketSubstanceAt(_ context.Context, bases, quotes []canonical.Asset, _ time.Time, _ time.Duration, _ timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

// flightReader gates every read through withheldBy, as the store does.
type flightReader struct {
	gate   *pricingguard.SubstanceGate
	prices map[string]string
}

func (r flightReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (PriceSnapshot, []string, bool, error) {
	price, ok := r.prices[a.String()+"/"+q.String()]
	if !ok {
		return PriceSnapshot{}, nil, false, ErrPriceNotFound
	}
	if w := withheldBy(ctx, r.gate, nil, a, q, "test"); w != pricingguard.NotWithheld {
		return PriceSnapshot{}, nil, false, PriceWithheldError(w)
	}
	return PriceSnapshot{AssetID: a.String(), Quote: q.String(), Price: price}, []string{"sdex"}, false, nil
}

func (r flightReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, nil
}

// TestPriceReadFlightMergeRequiresCovers: a flight over another pair (an
// XLM cross leg) never writes the request's record; its own pair does.
func TestPriceReadFlightMergeRequiresCovers(t *testing.T) {
	thin := mustAsset(t, "THIN-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	usd := mustAsset(t, "fiat:USD")
	xlm := canonical.NativeAsset()
	gate := pricingguard.NewSubstanceGate(flightStore{thin.String(): true, xlm.String(): true}, pricingguard.SubstanceGateOptions{})
	reader := flightReader{gate: gate, prices: map[string]string{
		thin.String() + "/fiat:USD": "0.0123",
		xlm.String() + "/fiat:USD":  "0.16",
	}}
	s := New(Options{Prices: reader})
	ctx, adm := WithThinAdmission(context.Background(), thin, usd, true)

	if _, _, _, _, err := s.readPriceWithAliasesServed(ctx, reader, xlm, usd); !errors.Is(err, ErrPriceWithheld) {
		t.Fatalf("control: the XLM leg read = %v, want withheld", err)
	}
	if adm.Admitted() || adm.Evidence() != nil {
		t.Fatalf("an uncovered flight wrote the record: admitted=%v evidence=%+v", adm.Admitted(), adm.Evidence())
	}

	snap, _, _, _, err := s.readPriceWithAliasesServed(ctx, reader, thin, usd)
	if err != nil || snap.Price != "0.0123" {
		t.Fatalf("own pair: price=%q err=%v, want the thin price released", snap.Price, err)
	}
	if ev := adm.Evidence(); !adm.Admitted() || ev == nil || !ev.Base.Equal(thin) {
		t.Errorf("own pair: admitted=%v evidence=%+v, want the request base's evidence", adm.Admitted(), ev)
	}
}

// TestListingAdmitsThinFlaggedAndCountsOnServe pins the listing half:
// under the opt-in a measured-thin row keeps its price flagged and loses
// its pills; it counts only if it is still priced after every overlay.
func TestListingAdmitsThinFlaggedAndCountsOnServe(t *testing.T) {
	deep := "AQUA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	thin := "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	nulled := "NULL-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	s := &Server{Options: Options{Substance: &stubListingGate{allow: map[string]bool{deep + "|native": true}}}}
	p1, p2, p3, ch := "0.001", "0.13", "0.20", "+1.00"
	rows := []AssetDetail{
		{AssetID: deep, PriceUSD: &p1, Change24hPct: &ch},
		{AssetID: thin, PriceUSD: &p2, Change24hPct: &ch, Change7dPct: &ch},
		{AssetID: nulled, PriceUSD: &p3},
	}
	served := obs.PriceServeThinAdmittedTotal.WithLabelValues("listing", string(pricingguard.FloorNone))
	before := testutil.ToFloat64(served)

	s.applySubstanceGateToListing(context.Background(), rows, true)
	if rows[0].ThinMarket || rows[0].Change24hPct == nil {
		t.Error("a cleared row must be untouched")
	}
	if rows[1].PriceUSD == nil || !rows[1].ThinMarket || rows[1].PriceWithheldReason != "" {
		t.Errorf("thin row: price=%v thin=%v reason=%q, want the price served flagged", rows[1].PriceUSD, rows[1].ThinMarket, rows[1].PriceWithheldReason)
	}
	if rows[1].Change24hPct != nil || rows[1].Change7dPct != nil {
		t.Error("a thin row must not carry change pills")
	}
	rows[2].PriceUSD = nil // a later overlay (scam suppression) unpriced it
	settleListingThinRows(rows)
	if rows[2].ThinMarket {
		t.Error("an unpriced row kept thin_market")
	}
	if got := testutil.ToFloat64(served) - before; got != 1 {
		t.Errorf("listing thin serves counted = %v, want 1 (the unpriced row is not a serve)", got)
	}
}

func TestDeclaredPegBeatsThinListingPrice(t *testing.T) {
	fx := &stubFXHistory{points: []FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
	}}
	s := pegTestServer(t, fx)
	s.Substance = &stubListingGate{allow: map[string]bool{}}
	dust := "0.80"
	rows := []AssetDetail{{AssetID: pegTestAUDD, PriceUSD: &dust}}
	ctx := context.Background()
	s.applySubstanceGateToListing(ctx, rows, true)
	s.fillDeclaredPegPricesInListing(ctx, rows)
	settleListingThinRows(rows)
	if rows[0].PriceUSD == nil || *rows[0].PriceUSD != "0.655" || rows[0].PriceBasis != priceBasisDeclaredPeg {
		t.Fatalf("price=%v basis=%q, want the declared peg over the thin market", strPtr(rows[0].PriceUSD), rows[0].PriceBasis)
	}
	if rows[0].ThinMarket {
		t.Error("a peg-filled row must not read as thin_market")
	}
}

// TestThinPriceDerivesNoValuation: no market cap, series or listing
// valuation status may be computed from a thin price.
func TestThinPriceDerivesNoValuation(t *testing.T) {
	price := "0.16"
	row := AssetDetail{AssetID: "native", Code: "XLM", Decimals: 7, PriceUSD: &price, ThinMarket: true}
	precise := observedSupply(map[string]string{"native": "100000000000000000"})
	(&Server{}).fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{})
	if row.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %v from a thin price", *row.MarketCapUSD)
	}
	if priceSeriesPublishable(&row) {
		t.Error("a thin price must not publish a price series")
	}
	row.ThinMarket = false
	if !priceSeriesPublishable(&row) {
		t.Error("control: a cleared market price publishes its series")
	}

	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Options: Options{VerifiedCurrencies: cat}}
	anchor := AssetDetail{AssetID: "SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH", PriceUSD: &price}
	if s.listingValuationCandidate(&anchor) || anchor.ListingValuation == nil ||
		anchor.ListingValuation.Status != ListingValuationMarketPriceObserved {
		t.Fatalf("control: a cleared price must read as market_price_observed, got %+v", anchor.ListingValuation)
	}
	thinAnchor := AssetDetail{AssetID: anchor.AssetID, PriceUSD: &price, ThinMarket: true}
	if !s.listingValuationCandidate(&thinAnchor) || thinAnchor.ListingValuation != nil {
		t.Errorf("a thin price was taken as an observed market price: %+v", thinAnchor.ListingValuation)
	}
}
