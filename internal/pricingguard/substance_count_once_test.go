package pricingguard

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// withheldFor sums every obs.PriceServeSubstanceWithheldTotal series
// carrying surface=surface, whatever its other labels.
func withheldFor(t *testing.T, surface string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		obs.PriceServeSubstanceWithheldTotal.Collect(ch)
		close(ch)
	}()
	var sum float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatal(err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "surface" && l.GetValue() == surface {
				sum += pb.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// TestAssetSubstanceVerdict_CountsTheAssetOnce (GH-1054): the listing
// asks one question per asset — may its USD price be served — by
// probing XLM, fiat:USD and each declared peg. The withheld counter
// counted every refused probe, so one withheld row read as three, and
// the figure moved when an operator declared another peg.
func TestAssetSubstanceVerdict_CountsTheAssetOnce(t *testing.T) {
	const surface = "count_once_test"
	asset := mustAsset(t, "THIN-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	peg := mustAsset(t, "USDX-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}},
		SubstanceGateOptions{Policy: testPolicy()})

	before := withheldFor(t, surface)
	allowed, measured := AssetSubstanceVerdict(context.Background(), gate, asset, []canonical.Asset{peg}, surface)
	if allowed || !measured {
		t.Fatalf("AssetSubstanceVerdict = (%v, %v), want (false, true): no quote has a market", allowed, measured)
	}
	if got := withheldFor(t, surface) - before; got != 1 {
		t.Errorf("one withheld asset counted %v times, want 1 (three quotes were probed)", got)
	}
}

// TestAssetSubstanceVerdict_AllowedAssetIsNotCounted: an asset refused
// on its XLM quote but cleared on fiat:USD is SERVED, so it must not
// show up as withheld at all.
func TestAssetSubstanceVerdict_AllowedAssetIsNotCounted(t *testing.T) {
	const surface = "count_allowed_test"
	asset := mustAsset(t, "DEEP-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	deep := timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 500, SpanSeconds: 20 * 3600}
	pair, err := canonical.NewPair(asset, fiatUSD)
	if err != nil {
		t.Fatal(err)
	}
	gate := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): deep}},
		SubstanceGateOptions{Policy: testPolicy()})

	before := withheldFor(t, surface)
	allowed, _ := AssetSubstanceVerdict(context.Background(), gate, asset, nil, surface)
	if !allowed {
		t.Fatal("asset with a deep fiat:USD market was withheld")
	}
	if got := withheldFor(t, surface) - before; got != 0 {
		t.Errorf("a served asset was counted %v times as withheld, want 0", got)
	}
}
