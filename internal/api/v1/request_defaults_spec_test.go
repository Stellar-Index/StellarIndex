// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// A declared schema default is a value generated clients send on the
// caller's behalf, so the server must accept it wherever the parameter is
// accepted. Both tests below walk the spec rather than naming the value, so
// a default edited later is held to the same bar.

// specQuoteDefaults collects the `default` of every `quote` query parameter
// and every `quote` request-body property in the spec.
func specQuoteDefaults(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		if n["name"] == "quote" {
			if schema, ok := n["schema"].(map[string]any); ok {
				if d, ok := schema["default"].(string); ok {
					*out = append(*out, d)
				}
			}
		}
		if props, ok := n["properties"].(map[string]any); ok {
			if q, ok := props["quote"].(map[string]any); ok {
				if d, ok := q["default"].(string); ok {
					*out = append(*out, d)
				}
			}
		}
		for _, v := range n {
			specQuoteDefaults(v, out)
		}
	case []any:
		for _, v := range n {
			specQuoteDefaults(v, out)
		}
	}
}

// TestSpecQuoteDefaultsParse — POST /price/batch documented `quote` as
// defaulting to bare "USD", which the quote parser rejects with 400
// invalid-quote; only an omitted quote got the real default, fiat:USD.
func TestSpecQuoteDefaultsParse(t *testing.T) {
	var defaults []string
	specQuoteDefaults(loadSpecDoc(t), &defaults)
	// The shared GET Quote parameter and the batch body both declare one.
	if len(defaults) < 2 {
		t.Fatalf("found %d quote default(s) in the spec, want at least 2: %v", len(defaults), defaults)
	}
	s := &Server{}
	for _, d := range defaults {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/price/batch", nil)
		if _, ok := s.parsePriceBatchQuote(rec, req, d); !ok {
			t.Errorf("spec quote default %q is rejected by the quote parser (status %d): a client "+
				"sending the documented default gets a 400", d, rec.Code)
		}
	}
}

// TestAssetsOrderByDefaultIsAcceptedWithAssetClass — /v1/assets rejects
// an explicit order_by combined with asset_class. A schema default on
// order_by is exactly what a default-filling client sends explicitly, so
// declaring one made the documented default 400 on every class listing
// while omitting it succeeded.
func TestAssetsOrderByDefaultIsAcceptedWithAssetClass(t *testing.T) {
	paths, _ := loadSpecDoc(t)["paths"].(map[string]any)
	op, _ := paths["/assets"].(map[string]any)["get"].(map[string]any)
	params, _ := op["parameters"].([]any)
	var def string
	var sawOrderBy bool
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] != "order_by" {
			continue
		}
		sawOrderBy = true
		schema, _ := pm["schema"].(map[string]any)
		def, _ = schema["default"].(string)
	}
	if !sawOrderBy {
		t.Fatal("GET /assets declares no order_by parameter; this guard is reading the wrong operation")
	}
	if def == "" {
		return
	}
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{VerifiedCurrencies: cat}).Handler()
	for _, class := range []string{"crypto", "stablecoin", "all"} {
		q := url.Values{"asset_class": {class}, "order_by": {def}}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets?"+q.Encode(), nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /v1/assets?%s = %d, want 200: the spec declares order_by default %q, "+
				"so a client that fills defaults sends it with every asset_class", q.Encode(), rec.Code, def)
		}
	}
}
