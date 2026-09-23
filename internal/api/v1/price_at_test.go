// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// priceAtStub honours maxStaleness the way the wired producer does
// (timescale ClosedVWAPAtOrBefore and the pricing guard both refuse a
// bucket staler than the bound), unless ignoreStaleness asks for the
// out-of-contract reader the handler's own re-check defends against.
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

// TestHandlePriceAt pins board #46: a historical instant serves the
// closed bucket at-or-before it with the BUCKET's own observed_at;
// a bucket older than the 24h honesty cap 404s instead of
// fabricating continuity; future ts and missing ts are 400s.
func TestHandlePriceAt(t *testing.T) {
	ts := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	near := ts.Add(-45 * time.Minute)

	s := &Server{priceAt: priceAtStub{value: "0.128", bucketAt: near}}
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
	s = &Server{priceAt: priceAtStub{value: "0.128", bucketAt: stale, gotStaleness: &gotStaleness}}
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
	s = &Server{priceAt: priceAtStub{value: "0.128", bucketAt: stale, ignoreStaleness: true}}
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

// TestHandlePriceAt_WithheldDistinctFromNotFound pins RLT-454: a
// reader that returns ErrPriceWithheld (the pair HAS a closed bucket
// but the substance/scam gate refuses to publish it) must 404 with the
// distinct errors/price-withheld type, not the generic
// errors/price-not-found the "we have never seen this pair" case uses
// — same contract handlePrice and handlePriceTip already carry.
func TestHandlePriceAt_WithheldDistinctFromNotFound(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{priceAt: priceAtStub{err: ErrPriceWithheld}}
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

// TestHandlePriceAt_GuardedIsWithheldNotNoData: a bucket the
// serving-sanity guard refused exists, so the 404 is price-withheld with
// the guard's wording — not price-not-found's "no closed bucket", and not
// the thin-market sentence the bare withheld sentinel gets.
func TestHandlePriceAt_GuardedIsWithheldNotNoData(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{priceAt: priceAtStub{err: ErrPriceAtGuarded}}
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

// priceAtPairStub answers only for pairs present in byPair (keyed
// "base/quote"); everything else gets ErrPriceAtUnavailable. Lets the
// stablecoin-fallback test distinguish the literal fiat:USD read from
// the peg retry.
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

// TestHandlePriceAt_StablecoinFallback pins the CAGG sibling of the
// #1217-family stablecoin proxy: a historical native/fiat:USD lookup
// with no direct fiat:USD bucket serves the USD-pegged-classic bucket
// instead, echoing the REQUESTED quote and stamping
// flags.triangulated.
func TestHandlePriceAt_StablecoinFallback(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	near := ts.Add(-3 * time.Minute)

	s := &Server{
		priceAt: priceAtPairStub{
			byPair:   map[string]string{"native/" + usdc.String(): "0.1626"},
			bucketAt: near,
		},
		usdPeggedClassics: []canonical.Asset{usdc},
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
