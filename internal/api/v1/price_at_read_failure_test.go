// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// priceAtReadFailStub fails reads with err and serves every other read
// from a fresh bucket. failPair, when set, limits the failure to that
// "<base>/<quote>" orientation; failOlderThan, when set, limits it to
// instants at least that far in the past (one /v1/price/changes horizon).
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

// TestHandlePriceAt_ReaderFailureIsNotNotFound: a reader error that is
// neither ErrPriceAtUnavailable nor withheld-class says nothing about
// whether a bucket exists, so /v1/price/at must not answer the
// "no closed bucket" 404 — a timeout is a retryable 503, any other
// failure a 500.
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
			s := &Server{priceAt: tc.stub, logger: discardLogger()}
			rec := httptest.NewRecorder()
			s.handlePriceAt(rec, httptest.NewRequest(http.MethodGet,
				"/v1/price/at?asset=native&quote=fiat:USD&ts="+ts.Format(time.RFC3339), nil))
			assertReadFailureProblem(t, rec, tc.wantStatus, tc.wantType)
		})
	}
}

// TestHandlePriceChanges_ReaderFailureIsNotAbsence: a failed read of the
// current anchor is not "no current price" (404), and a failed read of
// one horizon is not available=false beside populated siblings — the
// spec defines that shape as "no data that far back", never an error.
func TestHandlePriceChanges_ReaderFailureIsNotAbsence(t *testing.T) {
	cases := []struct {
		name       string
		stub       priceAtReadFailStub
		wantStatus int
		wantType   string
	}{
		{"anchor db timeout", priceAtReadFailStub{err: context.DeadlineExceeded}, http.StatusServiceUnavailable, "price-unavailable"},
		{"anchor plain reader error", priceAtReadFailStub{err: errors.New("boom")}, http.StatusInternalServerError, "internal"},
		{"7d horizon db timeout", priceAtReadFailStub{
			err: context.DeadlineExceeded, failOlderThan: 3 * 24 * time.Hour,
		}, http.StatusServiceUnavailable, "price-unavailable"},
		{"7d horizon plain reader error", priceAtReadFailStub{
			err: errors.New("boom"), failOlderThan: 3 * 24 * time.Hour,
		}, http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{priceAt: tc.stub, logger: discardLogger()}
			rec := httptest.NewRecorder()
			s.handlePriceChanges(rec, httptest.NewRequest(http.MethodGet,
				"/v1/price/changes?asset=native&quote=fiat:USD", nil))
			assertReadFailureProblem(t, rec, tc.wantStatus, tc.wantType)
		})
	}
}
