package clickhouse

import (
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// A per-arm `LIMIT 1 BY <key>` between the sort-key ORDER BY and the LIMIT stops ClickHouse's
// read-in-order early exit: the read walks the account's (or table's) whole range before the cut.
// The page readers instead read a bounded WINDOW of rows in sort-key order, collapse adjacent
// duplicate keys here, and fall back to the exact LIMIT 1 BY query only when the window cannot
// prove it holds a full page of distinct keys.

const (
	// windowFactorKeys: row-per-key tables (operations, movements). Duplicates
	// are un-merged ReplacingMergeTree parts, so a small multiple suffices.
	windowFactorKeys = 2
	// windowFactorTxArm: ops_by_source / operation_participants carry several rows per transaction (tx sentinel + one per op), so a tx arm
	// needs a wider window to hold `limit` distinct transactions.
	windowFactorTxArm = 16
)

// exactFallbackReads is the closed `read` label set of stellarindex_explorer_exact_fallback_total, pre-set so a rate never starts from a
// missing series.
var exactFallbackReads = []string{"recent_operations", "sourced_tx_keys", "sourced_op_keys", "account_movements", "asset_movements", "contract_events"}

func init() {
	for _, r := range exactFallbackReads {
		obs.ExplorerExactFallbackTotal.WithLabelValues(r)
	}
}

// exactFallback counts one window that could not prove a full page, so the read re-ran as the exact O(history) query.
func exactFallback(read string) { obs.ExplorerExactFallbackTotal.WithLabelValues(read).Inc() }

// windowRows is the row budget one windowed read asks for.
func windowRows(limit, factor int) int { return limit*factor + 8 }

// dedupWindow collapses adjacent same-key rows of a sort-key-ordered window read of `window` rows (newer picks the survivor; nil keeps the
// first). ok is false when the window was filled with fewer than `limit` complete key groups: further keys may lie beyond it, so the
// caller must use the exact query. When filled, the final group is dropped, as its remaining duplicates may lie beyond the window.
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
