package v1

import (
	"testing"
	"time"
)

func TestVendorOnlySources(t *testing.T) {
	for _, tc := range []struct {
		srcs []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"coingecko_daily"}, true},
		{[]string{"coingecko_daily", "poloniex_via_btc"}, true},
		{[]string{"coingecko_daily", "sdex"}, false},
		{[]string{"kraken"}, false},
		{[]string{"not_a_registered_source"}, false},
		{[]string{"coingecko_daily", "not_a_registered_source"}, false},
	} {
		if got := vendorOnlySources(tc.srcs); got != tc.want {
			t.Errorf("vendorOnlySources(%v) = %v, want %v", tc.srcs, got, tc.want)
		}
	}
}

// A vendor-only point's stored volume is the carrier row's amount, not fills: the wire drops it. A venue point keeps its.
func TestHistoryWirePoint_VendorOnlyDropsVolume(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	vol := "0.5"
	vendor := historyWirePoint(HistoryPoint{Bucket: t0, VWAP: "0.0030", VolumeUSD: &vol, Sources: []string{"coingecko_daily"}})
	if vendor.VUSD != nil {
		t.Errorf("vendor-only v_usd = %q, want absent", *vendor.VUSD)
	}
	if len(vendor.Sources) != 1 || vendor.Sources[0] != "coingecko_daily" {
		t.Errorf("vendor-only sources = %v, want [coingecko_daily]", vendor.Sources)
	}
	venue := historyWirePoint(HistoryPoint{Bucket: t0, VWAP: "0.0030", VolumeUSD: &vol, Sources: []string{"kraken"}})
	if venue.VUSD == nil || *venue.VUSD != vol {
		t.Errorf("venue v_usd = %v, want %q", venue.VUSD, vol)
	}
}

func TestWithoutVendorVolumes(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	in := []OHLCSeriesBar{
		{T: WireTime(t0), O: "1", H: "2", L: "0.5", C: "1.5", VBase: "100", VQuote: "150", N: 3, Sources: []string{"sdex"}},
		{T: WireTime(t0.Add(time.Hour)), O: "1.5", H: "1.5", L: "1.5", C: "1.5", VBase: "10000000", VQuote: "15000000", N: 1, Sources: []string{"coingecko_daily"}},
	}
	out := withoutVendorVolumes(in)
	if &out[0] == &in[0] {
		t.Fatal("input mutated in place; a shared fixture must survive the call")
	}
	if in[1].VBase != "10000000" || in[1].N != 1 {
		t.Errorf("input changed: %+v", in[1])
	}
	if out[0].VBase != "100" || out[0].VQuote != "150" || out[0].N != 3 {
		t.Errorf("venue bar changed: %+v", out[0])
	}
	if out[1].VBase != "0" || out[1].VQuote != "0" || out[1].N != 0 {
		t.Errorf("vendor bar volumes = %q/%q/%d, want 0/0/0", out[1].VBase, out[1].VQuote, out[1].N)
	}
	if out[1].O != "1.5" || out[1].C != "1.5" || len(out[1].Sources) != 1 {
		t.Errorf("vendor bar lost its prices or provenance: %+v", out[1])
	}
	venueOnly := in[:1]
	if got := withoutVendorVolumes(venueOnly); &got[0] != &venueOnly[0] {
		t.Error("a series with no vendor-only bar must be returned as is")
	}
}
