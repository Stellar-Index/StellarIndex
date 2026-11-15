package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A cap built on a trustline floor is a floor; a cap on a full reading is not.
func TestFillRowMarketCap_FloorSupplyMarksTheCapALowerBound(t *testing.T) {
	const asset = "FOO-GAFOO"
	price := "1.00"
	cases := []struct {
		name    string
		precise map[string]timescale.SupplyObservation
		broad   map[string]string
		want    bool
	}{
		{"observation", observedSupply(map[string]string{asset: "10000000"}), nil, false},
		{"trustline floor", nil, map[string]string{asset: "10000000"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			row := AssetDetail{AssetID: asset, Code: "FOO", Decimals: 7, PriceUSD: &price}
			s.fillRowMarketCap(context.Background(), &row, tc.precise, nil, tc.broad, nil)
			if row.MarketCapUSD == nil {
				t.Fatal("market_cap_usd not published")
			}
			if row.MarketCapLowerBound != tc.want {
				t.Errorf("market_cap_lower_bound = %v, want %v", row.MarketCapLowerBound, tc.want)
			}
		})
	}
}

// A candidate read that failed is not "no route": the detail must not cache a clean null price.
func TestFillTransitivePrice_FailedReadMarksTheDetailUnread(t *testing.T) {
	asset, err := canonical.ParseAsset(withheldTestAsset)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		pricer *stubPricer
		want   bool
	}{
		"read failed": {&stubPricer{err: errors.New("db down")}, true},
		"no route":    {&stubPricer{ok: false}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{TransitivePricer: tc.pricer})
			var detail AssetDetail
			s.fillTransitivePrice(context.Background(), &detail, asset, asset.String())
			if detail.PriceUSD != nil {
				t.Errorf("price_usd = %q, want none", *detail.PriceUSD)
			}
			if detail.extensionUnread != tc.want {
				t.Errorf("extensionUnread = %v, want %v", detail.extensionUnread, tc.want)
			}
		})
	}
}

// staleUSDReader serves every pair with the reader's stale verdict set.
type staleUSDReader struct{ withheldUSDReader }

func (staleUSDReader) LatestPrice(_ context.Context, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{AssetID: asset.String(), Quote: quote.String(), Price: "0.25"}, []string{"sdex"}, true, nil
}

// The price reader's stale verdict reaches the detail's stale flag.
func TestPopulatePriceUSD_ReaderStaleVerdictMarksTheDetailStale(t *testing.T) {
	asset, err := canonical.ParseAsset(withheldTestAsset)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Options: Options{Prices: staleUSDReader{}}}
	var detail AssetDetail
	s.populatePriceUSD(context.Background(), &detail, asset)
	if detail.PriceUSD == nil {
		t.Fatal("price_usd not served")
	}
	// Stale, and still cacheable: an old price is not a failed read.
	if stale, degraded := detailFreshness(false, &detail, false, false); !detail.priceStale || !stale || degraded {
		t.Errorf("priceStale = %v, stale = %v, degraded = %v, want true, true, false", detail.priceStale, stale, degraded)
	}
}

// oldVWAPReader answers only the VWAP tier, with a bucket of the given age.
type oldVWAPReader struct{ age time.Duration }

func (r oldVWAPReader) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	return "0.11", time.Now().Add(-r.age), 100, []string{"binance"}, true, nil
}

func (oldVWAPReader) LatestAggregatorPrices(context.Context, canonical.Asset, canonical.Asset, []string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (oldVWAPReader) LookupTriangulated(context.Context, canonical.Asset, canonical.Asset, time.Duration) (string, time.Time, bool, error) {
	return "", time.Time{}, false, nil
}

// A VWAP past its freshness ceiling is served when no lower tier answers; it is then reported stale.
func TestPopulateGlobalCryptoPrice_OldVWAPIsReportedStale(t *testing.T) {
	vc := &currency.VerifiedCurrency{Ticker: "XLM", Slug: "stellar", Name: "Stellar Lumens", Class: currency.ClassCrypto}
	for name, tc := range map[string]struct {
		age  time.Duration
		want bool
	}{
		"three hours old": {3 * time.Hour, true},
		"one minute old":  {time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{})
			s.globalPrice = oldVWAPReader{age: tc.age}
			s.globalPriceOpts = aggregate.GlobalPriceOptions{MaxVWAPAge: 10 * time.Minute}
			ctx, rep := withReadReport(context.Background())
			view := s.populateGlobalCryptoPrice(ctx, GlobalAssetView{}, vc)
			if view.PriceUSD == nil {
				t.Fatal("price_usd not served")
			}
			if rep.stale.Load() != tc.want {
				t.Errorf("stale reported = %v, want %v", rep.stale.Load(), tc.want)
			}
		})
	}
}

// A suppressed price takes its stale mark with it.
func TestSuppressScamIssuerPricing_ClearsThePriceStaleMark(t *testing.T) {
	price := "0.25"
	d := AssetDetail{PriceUSD: &price, priceStale: true, IssuerDirectoryTags: []string{"malicious"}}
	suppressScamIssuerPricing(&d)
	if d.PriceUSD != nil || d.priceStale {
		t.Errorf("price = %v, priceStale = %v, want neither", d.PriceUSD, d.priceStale)
	}
}

// unmeasuredGate never rules: every probe fails to measure.
type unmeasuredGate struct{ stubListingGate }

func (*unmeasuredGate) Probe(context.Context, canonical.Asset, canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return false, false, pricingguard.FloorNone
}

// A gate that did not rule withholds the row price, and the null is not a clean "no price".
func TestApplyAssetRowToDetail_UnmeasuredGateMarksTheDetailUnread(t *testing.T) {
	asset, err := canonical.ParseAsset(withheldTestAsset)
	if err != nil {
		t.Fatal(err)
	}
	price := "0.25"
	for name, tc := range map[string]struct {
		gate PriceSubstanceGate
		want bool
	}{
		"gate did not rule": {&unmeasuredGate{}, true},
		"gate refused":      {&stubListingGate{allow: map[string]bool{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{Substance: tc.gate})
			var detail AssetDetail
			s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{PriceUSD: &price}, nil, asset.String())
			if detail.PriceUSD != nil {
				t.Errorf("price_usd = %q, want withheld", *detail.PriceUSD)
			}
			if detail.extensionUnread != tc.want {
				t.Errorf("extensionUnread = %v, want %v", detail.extensionUnread, tc.want)
			}
		})
	}
}

// The catalogue listing reports a row priced from an old VWAP as stale, not as unmeasured.
func TestFillAndRankCatalogueRows_OldVWAPIsReportedStale(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	vc, ok := cat.LookupByTicker("XLM")
	if !ok {
		t.Fatal("no XLM in the embedded catalogue")
	}
	s := New(Options{VerifiedCurrencies: cat})
	s.globalPrice = oldVWAPReader{age: 3 * time.Hour}
	s.globalPriceOpts = aggregate.GlobalPriceOptions{MaxVWAPAge: 10 * time.Minute}
	rows := []AssetDetail{{Slug: vc.Slug}}
	unmeasured, stale := s.fillAndRankCatalogueRows(context.Background(), rows, assetListFilters{})
	if rows[0].PriceUSD == nil {
		t.Fatal("row not priced")
	}
	if unmeasured || !stale {
		t.Errorf("unmeasured = %v, stale = %v, want false, true", unmeasured, stale)
	}
}
