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

// priceChangesAgeStub answers PriceAt by classifying how far ts is
// behind now into a fixed horizon bucket — so one stub serves the
// current price AND every horizon reference from a single request.
// Returning ("", …, ErrPriceAtUnavailable) for a bucket models "no
// data that far back" (the null-horizon case).
type priceChangesAgeStub struct{}

func (priceChangesAgeStub) PriceAt(
	_ context.Context, _ canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	age := time.Since(ts)
	switch {
	case age < 30*time.Minute: // current
		return "1.00", ts, 60, nil
	case age < 12*time.Hour: // 1h horizon
		return "0.99", ts, 60, nil
	case age < 4*24*time.Hour: // 24h horizon
		return "0.80", ts, 60, nil
	case age < 20*24*time.Hour: // 7d horizon — served by a 1h bar
		return "0.50", ts, 3600, nil
	default: // 30d horizon — no data that far back
		return "", time.Time{}, 0, ErrPriceAtUnavailable
	}
}

func TestHandlePriceChanges_503WhenReaderNil(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes?asset=native", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePriceChanges_MissingAsset400(t *testing.T) {
	s := &Server{priceAt: priceChangesAgeStub{}}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePriceChanges_Identity400(t *testing.T) {
	s := &Server{priceAt: priceChangesAgeStub{}}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes?asset=fiat:USD&quote=fiat:USD", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlePriceChanges_HappyPath pins the multi-horizon shape:
// current price + a signed delta per horizon, with a horizon that has
// no data that far back nulled + flagged available:false rather than
// erroring the whole call.
func TestHandlePriceChanges_HappyPath(t *testing.T) {
	s := &Server{priceAt: priceChangesAgeStub{}}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes?asset=native&quote=fiat:USD", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"current_price":"1.00"`,
		`"current_price_type":"vwap"`,
		`"resolution":"1m"`,      // current bucket resolution
		`"change_pct":"+1.01"`,   // 1h: (1.00-0.99)/0.99
		`"change_pct":"+25.00"`,  // 24h: (1.00-0.80)/0.80
		`"change_pct":"+100.00"`, // 7d: (1.00-0.50)/0.50
		`"resolution":"1h"`,      // 7d reference served by a 1h bar
		`"change_pct":null`,      // 30d unavailable
		`"available":false`,      // 30d flag
		`"available":true`,       // the resolved horizons
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
}

func TestHandlePriceChanges_404WhenNoCurrent(t *testing.T) {
	// A pair-aware stub with an EMPTY table answers ErrPriceAtUnavailable
	// for every pair — so there is no current price to anchor on.
	s := &Server{priceAt: priceChangesPairStub{byPair: map[string]string{}}}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes?asset=native&quote=fiat:USD", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlePriceChanges_StablecoinFallbackTriangulated pins that a
// native/fiat:USD request with no direct fiat:USD bucket resolves via
// the operator's USD-pegged classic and flags triangulated — the same
// proxy chain /v1/price and /v1/price/at use.
func TestHandlePriceChanges_StablecoinFallbackTriangulated(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	s := &Server{
		priceAt:           priceChangesPairStub{byPair: map[string]string{"native/" + usdc.String(): "0.16"}},
		usdPeggedClassics: []canonical.Asset{usdc},
	}
	rec := httptest.NewRecorder()
	s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet, "/v1/price/changes?asset=native&quote=fiat:USD", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"quote":"fiat:USD"`, // requested quote echoed, not the peg
		`"current_price":"0.16"`,
		`"triangulated":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
}

// priceChangesPairStub answers only for pairs present in byPair (keyed
// "base/quote"), for ANY ts — so it exercises the pair-orientation /
// stablecoin-fallback resolution without modeling horizon ages.
type priceChangesPairStub struct {
	byPair map[string]string
}

func (s priceChangesPairStub) PriceAt(
	_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	if v, ok := s.byPair[pair.Base.String()+"/"+pair.Quote.String()]; ok {
		return v, ts, 60, nil
	}
	return "", time.Time{}, 0, ErrPriceAtUnavailable
}
