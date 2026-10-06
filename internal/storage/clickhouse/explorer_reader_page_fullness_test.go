package clickhouse

import (
	"context"
	"strings"
	"testing"
)

// Regression tests for #290: an account page came back SHORT while older
// history remained, and the handlers emit `next_cursor` only on a FULL page
// (internal/api/v1/explorer/accounts.go — the OpenAPI contract says the cursor
// is "absent on the last page"), so a client walking the history stopped there
// with older transactions unreached. Silent truncation, not a cosmetic short
// page.
//
// Cause: the keyset merge of the sourced and participant arms took its limit
// over keys that still held cross-arm DUPLICATES. A tx the account sourced
// that ALSO carries it as a non-source participant of one of its operations is
// emitted by BOTH arms, so each such tx consumed two slots and the page lost a
// row per overlap. The merge (mergeKeysDesc) must dedupe BEFORE its cut, so
// the keyset is exactly min(limit, distinct keys older than the cursor).

// TestAccountTransactions_PageIsShortOnlyAtEndOfHistory: with cross-arm
// overlap present, a page must still carry `limit` rows whenever `limit`
// distinct txs remain — the premise of the handler's "emit next_cursor iff
// len(rows) == limit" rule.
func TestAccountTransactions_PageIsShortOnlyAtEndOfHistory(t *testing.T) {
	const limit = 5
	k := txKeyRow
	cases := []struct {
		name                 string
		sourced, participant [][]any
		want                 string // the hydrated key set
	}{
		{
			name:        "total overlap between the arms",
			sourced:     [][]any{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0), k(95, 0)},
			participant: [][]any{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0), k(95, 0)},
			want:        "IN ((100,0),(99,0),(98,0),(97,0),(96,0))",
		},
		{
			name:        "one overlapping tx at the head of the page",
			sourced:     [][]any{k(100, 0), k(98, 0), k(96, 0), k(94, 0)},
			participant: [][]any{k(100, 0), k(99, 0), k(97, 0), k(95, 0)},
			want:        "IN ((100,0),(99,0),(98,0),(97,0),(96,0))",
		},
		{
			name:        "same ledger, several txs, overlap tie-broken by tx_index",
			sourced:     [][]any{k(100, 9), k(100, 7), k(100, 5), k(100, 3)},
			participant: [][]any{k(100, 9), k(100, 8), k(100, 6), k(100, 4)},
			want:        "IN ((100,9),(100,8),(100,7),(100,6),(100,5))",
		},
		{
			// End of history: the page IS legitimately short.
			name:        "no overlap, fewer distinct txs than the page",
			sourced:     [][]any{k(100, 0), k(98, 0)},
			participant: [][]any{k(99, 0)},
			want:        "IN ((100,0),(99,0),(98,0))",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := &txArmRouter{sourced: tc.sourced, participant: tc.participant}
			conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
			if _, _, err := (&ExplorerReader{conn: conn}).AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
				t.Fatalf("AccountTransactions: %v", err)
			}
			if last := conn.queries[len(conn.queries)-1]; !strings.Contains(last, tc.want) {
				t.Fatalf("hydrated keys are not the distinct top %d (%s) — a page shorter than the limit while older "+
					"history remains makes the handler withhold next_cursor (#290): %s", limit, tc.want, last)
			}
		})
	}
}
