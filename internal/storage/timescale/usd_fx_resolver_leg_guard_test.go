// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func legMinutes(t *testing.T, newest time.Time, rates ...string) []legMinute {
	t.Helper()
	out := make([]legMinute, len(rates))
	for i, s := range rates {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("bad rate %q", s)
		}
		out[i] = legMinute{bucket: newest.Add(-time.Duration(i) * time.Minute), rate: r, text: s}
	}
	return out
}

func TestGuardedLegMinute(t *testing.T) {
	t.Parallel()
	newest := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		rates  []string
		want   string
		wantOK bool
	}{
		{"no minutes", nil, "", false},
		{"lone minute keeps the candidate", []string{"5"}, "5", true},
		{"in-band candidate is served", []string{"0.32", "0.31", "0.315", "0.31", "0.32", "0.315"}, "0.32", true},
		{"spike yields to the newest in-band minute", []string{"9", "0.31", "0.315", "0.31", "0.32", "0.315"}, "0.31", true},
		{"crash yields too", []string{"0.0001", "0.315", "0.31", "0.315", "0.32", "0.31"}, "0.315", true},
		{"thin baseline still bounds a 100x print", []string{"31.5", "0.315", "0.31"}, "0.315", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := guardedLegMinute(legMinutes(t, newest, tc.rates...))
			if ok != tc.wantOK || got.text != tc.want {
				t.Errorf("guardedLegMinute = (%q, %t), want (%q, %t)", got.text, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestVWAPUSDFXResolver_DirectLegRefusesASpikeMinute pins API parity on the trade path: one 30x newest minute
// against a steady XLM/USDC series must not price a trade; the newest in-band minute does.
func TestVWAPUSDFXResolver_DirectLegRefusesASpikeMinute(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	rows := [][]driver.Value{{at, "9.00000000"}}
	for i := 1; i <= 6; i++ {
		rows = append(rows, []driver.Value{at.Add(-time.Duration(i) * time.Minute), "0.31500000"})
	}
	store, conn := newScriptedStore(t, scriptedResult{cols: []string{"bucket", "vwap"}, rows: rows})
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	got, ok, err := r.USDPriceAt(context.Background(), canonical.NativeAsset(), at)
	if err != nil || !ok {
		t.Fatalf("USDPriceAt = (%q, %t, %v), want a rate", got, ok, err)
	}
	if got != "0.315" {
		t.Errorf("USDPriceAt(native) = %q, want 0.315 (the spike minute must be refused)", got)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want 1", len(conn.stmts))
	}
}

// TestQueryXLMLeg_GuardsAcrossOrientations pins that the bridge leg guards on XLM-per-asset after inversion:
// an XLM-first row is a rate, not its reciprocal, in the baseline.
func TestQueryXLMLeg_GuardsAcrossOrientations(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	// Asset worth 4 XLM: stored TOKEN/XLM as 4, XLM/TOKEN as 0.25. Newest minute is a 50x print.
	rows := [][]driver.Value{{at, "200", false}}
	for i := 1; i <= 6; i++ {
		v, inv := "4", false
		if i%2 == 0 {
			v, inv = "0.25", true
		}
		rows = append(rows, []driver.Value{at.Add(-time.Duration(i) * time.Minute), v, inv})
	}
	store, _ := newScriptedStore(t, scriptedResult{cols: []string{"bucket", "vwap", "inverted"}, rows: rows})
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	token, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	got, err := r.queryXLMLeg(context.Background(), token, at)
	if err != nil || got == nil {
		t.Fatalf("queryXLMLeg = (%v, %v), want a rate", got, err)
	}
	if got.Cmp(big.NewRat(4, 1)) != 0 {
		t.Errorf("queryXLMLeg = %s, want 4 (spike refused, inverted rows normalised)", got.RatString())
	}
}

func TestLegQueriesTakeTheGuardSample(t *testing.T) {
	t.Parallel()
	limit := "LIMIT 41"
	if got := strings.Count(xlmLegQuery(""), limit); got != 3 {
		t.Errorf("xlmLegQuery has %d %q, want 3 (both arms and the outer pick)", got, limit)
	}
	for _, q := range []string{directLegQuery(false, false), directLegQuery(true, true)} {
		if !strings.Contains(q, limit) {
			t.Errorf("directLegQuery lacks %q:\n%s", limit, q)
		}
	}
}
