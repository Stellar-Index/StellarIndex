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
		"recentOperationsQuery(first page)":                     recentOperationsQuery(false, true),
		"recentOperationsQuery(first page, unbounded fallback)": recentOperationsQuery(false, false),
		"recentOperationsQuery(cursor)":                         recentOperationsQuery(true, true),
		"recentOperationsQuery(cursor, unbounded fallback)":     recentOperationsQuery(true, false),
		"opTypeStatsQuery":                                      opTypeStatsQuery,
		"accountOpTypeCountsQuery":                              accountOpTypeCountsQuery,
		"accountTransactionsQuery(first page)":                  accountTransactionsQuery(false),
		"accountTransactionsQuery(cursor)":                      accountTransactionsQuery(true),
		"accountOperationsQuery(first page)":                    accountOperationsQuery(false, false),
		"accountOperationsQuery(cursor)":                        accountOperationsQuery(true, false),
		"accountOperationsQuery(watermark)":                     accountOperationsQuery(true, true),
		"contractEventsRecentQuery(first page)":                 contractEventsRecentQuery(false, false),
		"contractEventsRecentQuery(cursor)":                     contractEventsRecentQuery(true, false),
		"recentContractsQuery":                                  recentContractsQuery,
		"contractInteractionsQuery":                             contractInteractionsQuery,
		"contractCodeHistoryQuery":                              contractCodeHistoryQuery,
		"accountTrustlinesQuery":                                accountTrustlinesQuery,
		"accountOffersQuery":                                    accountOffersQuery,
		"assetHoldersQuery":                                     assetHoldersQuery,
		"assetHoldersCountQuery":                                assetHoldersCountQuery,
		"nativeHoldersQuery":                                    nativeHoldersQuery,
		"nativeHoldersCountQuery":                               nativeHoldersCountQuery,
		"accountsUnspendableQuery":                              accountsUnspendableQuery,
		"classicCirculatingSupplyQuery":                         classicCirculatingSupplyQuery,
		"accountMovementsQuery(bare)":                           accountMovementsQuery(AccountMovementFilter{}, false),
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

// TestClassicCirculatingSupplyQuery_Bounds — this one additionally
// carries an explicit execution ceiling, and the reason is the same as
// accountsByWealthQuery's: the Go client pins the connection default to
// 30s, which is BELOW this query's measured runtime (avg 24-27s, max
// 75.5s on r1 over 14 days). It survives today only because r1's
// api_serving profile overrides the value to 184 — a deployment without
// that override would kill it every time, and the classic-supply
// fallback would just stop having answers with no error anyone reads.
// The ceiling must also stay INSIDE the caller's 3-minute detached
// refresh budget (classicSupplyRefreshBudget), or ClickHouse keeps
// working on a query nobody is waiting for.
func TestClassicCirculatingSupplyQuery_Bounds(t *testing.T) {
	requireScanSettings(t, "classicCirculatingSupplyQuery", classicCirculatingSupplyQuery)
	if !strings.Contains(classicCirculatingSupplyQuery, "max_execution_time = 150") {
		t.Errorf("classicCirculatingSupplyQuery missing its 150s execution ceiling:\n%s",
			classicCirculatingSupplyQuery)
	}
	// ADR-0003: the sum must be taken in i128, not the column's native
	// width. Losing toInt128 here does not error — it silently wraps on
	// a large-supply asset.
	if !strings.Contains(classicCirculatingSupplyQuery, "sum(toInt128(balance))") {
		t.Errorf("classicCirculatingSupplyQuery lost its i128 sum (ADR-0003):\n%s",
			classicCirculatingSupplyQuery)
	}
}

// TestExplorerScanQueries_ShapePreserved pins the load-bearing shape
// fragments of the refactored builders — the parts whose loss would be a
// CORRECTNESS regression, not just a perf one (per-arm LIMITs, LIMIT 1 BY
// dedup, the cursor tuple comparisons).
func TestExplorerScanQueries_ShapePreserved(t *testing.T) {
	txQ := accountTransactionsQuery(true)
	// Tx-key dedupe inside the arms is `LIMIT 1 BY ledger_seq, tx_index`
	// over the account-keyed tables (2026-08-28 — it replaced the
	// `SELECT DISTINCT` of the old resolve-over-stellar.transactions arms;
	// see TestAccountListings_ArmsPageAccountKeyedTables).
	for _, s := range []string{
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
	// A first page carries no CURSOR clause on either arm. (Until
	// 2026-09-02 this was asserted as "no WHERE at all", which was a
	// faithful proxy only while the query had no lower bound either — the
	// #444 tail-window bound is a WHERE on the first page, and is pinned
	// in explorer_reader_recent_operations_test.go. The property this row
	// actually protects — the arg count matching the clause count — is
	// now asserted directly.)
	for _, q := range []string{recentOperationsQuery(false, true), recentOperationsQuery(false, false)} {
		if strings.Contains(q, recentOperationsCursorPredicate) || strings.Contains(q, "(ledger_seq, tx_index, op_index) < (?, ?, ?)") {
			t.Errorf("recentOperationsQuery(false, …) must not carry a cursor clause:\n%s", q)
		}
		if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index") {
			t.Errorf("recentOperationsQuery(false, …) missing its DAT-10 LIMIT 1 BY dedup:\n%s", q)
		}
	}
	for _, q := range []string{recentOperationsQuery(true, true), recentOperationsQuery(true, false)} {
		// The keyset comparison is now spelled index-prunably (#484) —
		// `ledger_seq < ? OR (ledger_seq = ? AND (tx_index, op_index) <
		// (?, ?))`, exactly equivalent to the tuple form it replaced. The
		// full shape (including the forbidden tuple-only spelling) is
		// pinned in explorer_reader_deep_cursor_test.go; this row keeps
		// asserting the property it always asserted, that a cursor arm
		// carries a full-primary-key keyset comparison.
		if !strings.Contains(q, recentOperationsCursorPredicate) {
			t.Errorf("recentOperationsQuery(true, …) missing its keyset cursor comparison:\n%s", q)
		}
		if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index") {
			t.Errorf("recentOperationsQuery(true, …) missing its DAT-10 LIMIT 1 BY dedup:\n%s", q)
		}
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
