// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestSampleAccountIDsQuery_SeededChangeLogFrame pins that the -sample frame is drawn from the change log
// reconcile-balances proves (not ledger_entries_current) and that the seed orders it, so seeds give
// different cohorts.
func TestSampleAccountIDsQuery_SeededChangeLogFrame(t *testing.T) {
	q := strings.Join(strings.Fields(sampleAccountIDsQuery), " ")
	if !strings.Contains(q, "FROM stellar.ledger_entry_changes ") {
		t.Errorf("sample frame not drawn from stellar.ledger_entry_changes: %s", q)
	}
	if strings.Contains(q, "ledger_entries_current") {
		t.Errorf("sample frame reads the ledger_entries_current projection: %s", q)
	}
	if !strings.Contains(q, "ORDER BY cityHash64(account_id, ?)") {
		t.Errorf("sample order does not take a seed parameter: %s", q)
	}
	if got := strings.Count(q, "?"); got != 3 {
		t.Errorf("placeholders = %d, want 3 (minLedger, seed, n)", got)
	}
}
