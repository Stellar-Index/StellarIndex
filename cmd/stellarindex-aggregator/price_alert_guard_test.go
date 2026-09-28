package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// discardLogger is a no-op logger so the guard's warn branch (which calls
// pair.String()) is exercised without noise.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The price-alert evaluator reads the same raw prices_1m closed bucket
// (LatestClosedVWAP1mForPair) as the two API serving paths — a bare
// Σ(quote)/Σ(base) CAGG that bypasses the σ-outlier / min-volume / freeze
// filters. Without the serving-sanity guard, a fat-finger / manipulation
// print in the served minute would fire a SPURIOUS customer price alert.
// These tests mirror the pricingguard package cases for the price-alert
// path: normal→unchanged, fat-finger→last-known-good, thin→pass-through.

// fakeAlertVWAPStore is a priceAlertVWAPStore returning canned rows so the
// reader (and its guard call) is testable without a database.
type fakeAlertVWAPStore struct {
	latest      timescale.Vwap1mRow
	latestErr   error
	trailing    []timescale.Vwap1mRow
	trailingErr error
}

func (f fakeAlertVWAPStore) LatestClosedVWAP1mForPair(context.Context, canonical.Pair) (timescale.Vwap1mRow, error) {
	return f.latest, f.latestErr
}

func (f fakeAlertVWAPStore) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return f.trailing, f.trailingErr
}

// alertRow builds a closed-bucket row at a given minutes-ago offset.
func alertRow(minutesAgo int, vwap string) timescale.Vwap1mRow {
	return timescale.Vwap1mRow{
		Bucket:  time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute),
		VWAP:    vwap,
		Sources: []string{"soroswap"},
	}
}

// steadyAlertRows returns n newest-first flat-1.0 trailing buckets starting
// one minute older than the candidate (which is at minutesAgo=0).
func steadyAlertRows(n int) []timescale.Vwap1mRow {
	rows := make([]timescale.Vwap1mRow, n)
	for i := 0; i < n; i++ {
		rows[i] = alertRow(i+1, "1.0")
	}
	return rows
}

// alertUSDPair returns the native + fiat:USD assets the reader turns into a
// pair internally.
func alertUSDAssets(t *testing.T) (base, quote canonical.Asset) {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	return canonical.NativeAsset(), usd
}

func TestPriceAlertReader_NormalBucketServedUnchanged(t *testing.T) {
	base, quote := alertUSDAssets(t)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "1.01"), trailing: steadyAlertRows(12)},
		logger: discardLogger(),
	}
	price, bucketClose, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price != "1.01" {
		t.Fatalf("served price = %s, want the candidate 1.01 (byte-identical)", price)
	}
	// bucketClose is the candidate bucket + 1 minute.
	wantClose := alertRow(0, "1.01").Bucket.Add(time.Minute)
	if !bucketClose.Equal(wantClose) {
		t.Fatalf("bucketClose = %v, want candidate close %v", bucketClose, wantClose)
	}
}

func TestPriceAlertReader_FatFingerServesLKGNoSpuriousAlert(t *testing.T) {
	// A 100x fat-finger in the latest bucket must NOT be surfaced to the
	// evaluator (it would fire a spurious alert). The guard swaps in the
	// newest clean trailing bucket (last-known-good).
	base, quote := alertUSDAssets(t)
	trailing := steadyAlertRows(12)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "100.0"), trailing: trailing},
		logger: discardLogger(),
	}
	price, bucketClose, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price != "1.0" {
		t.Fatalf("served price = %s, want last-known-good 1.0 (no spurious alert)", price)
	}
	// The served close is the older LKG bucket, never the manipulated minute.
	wantClose := trailing[0].Bucket.Add(time.Minute)
	if !bucketClose.Equal(wantClose) {
		t.Fatalf("bucketClose = %v, want last-known-good close %v", bucketClose, wantClose)
	}
}

func TestPriceAlertReader_ThinHistoryWiderFiniteBand(t *testing.T) {
	// Post-M11(b): a thin baseline no longer fails fully open. A gross
	// (>10x) print is caught and served as last-known-good (no spurious
	// alert); a within-order-of-magnitude value still passes through.
	base, quote := alertUSDAssets(t)
	gross := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "999.0"), trailing: steadyAlertRows(3)},
		logger: discardLogger(),
	}
	price, _, ok, err := gross.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price != "1.0" {
		t.Fatalf("served price = %s, want last-known-good 1.0 (thin-history gross print caught)", price)
	}

	moderate := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "5.0"), trailing: steadyAlertRows(3)},
		logger: discardLogger(),
	}
	price, _, ok, err = moderate.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price != "5.0" {
		t.Fatalf("served price = %s, want candidate 5.0 (within-band thin-history pass-through)", price)
	}
}

// fakeAlertDecimalsLookup is an aggregate.DecimalsLookup with a fixed
// asset->decimals table, for testing without the real decimals cache.
type fakeAlertDecimalsLookup map[string]int

func (f fakeAlertDecimalsLookup) Lookup(assetID string) (int, bool) {
	d, ok := f[assetID]
	return d, ok
}

// TestPriceAlertReader_NonstandardDecimalsCorrected proves #748: a
// confirmed non-7-decimal base leg (the runbook's real CC2RB… incident,
// decimals()=9 vs USDC's 7, raw ratio 41.32) must be corrected the same
// way /v1/price's TestPrice_NonstandardDecimals_NormalizesFlaggedBaseLeg
// corrects it (K = 10^(9-7) = 100 -> 4132), not compared/served raw.
func TestPriceAlertReader_NonstandardDecimalsCorrected(t *testing.T) {
	base, quote := alertUSDAssets(t)
	trailing := make([]timescale.Vwap1mRow, 12)
	for i := range trailing {
		trailing[i] = alertRow(i+1, "41.32")
	}
	reader := priceAlertVWAPReader{
		store:    fakeAlertVWAPStore{latest: alertRow(0, "41.32"), trailing: trailing},
		logger:   discardLogger(),
		decimals: fakeAlertDecimalsLookup{base.String(): 9},
	}
	price, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price != "4132.0000" {
		t.Fatalf("served price = %s, want decimals-corrected 4132 (raw 41.32 x 10^(9-7)), matching /v1/price", price)
	}
}

// TestPriceAlertReader_SubEpsilonCorrectedPriceStaysNonZero proves the
// slice-C03 must_fix: a corrected price below 1e-10 must not truncate to
// "0". Base decimals 7, quote decimals 18 -> divide by 10^11: raw "1.0"
// corrects to the exact decimal 0.00000000001 (1e-11), which a fixed
// FloatString(10) render would truncate to "0".
func TestPriceAlertReader_SubEpsilonCorrectedPriceStaysNonZero(t *testing.T) {
	base, quote := alertUSDAssets(t)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "1.0"), trailing: steadyAlertRows(12)},
		logger: discardLogger(),
		decimals: fakeAlertDecimalsLookup{
			base.String():  7,
			quote.String(): 18,
		},
	}
	price, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price; got ok=%v err=%v", ok, err)
	}
	if price == "0" {
		t.Fatalf("served price truncated a positive corrected price to 0")
	}
	if price != "0.000000000010" {
		t.Fatalf("served price = %s, want the exact corrected decimal 0.000000000010 (1e-11)", price)
	}
}

func TestPriceAlertReader_NoClosedBucketIsBenignNoOp(t *testing.T) {
	// sql.ErrNoRows (no closed bucket in scope) stays ok=false, nil — the
	// evaluator skips the pair. The guard must not run (and does not).
	base, quote := alertUSDAssets(t)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latestErr: sql.ErrNoRows},
		logger: discardLogger(),
	}
	_, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if ok || err != nil {
		t.Fatalf("no closed bucket must be ok=false,nil; got ok=%v err=%v", ok, err)
	}
}

// A bucket the guard could not validate (no trailing baseline — a pair's
// first minute, or its first after a day dormant) is the fail-open case
// the guard serves only as low-confidence. An alert has no stale flag to
// carry that doubt, so it must not fire off it (#677): a single
// manipulated first print would otherwise deliver a customer webhook.
func TestPriceAlertReader_UnvalidatedBucketDoesNotFire(t *testing.T) {
	base, quote := alertUSDAssets(t)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "100.0")},
		logger: discardLogger(),
	}
	price, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil {
		t.Fatalf("LatestVWAP: %v", err)
	}
	if ok {
		t.Fatalf("served %q for an alert off a bucket with no trailing baseline; want ok=false", price)
	}
}
