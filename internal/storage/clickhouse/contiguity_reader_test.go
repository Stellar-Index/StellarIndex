// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

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

func TestLedgerWindowCoverage_DuplicateRows(t *testing.T) {
	cases := []struct {
		name          string
		present, rows uint64
		want          uint64
	}{
		{"merged", 10, 10, 0},
		{"one-unmerged-reingest", 10, 11, 1},
		{"fully-doubled", 10, 20, 10},
		{"rows-unset-saturates", 10, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := LedgerWindowCoverage{Expected: 10, Present: tc.present, Rows: tc.rows}
			if got := c.DuplicateRows(); got != tc.want {
				t.Fatalf("DuplicateRows() = %d, want %d", got, tc.want)
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
		// Unreachable through QueryECWindowCoverage (covered is a subset of tx-bearing), but the guard must still
		// saturate to 0 rather than wrap uint64 to ~1.8e19 on unrelated cardinalities.
		{"covered-exceeds-tx-bearing", 900, 903, 0},
		{"both-zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := ECWindowCoverage{TxLedgers: tc.txLedgers, ECCoveredTxLedgers: tc.ecCovered}
			if got := w.Missing(); got != tc.want {
				t.Fatalf("Missing() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestECWindowCoverageQuery_AntiJoin: the Check-2 coverage count must be per tx-bearing ledger, since
// entry changes at tx_count == 0 ledgers would otherwise cancel real gaps. It asserts a uniqExactIf over
// stellar.ledgers with ledger_entry_changes only inside the membership subquery.
func TestECWindowCoverageQuery_AntiJoin(t *testing.T) {
	q := ecWindowCoverageQuery()

	// The covered side must be conditional over the tx-bearing set.
	if !strings.Contains(q, "uniqExactIf(ledger_seq, ledger_seq IN (") {
		t.Errorf("coverage count is not a per-tx-bearing-ledger membership test:\n%s", q)
	}
	// Both counts must be driven by the SAME tx-bearing row set.
	if !strings.Contains(q, "FROM stellar.ledgers") ||
		!strings.Contains(q, "WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0") {
		t.Errorf("coverage scan is not anchored on the tx-bearing ledgers set:\n%s", q)
	}
	// ledger_entry_changes may appear ONLY as the membership subquery's
	// source. A second top-level scan of it is the naive shape.
	if n := strings.Count(q, "stellar.ledger_entry_changes"); n != 1 {
		t.Errorf("stellar.ledger_entry_changes referenced %d times, want exactly 1 (membership subquery only):\n%s", n, q)
	}
	// The naive statement must not appear: an unrestricted uniqExact
	// over entry_changes would count tx_count == 0 ledgers as coverage.
	if strings.Contains(q, "uniqExact(ledger_seq) FROM stellar.ledger_entry_changes") {
		t.Errorf("pre-fix standalone entry-changes cardinality is still present:\n%s", q)
	}
	// Snapshot seed rows (empty tx_hash) are not tx-meta coverage.
	if !strings.Contains(q, "WHERE ledger_seq BETWEEN ? AND ? AND tx_hash != ''\n") {
		t.Errorf("membership subquery admits empty-tx_hash seed rows as coverage:\n%s", q)
	}
	// Four positional placeholders: (subquery lo, hi, outer lo, hi).
	if n := strings.Count(q, "?"); n != 4 {
		t.Errorf("query has %d placeholders, want 4 (subquery lo/hi + outer lo/hi):\n%s", n, q)
	}
}
