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

type windowVWAPStub struct{ windows map[time.Duration]string }

func (s windowVWAPStub) LookupTriangulatedVWAP(_ context.Context, _, _ canonical.Asset, w time.Duration) (CachedVWAP, bool, error) {
	v, ok := s.windows[w]
	return CachedVWAP{Value: v, ObservedAt: time.Now().UTC()}, ok, nil
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

// compositeWindowStub serves a value for one window and carries the
// aggregator's router meta for it, recording the key the meta was asked under.
type compositeWindowStub struct {
	window       time.Duration
	triangulated bool
	metaRaw      []byte
	askedBase    canonical.Asset
	askedQuote   canonical.Asset
	askedWindow  time.Duration
}

func (s *compositeWindowStub) LookupTriangulatedVWAP(_ context.Context, _, _ canonical.Asset, w time.Duration) (CachedVWAP, bool, error) {
	if w != s.window {
		return CachedVWAP{}, false, nil
	}
	return CachedVWAP{Value: "0.91", Triangulated: s.triangulated, ObservedAt: time.Now().UTC()}, true, nil
}

func (s *compositeWindowStub) LookupCompositeMeta(_ context.Context, base, quote canonical.Asset, w time.Duration) ([]byte, bool, error) {
	s.askedBase, s.askedQuote, s.askedWindow = base, quote, w
	if w != s.window || s.metaRaw == nil {
		return nil, false, nil
	}
	return s.metaRaw, true, nil
}

type frozenPairStub struct{}

func (frozenPairStub) FrozenForPair(context.Context, canonical.Asset, canonical.Asset) (bool, error) {
	return true, nil
}

// TestHandlePriceWindowed_CompositeFlags pins GH-951: the ?window= path is
// the surface that serves a router-priced target's composite, so it must
// carry the composite's diverged/rerouted qualifiers, read for the served
// window rather than the fallback's fixed 5m key.
func TestHandlePriceWindowed_CompositeFlags(t *testing.T) {
	asset, _ := canonical.ParseAsset("crypto:XLM")
	quote, _ := canonical.ParseAsset("fiat:GBP")
	meta := []byte(`{"path_count":2,"combined_confidence":0.81,"low_confidence":false,"diverged":true,"rerouted":true}`)
	serve := func(t *testing.T, s *Server) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=crypto:XLM&quote=fiat:GBP&window=3600", nil)
		s.handlePriceWindowed(rec, req, asset, quote, "3600")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	t.Run("triangulated composite surfaces its meta", func(t *testing.T) {
		stub := &compositeWindowStub{window: time.Hour, triangulated: true, metaRaw: meta}
		body := serve(t, &Server{triangulated: stub})
		for _, want := range []string{`"triangulated":true`, `"diverged":true`, `"rerouted":true`} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
		}
		if stub.askedWindow != time.Hour || !stub.askedBase.Equal(asset) || !stub.askedQuote.Equal(quote) {
			t.Errorf("meta asked for %s/%s @%s, want %s/%s @1h",
				stub.askedBase, stub.askedQuote, stub.askedWindow, asset, quote)
		}
	})

	t.Run("direct value never carries composite meta", func(t *testing.T) {
		stub := &compositeWindowStub{window: time.Hour, triangulated: false, metaRaw: meta}
		body := serve(t, &Server{triangulated: stub})
		for _, unwanted := range []string{`"diverged"`, `"rerouted"`} {
			if strings.Contains(body, unwanted) {
				t.Errorf("direct serve carries %s from an unpublished composite: %s", unwanted, body)
			}
		}
	})

	t.Run("frozen serve is single-sourced", func(t *testing.T) {
		stub := &compositeWindowStub{window: time.Hour}
		body := serve(t, &Server{triangulated: stub, freeze: frozenPairStub{}})
		for _, want := range []string{`"frozen":true`, `"single_source":true`} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
		}
	})
}
