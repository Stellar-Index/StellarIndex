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

func TestECWindowCoverage_Missing(t *testing.T) {
	cases := []struct {
		name                 string
		txLedgers, ecCovered uint64
		want                 uint64
	}{
		{"fully-covered", 900, 900, 0},
		{"deficit", 900, 850, 50},
		// Unreachable through QueryECWindowCoverage since C4-085 (the
		// covered side is a SUBSET of the tx-bearing side), but the guard
		// must still SATURATE to 0 rather than wrap uint64 to ~1.8e19 if a
		// caller hands over unrelated cardinalities.
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

// TestECWindowCoverageQuery_AntiJoin pins C4-085 (audit-2026-07-23): the
// Check-2 coverage count must be evaluated PER TX-BEARING LEDGER, not as a
// standalone cardinality of stellar.ledger_entry_changes.
//
// The pre-fix form ran two independent scans and subtracted them:
//
//	SELECT uniqExact(ledger_seq) FROM stellar.ledgers            WHERE … AND tx_count > 0
//	SELECT uniqExact(ledger_seq) FROM stellar.ledger_entry_changes WHERE …
//
// Entry-change rows exist for tx_count == 0 ledgers (protocol upgrades,
// config/base-reserve changes), so those ledgers inflated the second count
// while never appearing in the first — inside a 1,000,000-ledger window they
// cancelled genuinely-uncovered tx-bearing ledgers one-for-one and Check 2,
// the hard gate above -ec-floor, reported zero deficiency on a real gap.
//
// This asserts the structural property that makes that cancellation
// impossible: the coverage count is a uniqExactIf over stellar.ledgers'
// tx-bearing rows, with ledger_entry_changes appearing ONLY inside the
// membership subquery.
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
	// source. A second top-level scan of it is the pre-fix shape.
	if n := strings.Count(q, "stellar.ledger_entry_changes"); n != 1 {
		t.Errorf("stellar.ledger_entry_changes referenced %d times, want exactly 1 (membership subquery only):\n%s", n, q)
	}
	// The exact pre-fix statement must be gone: an unrestricted uniqExact
	// over entry_changes is what counted tx_count == 0 ledgers as coverage.
	if strings.Contains(q, "uniqExact(ledger_seq) FROM stellar.ledger_entry_changes") {
		t.Errorf("pre-fix standalone entry-changes cardinality is still present:\n%s", q)
	}
	// Four positional placeholders: (subquery lo, hi, outer lo, hi).
	if n := strings.Count(q, "?"); n != 4 {
		t.Errorf("query has %d placeholders, want 4 (subquery lo/hi + outer lo/hi):\n%s", n, q)
	}
}
