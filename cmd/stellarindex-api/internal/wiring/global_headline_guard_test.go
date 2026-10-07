package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// headlineVWAPStore is a GlobalPriceStore returning canned rows so the
// headline tier's guard handling is testable without a database.
type headlineVWAPStore struct {
	latest   timescale.Vwap1mRow
	trailing []timescale.Vwap1mRow
}

func (s headlineVWAPStore) LatestClosedVWAP1mForPair(context.Context, canonical.Pair) (timescale.Vwap1mRow, error) {
	return s.latest, nil
}

func (s headlineVWAPStore) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return s.trailing, nil
}

func (headlineVWAPStore) LatestAggregatorPricesForPair(
	context.Context, canonical.Asset, canonical.Asset, []string,
) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func headlineRow(minutesAgo int, vwap string) timescale.Vwap1mRow {
	return timescale.Vwap1mRow{
		Bucket:     time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute),
		VWAP:       vwap,
		TradeCount: 9,
		Sources:    []string{"sdex"},
	}
}

func headlineReader(store headlineVWAPStore) GlobalPriceReader {
	return GlobalPriceReader{S: store, PKPairFor: canonical.NewPair}
}

// The GlobalAssetView headline has no stale flag, so a bucket the guard
// could not validate must not be served as the vwap_native headline:
// the reader reports no data and the headline falls through to its other
// tiers, as the point-in-time guard does for the same reason.
func TestGlobalPriceReader_UnvalidatedBucketIsNoHeadline(t *testing.T) {
	base, quote := canonical.NativeAsset(), mustFiatUSD(t)
	reader := headlineReader(headlineVWAPStore{latest: headlineRow(0, "100.0")})
	vwap, _, _, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil {
		t.Fatalf("LatestVWAP: %v", err)
	}
	if ok {
		t.Fatalf("headline served %q from a bucket with no trailing baseline; want ok=false", vwap)
	}
}

func headlineTrailing() []timescale.Vwap1mRow {
	var trailing []timescale.Vwap1mRow
	for i := 3; i < 16; i++ {
		trailing = append(trailing, headlineRow(i, "1.0"))
	}
	return trailing
}

// The headline has no marker for a guard-held bucket either, so an outlier
// the guard replaces with last-known-good must not be served as vwap_native
// with a frozen price_as_of: the reader reports no data and the headline
// falls through to its other tiers.
func TestGlobalPriceReader_OutlierIsNoHeadline(t *testing.T) {
	base, quote := canonical.NativeAsset(), mustFiatUSD(t)
	reader := headlineReader(headlineVWAPStore{latest: headlineRow(0, "100.0"), trailing: headlineTrailing()})
	vwap, _, _, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil {
		t.Fatalf("LatestVWAP: %v", err)
	}
	if ok {
		t.Fatalf("headline served held value %q for an outlier minute; want ok=false", vwap)
	}
}

// An in-band candidate still serves, at its own bucket close.
func TestGlobalPriceReader_InBandBucketServesAtItsClose(t *testing.T) {
	base, quote := canonical.NativeAsset(), mustFiatUSD(t)
	reader := headlineReader(headlineVWAPStore{latest: headlineRow(0, "1.1"), trailing: headlineTrailing()})
	vwap, asOf, _, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("LatestVWAP: ok=%v err=%v, want the candidate bucket", ok, err)
	}
	if vwap != "1.1" {
		t.Fatalf("vwap = %s, want candidate 1.1", vwap)
	}
	if want := headlineRow(0, "1.1").Bucket.Add(time.Minute); !asOf.Equal(want) {
		t.Fatalf("asOf = %v, want the candidate bucket's close %v", asOf, want)
	}
}

func mustFiatUSD(t *testing.T) canonical.Asset {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	return usd
}
