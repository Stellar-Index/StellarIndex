// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// priceAtStub honours maxStaleness like the wired producer (a bucket staler than the bound is refused), unless
// ignoreStaleness models the out-of-contract reader the handler's own re-check defends against.
type priceAtStub struct {
	value           string
	bucketAt        time.Time
	resSec          int
	err             error
	ignoreStaleness bool
	// gotStaleness, when non-nil, records the bound the handler passed.
	gotStaleness *time.Duration
}

func (s priceAtStub) PriceAt(_ context.Context, _ canonical.Pair, ts time.Time, maxStaleness time.Duration) (string, time.Time, int, error) {
	if s.gotStaleness != nil {
		*s.gotStaleness = maxStaleness
	}
	res := s.resSec
	if res == 0 {
		res = 60
	}
	if s.err == nil && !s.ignoreStaleness && ts.Sub(s.bucketAt) > maxStaleness {
		return "", time.Time{}, 0, ErrPriceAtUnavailable
	}
	return s.value, s.bucketAt, res, s.err
}

// TestHandlePriceAt pins that a historical instant serves the closed bucket at-or-before it with the bucket's own
// observed_at; a bucket older than the 24h cap 404s rather than fabricate continuity; future or missing ts is 400.
func TestHandlePriceAt(t *testing.T) {
	ts := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	near := ts.Add(-45 * time.Minute)

	s := &Server{Options: Options{PriceAt: priceAtStub{value: "0.128", bucketAt: near}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/price/at?asset=native&ts="+ts.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"price":"0.128"`, `"price_type":"vwap"`, near.Format(time.RFC3339)} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}

	// Nearest bucket beyond the lookback cap → 404, not a stale lie. The
	// reader enforces the bound it is handed, so the cap the handler
	// passes IS the honesty cap.
	var gotStaleness time.Duration
	stale := ts.Add(-30 * 24 * time.Hour)
	s = &Server{Options: Options{PriceAt: priceAtStub{value: "0.128", bucketAt: stale, gotStaleness: &gotStaleness}}}
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != 404 {
		t.Errorf("beyond-cap bucket: status %d, want 404", rec.Code)
	}
	if gotStaleness != 24*time.Hour {
		t.Errorf("handler passed maxStaleness %v to the reader, want the 24h honesty cap", gotStaleness)
	}

	// Defence in depth: a reader that ignores the bound still cannot
	// get a beyond-cap bucket served.
	s = &Server{Options: Options{PriceAt: priceAtStub{value: "0.128", bucketAt: stale, ignoreStaleness: true}}}
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != 404 {
		t.Errorf("beyond-cap bucket from a bound-ignoring reader: status %d, want 404", rec.Code)
	}

	// Future ts → 400.
	future := time.Now().Add(48 * time.Hour).Format(time.RFC3339)
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, httptest.NewRequest(http.MethodGet, "/v1/price/at?asset=native&ts="+future, nil))
	if rec.Code != 400 {
		t.Errorf("future ts: status %d, want 400", rec.Code)
	}

	// Missing ts → 400 with steering.
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, httptest.NewRequest(http.MethodGet, "/v1/price/at?asset=native", nil))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "price/tip") {
		t.Errorf("missing ts: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestHandlePriceAt_WithheldDistinctFromNotFound pins that ErrPriceWithheld (the pair HAS a
// closed bucket but the substance/scam gate refuses to publish it) 404s with the distinct
// errors/price-withheld type, not errors/price-not-found, as handlePrice and handlePriceTip do.
func TestHandlePriceAt_WithheldDistinctFromNotFound(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{Options: Options{PriceAt: priceAtStub{err: ErrPriceWithheld}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/price/at?asset=native&ts="+ts.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"https://api.stellarindex.io/errors/price-withheld"`) {
		t.Errorf("body did not carry the distinct price-withheld type: %s", body)
	}
	if strings.Contains(body, `"type":"https://api.stellarindex.io/errors/price-not-found"`) {
		t.Errorf("body used the generic price-not-found type though the reader withheld the price: %s", body)
	}
}

// TestHandlePriceAt_GuardedIsWithheldNotNoData: a bucket the serving-sanity guard refused exists, so the 404 is price-
// withheld with the guard's wording, not price-not-found's "no closed bucket" or the thin-market sentence.
func TestHandlePriceAt_GuardedIsWithheldNotNoData(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{Options: Options{PriceAt: priceAtStub{err: ErrPriceAtGuarded}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/price/at?asset=native&ts="+ts.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"https://api.stellarindex.io/errors/price-withheld"`) {
		t.Errorf("guard refusal did not carry the price-withheld type: %s", body)
	}
	if !strings.Contains(body, "serving-sanity guard") {
		t.Errorf("guard refusal is not worded for the guard: %s", body)
	}
	if strings.Contains(body, "no closed bucket") || strings.Contains(body, "too thin") {
		t.Errorf("guard refusal claims a cause that did not fire: %s", body)
	}
}

// priceAtPairStub answers only for pairs in byPair ("base/quote") and gives the rest ErrPriceAtUnavailable, so the
// stablecoin-fallback test can tell the literal fiat:USD read from the peg retry.
type priceAtPairStub struct {
	byPair   map[string]string
	bucketAt time.Time
}

func (s priceAtPairStub) PriceAt(_ context.Context, pair canonical.Pair, _ time.Time, _ time.Duration) (string, time.Time, int, error) {
	if v, ok := s.byPair[pair.Base.String()+"/"+pair.Quote.String()]; ok {
		return v, s.bucketAt, 60, nil
	}
	return "", time.Time{}, 0, ErrPriceAtUnavailable
}

// TestHandlePriceAt_StablecoinFallback pins the CAGG sibling of the stablecoin proxy: a
// historical native/fiat:USD lookup with no direct fiat:USD bucket serves the USD-pegged
// classic bucket, echoing the REQUESTED quote and stamping flags.triangulated.
func TestHandlePriceAt_StablecoinFallback(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	near := ts.Add(-3 * time.Minute)

	s := &Server{
		Options: Options{PriceAt: priceAtPairStub{
			byPair:   map[string]string{"native/" + usdc.String(): "0.1626"},
			bucketAt: near,
		}, USDPeggedClassics: []canonical.Asset{usdc}},
	}
	req := httptest.NewRequest(http.MethodGet,
		"/v1/price/at?asset=native&quote=fiat:USD&ts="+ts.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"price":"0.1626"`,
		`"quote":"fiat:USD"`, // requested quote echoed, not the peg
		`"triangulated":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}

	// Non-USD fiat quote → fallback must not fire.
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, httptest.NewRequest(http.MethodGet,
		"/v1/price/at?asset=native&quote=fiat:EUR&ts="+ts.Format(time.RFC3339), nil))
	if rec.Code != 404 {
		t.Errorf("fiat:EUR quote: status %d, want 404", rec.Code)
	}
}

// TestHandlePriceAt_ReaderFailureIsNotNotFound: a reader error that is neither ErrPriceAtUnavailable nor withheld-class
// says nothing about bucket existence, so no "no closed bucket" 404; a timeout is a retryable 503, other failures 500.
func TestHandlePriceAt_ReaderFailureIsNotNotFound(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	nativeUSD, err := canonical.NewPair(canonical.NativeAsset(), canonical.Asset{Type: canonical.AssetFiat, Code: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		stub       priceAtReadFailStub
		wantStatus int
		wantType   string
	}{
		{"db timeout", priceAtReadFailStub{err: context.DeadlineExceeded}, http.StatusServiceUnavailable, "price-unavailable"},
		{"plain reader error", priceAtReadFailStub{err: errors.New("pq: relation does not exist")}, http.StatusInternalServerError, "internal"},
		// The walk must stop at the failed orientation: serving a later
		// alias would substitute another market for an unknown answer.
		{"first orientation fails, alias would serve", priceAtReadFailStub{
			err: context.DeadlineExceeded, failPair: nativeUSD.Base.String() + "/" + nativeUSD.Quote.String(),
		}, http.StatusServiceUnavailable, "price-unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{PriceAt: tc.stub}, logger: discardLogger()}
			rec := httptest.NewRecorder()
			s.handlePriceAt(rec, httptest.NewRequest(http.MethodGet,
				"/v1/price/at?asset=native&quote=fiat:USD&ts="+ts.Format(time.RFC3339), nil))
			assertReadFailureProblem(t, rec, tc.wantStatus, tc.wantType)
		})
	}
}

// priceAtReadFailStub fails reads with err and serves all others from a fresh bucket. failPair limits the failure to one
// "<base>/<quote>" orientation; failOlderThan limits it to instants at least that far back (one price/changes horizon).
type priceAtReadFailStub struct {
	err           error
	failPair      string
	failOlderThan time.Duration
}

func (s priceAtReadFailStub) PriceAt(
	_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	fails := s.failPair == "" || pair.Base.String()+"/"+pair.Quote.String() == s.failPair
	if s.failOlderThan > 0 && time.Since(ts) < s.failOlderThan {
		fails = false
	}
	if fails {
		return "", time.Time{}, 0, s.err
	}
	return "0.1", ts.Add(-time.Minute), 60, nil
}

func assertReadFailureProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != wantStatus {
		t.Fatalf("status %d, want %d: %s", rec.Code, wantStatus, body)
	}
	if !strings.Contains(body, `"type":"https://api.stellarindex.io/errors/`+wantType+`"`) {
		t.Errorf("body does not carry errors/%s: %s", wantType, body)
	}
	if strings.Contains(body, "price-not-found") || strings.Contains(body, `"available":false`) {
		t.Errorf("a failed read was reported as an absence of data: %s", body)
	}
}

func tinyRat(num int64, places int64) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(num), new(big.Int).Exp(big.NewInt(10), big.NewInt(places), nil))
}

// The derived fiat-cross chart leg: a tiny cross rate must survive.
func TestCrossFiatChartPoints_tinyCrossRateSurvives(t *testing.T) {
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	base := []FXQuotePoint{{Bucket: d, RateUSDText: "4000000000000", InverseUSDText: "0.00000000000025"}}
	quote := []FXQuotePoint{{Bucket: d, RateUSDText: "2", InverseUSDText: "0.5"}}
	got := crossFiatChartPoints(base, quote)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if want := "0.0000000000005000000000000"; got[0].P != want {
		t.Fatalf("cross P = %q, want %q", got[0].P, want)
	}
}

// The cross leg must recover the decimal the NUMERIC column held, not
// the float's binary expansion: 0.3/0.1 is exactly 3, not 2.9999….
func TestCrossFiatChartPoints_recoversDecimalRates(t *testing.T) {
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	base := []FXQuotePoint{{Bucket: d, RateUSDText: "0.1", InverseUSDText: "10"}}
	quote := []FXQuotePoint{{Bucket: d, RateUSDText: "0.3", InverseUSDText: "3.33333333333333333333"}}
	got := crossFiatChartPoints(base, quote)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if want := "3.0000000000"; got[0].P != want {
		t.Fatalf("cross P = %q, want %q", got[0].P, want)
	}
}
