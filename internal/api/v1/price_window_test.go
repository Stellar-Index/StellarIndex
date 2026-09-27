// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// heldWindowStub serves one window's value observed at a fixed instant,
// standing in for a vwap: key a freeze keeps alive past its window.
type heldWindowStub struct{ observedAt time.Time }

func (s heldWindowStub) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (CachedVWAP, bool, error) {
	return CachedVWAP{Value: "0.998", ObservedAt: s.observedAt}, true, nil
}

// TestHandlePriceWindowed_FrozenIsStale pins GH-761: a frozen pair's
// ?window= value is the one the freeze holds, below the window's
// baseline, so it ships flags.stale=true exactly as the default path does.
func TestHandlePriceWindowed_FrozenIsStale(t *testing.T) {
	asset, _ := canonical.ParseAsset("crypto:USDC")
	quote, _ := canonical.ParseAsset("fiat:USD")
	heldAt := time.Now().UTC().Add(-2*time.Hour - 20*time.Minute).Truncate(time.Minute)
	serve := func(t *testing.T, s *Server) (PriceSnapshot, Flags) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=crypto:USDC&quote=fiat:USD&window=300", nil)
		s.handlePriceWindowed(rec, req, asset, quote, "300")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Data  PriceSnapshot `json:"data"`
			Flags Flags         `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v body %s", err, rec.Body.String())
		}
		return env.Data, env.Flags
	}

	t.Run("frozen serve is stale and carries the held observation time", func(t *testing.T) {
		snap, flags := serve(t, &Server{triangulated: heldWindowStub{observedAt: heldAt}, freeze: frozenPairStub{}})
		if !flags.Frozen || !flags.Stale {
			t.Errorf("frozen ?window= serve: frozen=%v stale=%v, want both true", flags.Frozen, flags.Stale)
		}
		if got := time.Time(snap.ObservedAt); !got.Equal(heldAt) {
			t.Errorf("observed_at = %s, want the held value's %s", got, heldAt)
		}
	})

	t.Run("unfrozen serve is in-contract", func(t *testing.T) {
		_, flags := serve(t, &Server{triangulated: heldWindowStub{observedAt: time.Now().UTC()}})
		if flags.Frozen || flags.Stale {
			t.Errorf("unfrozen ?window= serve: frozen=%v stale=%v, want both false", flags.Frozen, flags.Stale)
		}
	})
}

// substanceStoreStub answers every substance read with one measurement.
type substanceStoreStub struct{ sub timescale.MarketSubstance }

func (s substanceStoreStub) PairMarketSubstance(context.Context, []canonical.Asset, []canonical.Asset, time.Duration) (timescale.MarketSubstance, error) {
	return s.sub, nil
}

func (s substanceStoreStub) PairMarketSubstanceAt(context.Context, []canonical.Asset, []canonical.Asset, time.Time, time.Duration, timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.sub, nil
}

// provenanceWindowStub serves one value for every window, carrying the
// aggregator's triangulation provenance.
type provenanceWindowStub struct{ triangulated bool }

func (s provenanceWindowStub) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (CachedVWAP, bool, error) {
	return CachedVWAP{Value: "0.0042", ObservedAt: time.Now().UTC(), Triangulated: s.triangulated}, true, nil
}

// TestHandlePriceWindowed_SubstanceGate pins that ?window= withholds a
// thin literal on-chain market exactly as the default window does: the
// reader behind window=60 404s such a pair price-withheld, so the same
// pair must not answer 200 under another window. The market here clears
// any volume floor but has too few active buckets — the half of the
// floor the aggregator's min_usd_volume never checks. Triangulated and
// fiat-quoted values have no literal market and stay served, as on the
// default path's cache fallback.
func TestHandlePriceWindowed_SubstanceGate(t *testing.T) {
	aqua, err := canonical.ParseAsset("AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	thin := timescale.MarketSubstance{VolumeUSD: "5000000", Buckets: 5, SpanSeconds: 12 * 3600, ValuedBuckets: 5}
	deep := timescale.MarketSubstance{VolumeUSD: "5000000", Buckets: 600, SpanSeconds: 20 * 3600, ValuedBuckets: 600}
	gate := func(sub timescale.MarketSubstance) *pricingguard.SubstanceGate {
		return pricingguard.NewSubstanceGate(substanceStoreStub{sub: sub}, pricingguard.SubstanceGateOptions{})
	}
	cases := []struct {
		name         string
		quote        canonical.Asset
		triangulated bool
		sub          timescale.MarketSubstance
		wantStatus   int
	}{
		{"thin direct on-chain market is withheld", usdc, false, thin, http.StatusNotFound},
		{"substantive direct on-chain market is served", usdc, false, deep, http.StatusOK},
		{"triangulated value stays scam-only", usdc, true, thin, http.StatusOK},
		{"fiat-quoted proxy value has no literal market", usd, false, thin, http.StatusOK},
	}
	for _, tc := range cases {
		for _, window := range []string{"300", "3600", "86400"} {
			t.Run(tc.name+"/window="+window, func(t *testing.T) {
				s := &Server{triangulated: provenanceWindowStub{triangulated: tc.triangulated}, substance: gate(tc.sub)}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/v1/price?window="+window, nil)
				s.handlePriceWindowed(rec, req, aqua, tc.quote, window)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				body := rec.Body.String()
				if tc.wantStatus == http.StatusNotFound {
					if !strings.Contains(body, "errors/price-withheld") || !strings.Contains(body, "market too thin") {
						t.Errorf("withheld body must carry the thin-market price-withheld problem: %s", body)
					}
					if strings.Contains(body, "0.0042") {
						t.Errorf("withheld body leaked the cached VWAP: %s", body)
					}
				} else if !strings.Contains(body, `"price":"0.0042"`) {
					t.Errorf("served body missing the cached VWAP: %s", body)
				}
			})
		}
	}
}
