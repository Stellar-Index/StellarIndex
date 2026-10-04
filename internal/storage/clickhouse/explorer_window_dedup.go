package clickhouse

import (
	"strconv"
	"strings"
)

// A per-arm `LIMIT 1 BY <key>` sits between the sort-key ORDER BY and the
// LIMIT, which stops ClickHouse's read-in-order early exit: the read walks the
// account's (or table's) whole range before the cut. The page readers instead
// read a bounded WINDOW of rows in sort-key order, collapse adjacent
// duplicate keys here, and fall back to the exact LIMIT 1 BY query only when
// the window cannot prove it holds a full page of distinct keys.

const (
	// windowFactorKeys: row-per-key tables (operations, movements). Duplicates
	// are un-merged ReplacingMergeTree parts, so a small multiple suffices.
	windowFactorKeys = 2
	// windowFactorTxArm: ops_by_source / operation_participants carry several
	// rows per transaction (tx sentinel + one per op), so a tx arm needs a
	// wider window to hold `limit` distinct transactions.
	windowFactorTxArm = 16
)

// windowRows is the row budget one windowed read asks for.
func windowRows(limit, factor int) int { return limit*factor + 8 }

// dedupWindow collapses adjacent same-key rows of a sort-key-ordered window
// read of `window` rows (newer picks the surviving duplicate; nil keeps the
// first). ok is false when the window was filled and holds fewer than `limit`
// complete key groups: the arm may have further keys the window never saw, so
// the caller must use the exact query. When the window is filled the final
// group is dropped, as its remaining duplicates may lie beyond the window.
func dedupWindow[R any, K comparable](rows []R, window, limit int, key func(R) K, newer func(a, b R) bool) (out []R, ok bool) {
	for i, r := range rows {
		if i > 0 && key(rows[i-1]) == key(r) {
			if last := len(out) - 1; newer != nil && newer(r, out[last]) {
				out[last] = r
			}
			continue
		}
		out = append(out, r)
	}
	if len(rows) < window {
		return out, true
	}
	if len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, len(out) >= limit
}

// tupleLiteral renders uint32 key columns as a SQL tuple. Values are numeric
// ClickHouse keys, never caller text, so there is nothing to escape.
func tupleLiteral(cols ...uint32) string {
	var sb strings.Builder
	sb.WriteByte('(')
	for i, c := range cols {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatUint(uint64(c), 10))
	}
	sb.WriteByte(')')
	return sb.String()
}

// tupleList renders `(a,b),(c,d),…` for an IN (…) list.
func tupleList[K any](keys []K, cols func(K) []uint32) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = tupleLiteral(cols(k)...)
	}
	return strings.Join(parts, ",")
}
