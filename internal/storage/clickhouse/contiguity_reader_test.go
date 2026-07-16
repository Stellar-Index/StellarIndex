// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import "testing"

func TestLedgerWindowCoverage_Missing(t *testing.T) {
	cases := []struct {
		name              string
		expected, present uint64
		want              uint64
	}{
		{"fully-covered", 1_000_000, 1_000_000, 0},
		{"one-gap", 1_000_000, 999_999, 1},
		{"empty-window", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := LedgerWindowCoverage{Expected: tc.expected, Present: tc.present}
			if got := c.Missing(); got != tc.want {
				t.Fatalf("Missing() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestECWindowCoverage_Missing(t *testing.T) {
	cases := []struct {
		name                 string
		txLedgers, ecCovered uint64
		want                 uint64
	}{
		{"fully-covered", 900, 900, 0},
		{"deficit", 900, 850, 50},
		// ECCovered > TxLedgers: entry_changes covers a non-tx-bearing
		// (protocol-upgrade/config-change) ledger. Must SATURATE to 0, not
		// wrap uint64 to ~1.8e19 — the regression this method's guard exists
		// for.
		{"covered-exceeds-tx-bearing", 900, 903, 0},
		{"both-zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := ECWindowCoverage{TxLedgers: tc.txLedgers, ECCovered: tc.ecCovered}
			if got := w.Missing(); got != tc.want {
				t.Fatalf("Missing() = %d, want %d", got, tc.want)
			}
		})
	}
}
