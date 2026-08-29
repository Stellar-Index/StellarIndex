package clickhouse

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for #290: an account page came back SHORT while older
// history remained, and the handlers emit `next_cursor` only on a FULL page
// (internal/api/v1/explorer/accounts.go — the OpenAPI contract says the cursor
// is "absent on the last page"), so a client walking the history stopped there
// with older transactions unreached. Silent truncation, not a cosmetic short
// page.
//
// Cause: the two-phase keyset query's MERGE stage — the `UNION ALL` of the
// sourced and participant arms — took its `LIMIT ?` over rows that still
// contained cross-arm DUPLICATES, and only the hydration pass deduped. A tx
// the account sourced that ALSO carries it as a non-source participant of one
// of its operations (an op with its own source_account naming the tx's source)
// is emitted by BOTH arms, so each such tx consumed two of the merge's limit
// slots and the page lost a row per overlap. Fix: `LIMIT 1 BY` at the merge,
// BEFORE its LIMIT, so the keyset is exactly min(limit, distinct keys older
// than the cursor).

// keysetMergeStage returns the emitted query's merge stage — the line that
// closes the UNION subquery and applies the merge ORDER BY / LIMIT. It is the
// only line whose first non-space character closes that subquery.
func keysetMergeStage(t *testing.T, q string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(q, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ") ORDER BY") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one keyset-merge stage, found %d in:\n%s", len(found), q)
	}
	return found[0]
}

// emittedAccountQuery runs a reader call against the stub conn and returns the
// SQL it actually emitted, so these assertions track the implementation rather
// than a copy of it.
func emittedAccountQuery(t *testing.T, call func(*ExplorerReader) error) string {
	t.Helper()
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) { return &stubRows{}, nil }
	if err := call(&ExplorerReader{conn: conn}); err != nil {
		t.Fatalf("reader call: %v", err)
	}
	return conn.queries[len(conn.queries)-1]
}

func TestAccountTransactions_KeysetMergeDedupesBeforeItsLimit(t *testing.T) {
	q := emittedAccountQuery(t, func(r *ExplorerReader) error {
		_, err := r.AccountTransactions(context.Background(), "GTEST", 50, ExplorerCursor{})
		return err
	})
	merge := keysetMergeStage(t, q)
	if !strings.Contains(merge, "LIMIT 1 BY ledger_seq, tx_index LIMIT ?") {
		t.Errorf("the keyset merge must dedupe BEFORE taking its LIMIT — a tx that is both sourced by the account "+
			"and a non-source participant of one of its ops is emitted by both arms, eats two limit slots, and the "+
			"page comes back short; the handler then withholds next_cursor and the client truncates its history (#290):\n%s", merge)
	}
}

// stagedPage evaluates the emitted query's stages over two arms of keys:
// per-arm (ORDER BY, LIMIT 1 BY, LIMIT), the merge (ORDER BY, LIMIT 1 BY only
// when the emitted merge stage carries it, LIMIT), then the hydration pass's
// LIMIT 1 BY over the surviving keys. It returns the rows the endpoint would
// serve.
func stagedPage(arm1, arm2 []opKey, limit int, mergeDedupes bool) []opKey {
	arm := func(keys []opKey) []opKey { return firstN(dedupeSortedDesc(sortDesc(keys)), limit) }
	merged := append(append([]opKey{}, arm(arm1)...), arm(arm2)...) // UNION ALL
	merged = sortDesc(merged)
	if mergeDedupes {
		merged = dedupeSortedDesc(merged)
	}
	return dedupeSortedDesc(firstN(merged, limit)) // merge LIMIT, then hydration's LIMIT 1 BY
}

func sortDesc(keys []opKey) []opKey {
	out := append([]opKey{}, keys...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ledger != out[j].ledger {
			return out[i].ledger > out[j].ledger
		}
		if out[i].txIndex != out[j].txIndex {
			return out[i].txIndex > out[j].txIndex
		}
		return out[i].opIndex > out[j].opIndex
	})
	return out
}

func dedupeSortedDesc(keys []opKey) []opKey {
	seen := map[opKey]bool{}
	out := make([]opKey, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func firstN(keys []opKey, n int) []opKey {
	if len(keys) > n {
		return keys[:n]
	}
	return keys
}

// TestAccountTransactions_PageIsShortOnlyAtEndOfHistory is the correctness half
// of #290: with cross-arm overlap present, a page must still carry `limit`
// rows whenever `limit` distinct txs remain — that is exactly the premise the
// handler's "emit next_cursor iff len(rows) == limit" rule rests on. The merge
// semantics are read off the SQL the reader actually emits, so removing the
// merge dedupe fails this test rather than only the shape assertion above.
func TestAccountTransactions_PageIsShortOnlyAtEndOfHistory(t *testing.T) {
	const limit = 5
	q := emittedAccountQuery(t, func(r *ExplorerReader) error {
		_, err := r.AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{})
		return err
	})
	mergeDedupes := strings.Contains(keysetMergeStage(t, q), "LIMIT 1 BY ledger_seq, tx_index LIMIT ?")

	k := func(ledger, txIndex uint32) opKey { return opKey{ledger, txIndex, 0} }
	cases := []struct {
		name       string
		arm1, arm2 []opKey
		want       []opKey // the page the endpoint must serve
	}{
		{
			// Every sourced tx also carries the account as a non-source
			// participant: pre-fix the merge's 5 slots held 5 rows but only
			// 3 distinct keys, so the page was short by 2 with history left.
			name: "total overlap between the arms",
			arm1: []opKey{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0), k(95, 0)},
			arm2: []opKey{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0), k(95, 0)},
			want: []opKey{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0)},
		},
		{
			name: "one overlapping tx at the head of the page",
			arm1: []opKey{k(100, 0), k(98, 0), k(96, 0), k(94, 0)},
			arm2: []opKey{k(100, 0), k(99, 0), k(97, 0), k(95, 0)},
			want: []opKey{k(100, 0), k(99, 0), k(98, 0), k(97, 0), k(96, 0)},
		},
		{
			name: "same ledger, several txs, overlap tie-broken by tx_index",
			arm1: []opKey{k(100, 9), k(100, 7), k(100, 5), k(100, 3)},
			arm2: []opKey{k(100, 9), k(100, 8), k(100, 6), k(100, 4)},
			want: []opKey{k(100, 9), k(100, 8), k(100, 7), k(100, 6), k(100, 5)},
		},
		{
			// End of history: the page IS legitimately short, which is what
			// lets the handler withhold next_cursor.
			name: "no overlap, fewer distinct txs than the page",
			arm1: []opKey{k(100, 0), k(98, 0)},
			arm2: []opKey{k(99, 0)},
			want: []opKey{k(100, 0), k(99, 0), k(98, 0)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stagedPage(tc.arm1, tc.arm2, limit, mergeDedupes)
			if len(got) != len(tc.want) {
				t.Fatalf("page has %d rows, want %d — a page shorter than the limit while older history remains "+
					"makes the handler withhold next_cursor and the client stop early (#290)\n got: %v\nwant: %v",
					len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("row %d = %v, want %v\n got: %v\nwant: %v", i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}
