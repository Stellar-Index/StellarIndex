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

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

type priceAtStub struct {
	value    string
	bucketAt time.Time
	err      error
}

func (s priceAtStub) PriceAt(context.Context, canonical.Pair, time.Time) (string, time.Time, error) {
	return s.value, s.bucketAt, s.err
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

	// Nearest bucket beyond the lookback cap → 404, not a stale lie.
	s = &Server{priceAt: priceAtStub{value: "0.128", bucketAt: ts.Add(-30 * 24 * time.Hour)}}
	rec = httptest.NewRecorder()
	s.handlePriceAt(rec, req)
	if rec.Code != 404 {
		t.Errorf("beyond-cap bucket: status %d, want 404", rec.Code)
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
