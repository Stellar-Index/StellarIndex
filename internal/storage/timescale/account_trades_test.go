package timescale

import (
	"strings"
	"testing"
)

// TestAccountTradesQuery_Shape pins the per-address trades read to the
// two-arm UNION discipline its file header documents: each arm keys one
// account column (taker / maker — the columns migration 0123 indexes),
// carries its OWN ORDER BY + LIMIT so it walks its partial index in
// output order and stops after one page, and the maker arm excludes
// taker-duplicated rows so a self-crossed trade appears once.
func TestAccountTradesQuery_Shape(t *testing.T) {
	for _, hasCursor := range []bool{false, true} {
		q := accountTradesQuery(hasCursor)

		parts := strings.Split(q, "UNION ALL")
		if len(parts) != 2 {
			t.Fatalf("hasCursor=%v: query is not a two-arm UNION:\n%s", hasCursor, q)
		}
		if !strings.Contains(parts[0], "WHERE taker = $1") {
			t.Errorf("hasCursor=%v: arm 1 must key the taker index:\n%s", hasCursor, parts[0])
		}
		if !strings.Contains(parts[1], "WHERE maker = $1 AND (taker IS NULL OR taker <> $1)") {
			t.Errorf("hasCursor=%v: arm 2 must key the maker index and exclude taker-arm rows:\n%s", hasCursor, parts[1])
		}
		const orderBy = "ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC LIMIT"
		if got := strings.Count(q, orderBy); got != 3 {
			t.Errorf("hasCursor=%v: want the full ORDER BY+LIMIT on both arms AND the outer merge (3 total), got %d:\n%s",
				hasCursor, got, q)
		}

		wantCursor := strings.Count(q, "(ts, ledger, tx_hash, op_index) < ($2, $3, $4, $5)")
		if hasCursor && wantCursor != 2 {
			t.Errorf("cursor page: both arms must carry the keyset tuple comparison, got %d:\n%s", wantCursor, q)
		}
		if !hasCursor && wantCursor != 0 {
			t.Errorf("first page: no cursor clause expected, got %d:\n%s", wantCursor, q)
		}
	}
}

// TestAccountTradesQuery_NoFloatCasts — ADR-0003: the NUMERIC amount
// columns must reach the driver as text, never through a float cast.
func TestAccountTradesQuery_NoFloatCasts(t *testing.T) {
	q := accountTradesQuery(true)
	for _, col := range []string{"base_amount::text", "quote_amount::text", "usd_volume::text"} {
		if !strings.Contains(q, col) {
			t.Errorf("query must cast %s server-side:\n%s", col, q)
		}
	}
	for _, bad := range []string{"::float", "::double", "::real"} {
		if strings.Contains(q, bad) {
			t.Errorf("query must not cast NUMERIC through %s (ADR-0003):\n%s", bad, q)
		}
	}
}

func TestClampAccountTradesLimit(t *testing.T) {
	cases := map[int]int{
		-1:   accountTradesDefaultLimit,
		0:    accountTradesDefaultLimit,
		1:    1,
		50:   50,
		200:  200,
		201:  accountTradesDefaultLimit,
		9999: accountTradesDefaultLimit,
	}
	for in, want := range cases {
		if got := clampAccountTradesLimit(in); got != want {
			t.Errorf("clampAccountTradesLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
