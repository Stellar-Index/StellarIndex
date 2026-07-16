// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import "testing"

func TestHashChainWindowResult_Checked(t *testing.T) {
	cases := []struct {
		name    string
		present uint64
		want    uint64
	}{
		{"typical-window", 1_000_000, 999_999},
		{"single-ledger", 1, 0},
		// Present==0: an entirely-missing window (a contiguity gap spanning
		// the whole bucket). Present-1 would otherwise be the b>a case that
		// wraps a uint64 subtraction to ~1.8e19 — the regression class
		// verify-contiguity's review caught (ECWindowCoverage.Missing()).
		// Checked() MUST saturate to 0 here instead.
		{"empty-window", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := HashChainWindowResult{Present: tc.present}
			if got := w.Checked(); got != tc.want {
				t.Fatalf("Checked() = %d, want %d", got, tc.want)
			}
		})
	}
}
