package pricingguard

import (
	"context"
	"math/big"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFailedFloor_NamesTheFloor pins the `floor` label: persistence
// floors first, then volume, split by whether the market was valued.
func TestFailedFloor_NamesTheFloor(t *testing.T) {
	pol := testPolicy()
	rat := func(n int64) *big.Rat { return new(big.Rat).SetInt64(n) }
	const span = 6 * 3600
	cases := []struct {
		name                   string
		vol                    *big.Rat
		buckets, valued, spanS int64
		want                   SubstanceFloor
	}{
		{"clears every floor", rat(1000), 20, 20, span, FloorNone},
		{"one burst: fails every floor, buckets named", rat(0), 1, 0, 0, FloorBuckets},
		{"enough buckets, too short", rat(5000), 20, 20, span - 1, FloorSpan},
		{"valued and thin", rat(999), 20, 20, span, FloorVolume},
		{"half valued is still valued", rat(999), 20, 10, span, FloorVolume},
		{"mostly unvalued", rat(999), 20, 9, span, FloorVolumeUnvalued},
		{"SEP-41/SEP-41: never valued", rat(0), 800, 0, 22 * 3600, FloorVolumeUnvalued},
	}
	for _, tc := range cases {
		if got := FailedFloor(tc.vol, tc.buckets, tc.valued, tc.spanS, pol); got != tc.want {
			t.Errorf("%s: FailedFloor = %q, want %q", tc.name, got, tc.want)
		}
		if ok := SubstanceOK(tc.vol, tc.buckets, tc.spanS, pol); ok != (tc.want == FloorNone) {
			t.Errorf("%s: SubstanceOK = %v disagrees with FailedFloor %q", tc.name, ok, tc.want)
		}
	}
}

// TestSubstanceGate_WithheldCountCarriesTheFloor: the counter says which
// floor refused the pair, from a fresh measurement and from the cache.
func TestSubstanceGate_WithheldCountCarriesTheFloor(t *testing.T) {
	const surface = "floor_label_test"
	asset := mustAsset(t, "SHRT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	pair, err := canonical.NewPair(asset, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	short := timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 40, SpanSeconds: 3600, ValuedBuckets: 40}
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): short}},
		SubstanceGateOptions{Policy: testPolicy()})
	counter := obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface, string(FloorSpan))
	for range 2 {
		if gate.Allowed(context.Background(), asset, canonical.NativeAsset(), surface) {
			t.Fatal("a one-hour market cleared the six-hour span floor")
		}
	}
	var pb dto.Metric
	if err := counter.Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 2 {
		t.Errorf("floor=%q series = %v, want 2", FloorSpan, got)
	}
	if got := withheldFor(t, surface); got != 2 {
		t.Errorf("withheld counted %v across all floors, want 2", got)
	}
}

// TestSubstanceGate_UnvaluedMarketIsWithheldAndNamed (GH-1052): a market
// with real persistence but no USD valuation — the SEP-41/SEP-41 shape,
// 800 buckets over 22h — stays withheld (an unvaluable volume cannot be
// verified, and waiving the floor would admit the self-minted pair the
// gate exists for), but under its own floor, not as a thin market.
func TestSubstanceGate_UnvaluedMarketIsWithheldAndNamed(t *testing.T) {
	a := mustAsset(t, "TOKA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	b := mustAsset(t, "TOKB-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	pair, err := canonical.NewPair(a, b)
	if err != nil {
		t.Fatal(err)
	}
	unvalued := timescale.MarketSubstance{VolumeUSD: "0", Buckets: 800, SpanSeconds: 22 * 3600}
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): unvalued}},
		SubstanceGateOptions{Policy: testPolicy()})
	allowed, measured, floor := gate.Probe(context.Background(), a, b)
	if allowed || !measured || floor != FloorVolumeUnvalued {
		t.Errorf("Probe = (allowed %v, measured %v, floor %q), want (false, true, %q)",
			allowed, measured, floor, FloorVolumeUnvalued)
	}
}
