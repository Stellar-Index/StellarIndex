// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunServedValueChecks_TolerancesAndOutages exercises the three
// outcome classes against stub servers: within tolerance (ok),
// drifted beyond tolerance (fail — the CS-010 class), and
// ground-truth outage (skipped, ok, NaN rel_err — a dark truth
// source must not read as a served-value failure).
func TestRunServedValueChecks_TolerancesAndOutages(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string
		served     float64
		truth      float64
		tolerance  float64
		truthFails bool
		wantOK     bool
		wantNaN    bool
	}{
		{"within tolerance", 105_000_000_000, 105_400_000_000, 0.005, false, true, false},
		{"cs-010 class drift fails", 105_000_000_000, 66_000_000_000, 0.02, false, false, false},
		{"truth outage skips", 105_000_000_000, 0, 0.005, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"total_supply":"105000000000"}}`))
			}))
			t.Cleanup(api.Close)
			truth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.truthFails {
					http.Error(w, "down", http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"totalSupply":"66000000000"}`))
			}))
			t.Cleanup(truth.Close)

			check := servedValueCheck{
				name: "probe", tolerance: tc.tolerance,
				// decimals=0: the stub serves natural units; the
				// base-unit scaling itself is pinned by
				// TestServedSupplyField_ScalesBaseUnits.
				served: servedSupplyField("native", "total_supply", 0),
				truth: func(ctx context.Context, c *http.Client) (float64, error) {
					var body map[string]any
					if err := getJSON(ctx, c, truth.URL, &body); err != nil {
						return 0, err
					}
					if tc.truth != 0 {
						return tc.truth, nil
					}
					return 0, nil
				},
			}
			results := runChecksForTest(ctx, api.URL, []servedValueCheck{check})
			r := results[0]
			if r.ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (rel_err=%v note=%s)", r.ok, tc.wantOK, r.relErr, r.note)
			}
			if tc.wantNaN != math.IsNaN(r.relErr) {
				t.Errorf("NaN rel_err = %v, want %v", math.IsNaN(r.relErr), tc.wantNaN)
			}
		})
	}
}

// runChecksForTest mirrors runServedValueChecks with an injected
// check table.
func runChecksForTest(ctx context.Context, apiBase string, checks []servedValueCheck) []servedValueResult {
	c := &http.Client{Timeout: 5 * time.Second}
	out := make([]servedValueResult, 0, len(checks))
	for _, chk := range checks {
		out = append(out, reconcileOneCheck(ctx, c, apiBase, chk))
	}
	return out
}

// TestRenderServedValueProm — the textfile body has the three gauge
// families, quotes check names, and omits rel_err for NaN.
func TestRenderServedValueProm(t *testing.T) {
	body := renderServedValueProm([]servedValueResult{
		{name: "a", relErr: 0.001, ok: true},
		{name: "b", relErr: math.NaN(), ok: true},
	}, time.Unix(1_751_000_000, 0))
	for _, want := range []string{
		`stellarindex_served_value_rel_err{check="a"} 0.001`,
		`stellarindex_served_value_ok{check="a"} 1`,
		`stellarindex_served_value_ok{check="b"} 1`,
		"stellarindex_served_value_last_run_unix 1751000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("textfile body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `rel_err{check="b"}`) {
		t.Error("NaN rel_err must be omitted, not rendered")
	}
}

// TestServedSupplyField_NullIsAFailure — a null served supply field
// is a pipeline failure, not a zero.
func TestServedSupplyField_NullIsAFailure(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_supply":null}}`))
	}))
	t.Cleanup(api.Close)
	_, err := servedSupplyField("native", "total_supply", 7)(context.Background(), &http.Client{}, api.URL)
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("want null-field error, got %v", err)
	}
}

// TestServedSupplyField_ScalesBaseUnits pins the empirical 2026-07-02
// finding: the F2 supply fields are BASE-UNIT decimal strings
// (stroops for classic), so the reader must scale by 10^-decimals
// before comparing against natural-unit ground truth.
func TestServedSupplyField_ScalesBaseUnits(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_supply":"500018068120000000"}}`)) // stroops
	}))
	t.Cleanup(api.Close)
	got, err := servedSupplyField("native", "total_supply", 7)(context.Background(), &http.Client{}, api.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := 50_001_806_812.0; got != want {
		t.Fatalf("scaled supply = %v, want %v", got, want)
	}
}
