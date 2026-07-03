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

type windowVWAPStub struct{ windows map[time.Duration]string }

func (s windowVWAPStub) LookupTriangulatedVWAP(_ context.Context, _, _ canonical.Asset, w time.Duration) (string, bool, bool, error) {
	v, ok := s.windows[w]
	return v, false, ok, nil
}

// TestHandlePriceWindowed pins board #43's window selection: a
// published window serves its VWAP with honest window_seconds; an
// unpublished one 404s (no silent substitution); junk 400s.
func TestHandlePriceWindowed(t *testing.T) {
	s := &Server{triangulated: windowVWAPStub{windows: map[time.Duration]string{5 * time.Minute: "0.205"}}}
	asset := canonical.NativeAsset()
	quote, _ := canonical.ParseAsset("fiat:USD")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&window=300", nil)
	s.handlePriceWindowed(rec, req, asset, quote, "300")
	if rec.Code != 200 {
		t.Fatalf("window=300: status %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"price":"0.205"`, `"window_seconds":300`, `"price_type":"vwap"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	s.handlePriceWindowed(rec, req, asset, quote, "86400")
	if rec.Code != 404 {
		t.Errorf("unpublished window: status %d, want 404 (no silent substitution)", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handlePriceWindowed(rec, req, asset, quote, "7")
	if rec.Code != 400 {
		t.Errorf("junk window: status %d, want 400", rec.Code)
	}
}
