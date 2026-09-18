// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// Regression suite for F019/F032 — the two call sites the pair-aware
// conversion left behind: /v1/twap and the chart/since-inception series
// gate both asked the scam gate about the BASE leg alone.
//
// Naming a directory-flagged issuer as the QUOTE describes the SAME
// market as naming it as the base: the aggregate reads fold both stored
// directions and invert the flipped one, so the number served on the
// swapped request is the withheld price to full precision. Withholding
// one orientation and publishing the other is not a gate.
//
// As in vwap_scam_quote_leg_test.go, these exercise the REAL
// *pricingguard.ScamGate over a recording stub directory rather than a
// hand-written fake: a fake that only answers the base-only question
// would let the test pass against the base-only call site, which is the
// vacuity this whole class came from. What the directory is ASKED is
// the proof that the quote leg reached the gate at all.

// chartQuoteLegSeries backs BOTH series reads with one real bucket, so
// an ungated request answers a 200 carrying an actual price rather than
// 404-ing for lack of data and passing vacuously.
//
// `points` feeds the windowed read behind /v1/chart (the bucket sits
// inside the 24h window); `pointsByPair` feeds the since-inception read.
func chartQuoteLegSeries(t *testing.T, pair canonical.Pair) *stubHistoryReader {
	t.Helper()
	pt := v1.HistoryPoint{
		Bucket: time.Now().UTC().Add(-time.Hour).Truncate(15 * time.Minute),
		VWAP:   "0.1600000000",
	}
	return &stubHistoryReader{
		points:       []v1.HistoryPoint{pt},
		pointsByPair: map[string][]v1.HistoryPoint{pair.String(): {pt}},
	}
}

func TestTWAPWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + flagged.String(): {scamTestTrade(t, native, flagged)},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote="+flagged.String())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/twap?base=native&quote=%s status = %d, want 404 — moving the "+
			"flagged asset to the quote side republishes the withheld market's price "+
			"as its exact reciprocal (F019). Body: %s", flagged.String(), resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(dir.asked) == 0 || dir.asked[0] != scamQuoteLegIssuer {
		t.Fatalf("directory asked about %v, want the QUOTE leg's issuer %q — a 404 that "+
			"never consulted the quote leg came from something other than the "+
			"withholding decision", dir.asked, scamQuoteLegIssuer)
	}
}

// TestTWAPServesWhenNeitherLegIsFlagged is the blast-radius guard:
// folding the quote leg in must not withhold ordinary markets.
func TestTWAPServesWhenNeitherLegIsFlagged(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote="+usdc.String())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/twap for an unflagged pair returned %d, want 200 — the pair-aware "+
			"gate must withhold only flagged issuers. Body: %s", resp.StatusCode, body)
	}
}

// TestChartWithholdsWhenTheQuoteLegIsScamFlagged covers the series
// surface, where the leak is worse than on a point: an ungated chart
// hands over the whole trajectory of the flagged market, inverted.
//
// The gate must also short-circuit BEFORE the read, so the flagged
// issuer's series never leaves the store.
func TestChartWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	native, _ := canonical.ParseAsset("native")
	pair, err := canonical.NewPair(native, flagged)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	reader := chartQuoteLegSeries(t, pair)
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote="+flagged.String()+"&timeframe=24h")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/chart?base=native&quote=%s status = %d, want 404 — the flagged "+
			"market's whole trajectory is republished, inverted, by naming it as the "+
			"quote (F019). Body: %s", flagged.String(), resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(dir.asked) == 0 || dir.asked[0] != scamQuoteLegIssuer {
		t.Fatalf("directory asked about %v, want the QUOTE leg's issuer %q", dir.asked, scamQuoteLegIssuer)
	}
	if reader.lastCall.granularity != "" {
		t.Errorf("HistoryPointsInRange was called (granularity=%q) despite the withholding "+
			"— the gate must short-circuit BEFORE the read", reader.lastCall.granularity)
	}
}

// TestHistorySinceInceptionWithholdsWhenTheQuoteLegIsScamFlagged pins
// the SECOND caller of seriesWithheldForScam. One helper, two routes:
// fixing only the one a test remembered is how this class recurs.
func TestHistorySinceInceptionWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	native, _ := canonical.ParseAsset("native")
	pair, err := canonical.NewPair(native, flagged)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	reader := chartQuoteLegSeries(t, pair)
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+flagged.String()+"&granularity=1d")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/history/since-inception with a flagged QUOTE leg status = %d, want 404 "+
			"— it serves the same bucketed VWAP chain as /v1/chart?timeframe=all. Body: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
}

// TestChartServesWhenNeitherLegIsFlagged is the blast-radius guard for
// the series gate.
func TestChartServesWhenNeitherLegIsFlagged(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")
	pair, err := canonical.NewPair(native, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	reader := chartQuoteLegSeries(t, pair)
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote="+usdc.String()+"&timeframe=24h")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/chart for an unflagged pair returned %d, want 200 — the pair-aware "+
			"gate must withhold only flagged issuers. Body: %s", resp.StatusCode, body)
	}
}
