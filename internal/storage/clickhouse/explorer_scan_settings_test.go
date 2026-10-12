package clickhouse

import (
	"strings"
	"testing"
)

// Pins the per-query resource bounds on every scan-shaped explorer read: at default max_threads a probe used
// 40x the memory it uses at max_threads = 4. The SETTINGS clause is asserted in the SQL text.

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
		"sourcedTxKeysExactQuery(first page)":                   sourcedTxKeysExactQuery(false),
		"sourcedTxKeysExactQuery(cursor)":                       sourcedTxKeysExactQuery(true),
		"sourcedOpKeysExactQuery(first page)":                   sourcedOpKeysExactQuery(false, false),
		"sourcedOpKeysExactQuery(cursor)":                       sourcedOpKeysExactQuery(true, false),
		"sourcedOpKeysExactQuery(watermark)":                    sourcedOpKeysExactQuery(true, true),
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

// TestClassicCirculatingSupplyQuery_Bounds: the query needs its own execution ceiling, since the client's
// 30s default would kill it every time, and that ceiling must stay inside classicSupplyRefreshBudget so
// ClickHouse stops unwanted work.
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

// TestExplorerScanQueries_ShapePreserved pins the shape fragments whose loss would be a correctness
// regression, not just a perf one: per-arm LIMITs, LIMIT 1 BY dedup and the cursor tuple comparisons.
func TestExplorerScanQueries_ShapePreserved(t *testing.T) {
	for name, tc := range map[string]struct {
		q      string
		shapes []string
	}{
		"sourcedTxKeysExactQuery": {sourcedTxKeysExactQuery(true), []string{
			"(ledger_seq, tx_index) < (?, ?)", "LIMIT 1 BY ledger_seq, tx_index LIMIT ?", "FROM stellar.ops_by_source",
		}},
		"sourcedOpKeysExactQuery": {sourcedOpKeysExactQuery(true, false), []string{
			"(ledger_seq, tx_index, op_index) < (?, ?, ?)", "LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?", "op_index != 4294967295",
		}},
	} {
		for _, s := range tc.shapes {
			if !strings.Contains(tc.q, s) {
				t.Errorf("%s missing %q:\n%s", name, s, tc.q)
			}
		}
	}
	if strings.Contains(sourcedTxKeysExactQuery(false), "< (?, ?)") || strings.Contains(sourcedOpKeysExactQuery(false, true), "< (?, ?, ?)") {
		t.Error("a first-page exact arm must not carry a cursor clause")
	}

	// A first page carries no cursor clause on either arm. This is not asserted as "no WHERE" because the
	// tail-window bound is a WHERE on the first page; the arg count must match the clause count.
	for _, q := range []string{recentOperationsQuery(false, true), recentOperationsQuery(false, false)} {
		if strings.Contains(q, recentOperationsCursorPredicate) || strings.Contains(q, "(ledger_seq, tx_index, op_index) < (?, ?, ?)") {
			t.Errorf("recentOperationsQuery(false, …) must not carry a cursor clause:\n%s", q)
		}
		if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index") {
			t.Errorf("recentOperationsQuery(false, …) missing its DAT-10 LIMIT 1 BY dedup:\n%s", q)
		}
	}
	for _, q := range []string{recentOperationsQuery(true, true), recentOperationsQuery(true, false)} {
		// The keyset comparison is index-prunable: `ledger_seq < ? OR (ledger_seq = ? AND (tx_index, op_index) < (?,
		// ?))`. The forbidden tuple-only spelling is pinned in explorer_reader_union_bounds_test.go; this asserts a
		// full-key keyset.
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

// TestNativeHoldersQueries_Shape: native has no trustlines, so both queries must read the ACCOUNT entry
// range, exclude removed entries and count only funded accounts. Otherwise /v1/assets/native/holders
// silently serves holder_count 0.
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
