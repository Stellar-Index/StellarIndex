package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// boardsStub serves the account-board reads: a provisioned rollup whose
// keyed read finds no row, and stats rows carrying the cycle's computed_at.
// Rows are shaped by the SELECT list, as ClickHouse returns them.
func boardsStub(t *testing.T, rollup, stats string, cycleAt time.Time) *stubConn {
	t.Helper()
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "SELECT rank FROM "+rollup+" LIMIT 1"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "FROM "+rollup+" WHERE"):
			return &stubRows{}, nil
		case strings.Contains(q, "FROM "+stats):
			withAt := strings.Contains(q, "computed_at")
			var data [][]any
			for _, m := range []struct {
				metric string
				value  int64
			}{{"from_ledger", 3}, {"thru_ledger", 1000}} {
				row := []any{m.metric, m.value}
				if withAt {
					row = append(row, cycleAt)
				}
				data = append(data, row)
			}
			return &stubRows{data: data}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
}

// A keyed miss ("this account never created one") is a served 200 whose
// computed_at must still be the cycle's, not Go's zero time rendered as
// 0001-01-01T00:00:00Z.
func TestAccountCreatorsKeyedMissCarriesTheCycleTime(t *testing.T) {
	cycleAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := &ExplorerReader{conn: boardsStub(t, "stellar.account_creators_rollup",
		"stellar.account_creators_stats", cycleAt)}

	got, ok, err := r.AccountCreators(t.Context(), 50, "GNOBODY")
	if err != nil || !ok {
		t.Fatalf("AccountCreators(keyed miss) = ok %v, err %v; want a served snapshot", ok, err)
	}
	if len(got.Board) != 0 {
		t.Fatalf("Board = %+v, want empty for a keyed miss", got.Board)
	}
	if !got.ComputedAt.Equal(cycleAt) {
		t.Errorf("ComputedAt = %s, want the cycle's %s", got.ComputedAt, cycleAt)
	}
}

func TestAccountSponsorsKeyedMissCarriesTheCycleTime(t *testing.T) {
	cycleAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := &ExplorerReader{conn: boardsStub(t, "stellar.account_sponsors_rollup",
		"stellar.account_sponsors_stats", cycleAt)}

	got, ok, err := r.AccountSponsors(t.Context(), 50, "GNOBODY")
	if err != nil || !ok {
		t.Fatalf("AccountSponsors(keyed miss) = ok %v, err %v; want a served snapshot", ok, err)
	}
	if len(got.Board) != 0 {
		t.Fatalf("Board = %+v, want empty for a keyed miss", got.Board)
	}
	if !got.ComputedAt.Equal(cycleAt) {
		t.Errorf("ComputedAt = %s, want the cycle's %s", got.ComputedAt, cycleAt)
	}
}

// createDDL returns the comment-stripped CREATE statement for object in a
// deploy/clickhouse file, or "" when the file declares none.
func createDDL(t *testing.T, file, object string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "deploy", "clickhouse", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	text := strings.Join(kept, "\n")
	start := strings.Index(text, "CREATE TABLE IF NOT EXISTS "+object+"\n")
	if start < 0 {
		return ""
	}
	end := strings.Index(text[start:], ";")
	if end < 0 {
		t.Fatalf("%s: unterminated CREATE for %s", file, object)
	}
	return text[start : start+end]
}

// TestAccountBoardKeyedReadsAreIndexed pins each keyed board read's filter
// column to something ClickHouse can prune on: the sort-key prefix or a
// bloom_filter skip index. The rollups are ORDER BY rank for the top-N page,
// so a bare `WHERE creator = ?` reads every granule to return at most one
// row. Checked against both the fresh-host DDL and the operator mirror.
func TestAccountBoardKeyedReadsAreIndexed(t *testing.T) {
	where := regexp.MustCompile(`FROM (stellar\.\w+) WHERE (\w+) = \?\s*$`)
	for _, tc := range []struct {
		query, mirror string
	}{
		{creatorsBoardKeyedSQL, "account_creators_rollup.sql"},
		{sponsorsBoardKeyedSQL, "account_sponsors_rollup.sql"},
	} {
		m := where.FindStringSubmatch(tc.query)
		if m == nil {
			t.Fatalf("keyed board read is not a single-column equality: %q", tc.query)
		}
		table, col := m[1], m[2]
		sortPrefix := regexp.MustCompile(`ORDER BY \(?\s*` + col + `\b`)
		skipIndex := regexp.MustCompile(`INDEX \w+ ` + col + ` TYPE bloom_filter\b`)
		for _, file := range []string{"tier1_schema.sql", tc.mirror} {
			ddl := createDDL(t, file, table)
			if ddl == "" {
				t.Fatalf("%s declares no %s", file, table)
			}
			if !sortPrefix.MatchString(ddl) && !skipIndex.MatchString(ddl) {
				t.Errorf("%s: %s filters on %s, but the table neither sorts by it nor "+
					"declares a bloom_filter skip index on it — every keyed read is a full scan:\n%s",
					file, table, col, ddl)
			}
		}
	}
}
