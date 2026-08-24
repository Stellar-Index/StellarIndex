package clickhouse

import (
	"strings"
	"testing"
)

// These tests pin the per-query resource bounds on every SCAN-SHAPED explorer
// read (route-sweep 2026-07-29). The measured fact they encode: at DEFAULT
// max_threads a ledger_entries_current probe fanned out over the post-D3 part
// layout to 4.76 GiB — 40× the 89 MiB the identical probe costs at
// max_threads = 4. Losing the SETTINGS clause from any of these queries
// silently reintroduces that class, so the clause is asserted per query, in
// the SQL text (the same convention recognition_query_test.go /
// event_reader_test.go use for their bounded scans).

// requireScanSettings asserts q carries the explorerScanSettings pins.
func requireScanSettings(t *testing.T, name, q string) {
	t.Helper()
	for _, s := range []string{
		"SETTINGS",
		"max_threads = 4",
		"max_memory_usage = 8589934592",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("%s missing bounded setting %q:\n%s", name, s, q)
		}
	}
}

func TestExplorerScanQueries_CarryBoundedSettings(t *testing.T) {
	for name, q := range map[string]string{
		"recentOperationsQuery(first page)":     recentOperationsQuery(false),
		"recentOperationsQuery(cursor)":         recentOperationsQuery(true),
		"opTypeStatsQuery":                      opTypeStatsQuery,
		"accountOpTypeCountsQuery":              accountOpTypeCountsQuery,
		"accountTransactionsQuery(first page)":  accountTransactionsQuery(false),
		"accountTransactionsQuery(cursor)":      accountTransactionsQuery(true),
		"accountOperationsQuery(first page)":    accountOperationsQuery(false, false),
		"accountOperationsQuery(cursor)":        accountOperationsQuery(true, false),
		"accountOperationsQuery(watermark)":     accountOperationsQuery(true, true),
		"contractEventsRecentQuery(first page)": contractEventsRecentQuery(false, false),
		"contractEventsRecentQuery(cursor)":     contractEventsRecentQuery(true, false),
		"recentContractsQuery":                  recentContractsQuery,
		"contractInteractionsQuery":             contractInteractionsQuery,
		"contractCodeHistoryQuery":              contractCodeHistoryQuery,
		"accountTrustlinesQuery":                accountTrustlinesQuery,
		"accountOffersQuery":                    accountOffersQuery,
		"assetHoldersQuery":                     assetHoldersQuery,
		"assetHoldersCountQuery":                assetHoldersCountQuery,
		"nativeHoldersQuery":                    nativeHoldersQuery,
		"nativeHoldersCountQuery":               nativeHoldersCountQuery,
		"accountsUnspendableQuery":              accountsUnspendableQuery,
		"accountMovementsQuery(bare)":           accountMovementsQuery(AccountMovementFilter{}, false),
		"accountMovementsQuery(filter+cursor)": accountMovementsQuery(AccountMovementFilter{
			Kind: "payment", Direction: AccountMovementSent, Asset: "native",
		}, true),
	} {
		requireScanSettings(t, name, q)
	}
}

// TestAccountsByWealthQuery_Bounds — the wealth refresh additionally carries
// the 150s execution ceiling (its Go-side budget is 3 min; the connection
// default of 30s killed it silently — see accountsByWealthQuery's doc).
func TestAccountsByWealthQuery_Bounds(t *testing.T) {
	requireScanSettings(t, "accountsByWealthQuery", accountsByWealthQuery)
	if !strings.Contains(accountsByWealthQuery, "max_execution_time = 150") {
		t.Errorf("accountsByWealthQuery missing its 150s execution ceiling:\n%s", accountsByWealthQuery)
	}
}

// TestExplorerScanQueries_ShapePreserved pins the load-bearing shape
// fragments of the refactored builders — the parts whose loss would be a
// CORRECTNESS regression, not just a perf one (per-arm LIMITs, LIMIT 1 BY
// dedup, the cursor tuple comparisons).
func TestExplorerScanQueries_ShapePreserved(t *testing.T) {
	txQ := accountTransactionsQuery(true)
	for _, s := range []string{
		"SELECT DISTINCT",
		"UNION ALL",
		"(ledger_seq, tx_index) < (?, ?)",
		"LIMIT 1 BY ledger_seq, tx_index",
		"operation_participants",
	} {
		if !strings.Contains(txQ, s) {
			t.Errorf("accountTransactionsQuery missing %q:\n%s", s, txQ)
		}
	}
	// Each arm carries its own cursor clause (two occurrences).
	if got := strings.Count(txQ, "(ledger_seq, tx_index) < (?, ?)"); got != 2 {
		t.Errorf("accountTransactionsQuery cursor clause count = %d, want 2 (one per arm)", got)
	}

	opQ := accountOperationsQuery(true, false)
	for _, s := range []string{
		"UNION ALL",
		"(ledger_seq, tx_index, op_index) < (?, ?, ?)",
		"LIMIT 1 BY ledger_seq, tx_index, op_index",
		"operation_participants",
	} {
		if !strings.Contains(opQ, s) {
			t.Errorf("accountOperationsQuery missing %q:\n%s", s, opQ)
		}
	}
	if got := strings.Count(opQ, "(ledger_seq, tx_index, op_index) < (?, ?, ?)"); got != 2 {
		t.Errorf("accountOperationsQuery cursor clause count = %d, want 2 (one per arm)", got)
	}

	// First pages carry no cursor clause.
	if strings.Contains(accountTransactionsQuery(false), "< (?, ?)") {
		t.Error("accountTransactionsQuery(false) must not carry a cursor clause")
	}
	if strings.Contains(recentOperationsQuery(false), "WHERE") {
		t.Error("recentOperationsQuery(false) must not carry a WHERE clause")
	}
	if !strings.Contains(recentOperationsQuery(true), "(ledger_seq, tx_index, op_index) < (?, ?, ?)") {
		t.Error("recentOperationsQuery(true) missing its cursor tuple comparison")
	}
	if !strings.Contains(recentOperationsQuery(false), "LIMIT 1 BY ledger_seq, tx_index, op_index") {
		t.Error("recentOperationsQuery missing its DAT-10 LIMIT 1 BY dedup")
	}

	// The movements builder's clause order must mirror its arg order.
	mq := accountMovementsQuery(AccountMovementFilter{Kind: "k", Direction: AccountMovementSent, Asset: "a"}, true)
	kindIdx := strings.Index(mq, "movement_kind = ?")
	dirIdx := strings.Index(mq, "direction = ?")
	assetIdx := strings.Index(mq, "asset = ?")
	curIdx := strings.Index(mq, "(ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
	if !(kindIdx >= 0 && dirIdx > kindIdx && assetIdx > dirIdx && curIdx > assetIdx) {
		t.Errorf("accountMovementsQuery clause order does not match arg order:\n%s", mq)
	}
}

// TestNativeHoldersQueries_Shape pins the native arm of AssetHolders
// (2026-07-31: /v1/assets/native/holders served {"holder_count":0} by
// construction because native has no trustlines). The load-bearing facts:
// both native queries read the ACCOUNT entry range — entry_type is the
// FIRST ORDER BY column, so this is a primary-index range read, and losing
// the predicate (or reintroducing a trustline/asset one) silently restores
// the empty board — exclude removed entries, and count only funded
// accounts.
func TestNativeHoldersQueries_Shape(t *testing.T) {
	for name, q := range map[string]string{
		"nativeHoldersQuery":      nativeHoldersQuery,
		"nativeHoldersCountQuery": nativeHoldersCountQuery,
	} {
		for _, s := range []string{
			"entry_type = 'account'",
			"change_type != 'removed'",
			"balance > 0",
		} {
			if !strings.Contains(q, s) {
				t.Errorf("%s missing %q:\n%s", name, s, q)
			}
		}
		if strings.Contains(q, "trustline") || strings.Contains(q, "asset = ?") {
			t.Errorf("%s must not carry a trustline/asset predicate — native has no trustlines:\n%s", name, q)
		}
	}
	if !strings.Contains(nativeHoldersQuery, "ORDER BY balance DESC") {
		t.Errorf("nativeHoldersQuery lost its balance ranking:\n%s", nativeHoldersQuery)
	}
}
