package clickhouse

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// classicSupplyConn returns an ExplorerReader with no holders rollup whose scan answers with the given (asset, circulating) rows, plus
// the recorder so the SQL can be inspected.
func classicSupplyConn(t *testing.T, rows [][]any) (*ExplorerReader, *stubConn) {
	t.Helper()
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.asset_holders_counts") {
			return nil, errors.New("Table stellar.asset_holders_counts does not exist")
		}
		if !strings.Contains(q, "stellar.ledger_entries_current") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{data: rows}, nil
	}}
	return &ExplorerReader{conn: conn}, conn
}

// TestClassicCirculatingSupply_PreservesAboveInt64Totals pins ADR-0003 for this reader: the Int128
// per-asset total must stay a STRING end to end. The 2^70 stroops fixture shows any int64 hop.
func TestClassicCirculatingSupply_PreservesAboveInt64Totals(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 70).String() // 1180591620717411303424
	if len(huge) <= len("9223372036854775807") {
		t.Fatalf("test premise broken: %s is not wider than int64", huge)
	}

	r, _ := classicSupplyConn(t, [][]any{
		{"USDC-" + testIssuer, huge},
		{"EURC-" + testIssuer, "1234567890123456789012345678"},
	})

	got, err := r.ClassicCirculatingSupply(t.Context())
	if err != nil {
		t.Fatalf("ClassicCirculatingSupply: %v", err)
	}
	if v := got["USDC-"+testIssuer]; v != huge {
		t.Errorf("USDC circulating = %q, want %q (ADR-0003: an i128 total must not be truncated)", v, huge)
	}
	if v := got["EURC-"+testIssuer]; v != "1234567890123456789012345678" {
		t.Errorf("EURC circulating = %q, want the 28-digit total verbatim", v)
	}
}

// TestClassicCirculatingSupply_OmitsUnusableRows: rows with an empty asset or total, or an exact "0", are
// dropped; publishing "0" would feed a zero market cap into /v1/assets instead of leaving the field absent.
func TestClassicCirculatingSupply_OmitsUnusableRows(t *testing.T) {
	r, _ := classicSupplyConn(t, [][]any{
		{"GOOD-" + testIssuer, "500"},
		{"", "999"},                        // no asset id
		{"NOTOTAL-" + testIssuer, ""},      // no total
		{"ZERO-" + testIssuer, "0"},        // exact zero
		{"NEGZERO-" + testIssuer, "-0"},    // not the literal "0" — kept
		{"SMALL-" + testIssuer, "0000001"}, // not the literal "0" — kept
	})

	got, err := r.ClassicCirculatingSupply(t.Context())
	if err != nil {
		t.Fatalf("ClassicCirculatingSupply: %v", err)
	}
	want := map[string]string{
		"GOOD-" + testIssuer:    "500",
		"NEGZERO-" + testIssuer: "-0",
		"SMALL-" + testIssuer:   "0000001",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d assets %v, want %d %v", len(got), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("supply[%q] = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"", "NOTOTAL-" + testIssuer, "ZERO-" + testIssuer} {
		if _, present := got[k]; present {
			t.Errorf("supply[%q] present, want omitted", k)
		}
	}
}

// TestClassicCirculatingSupply_QueryShape pins the summed population: trustline entries only, non-removed,
// balance > 0, grouped per asset, with toInt128 + toString for ADR-0003 no-truncation.
func TestClassicCirculatingSupply_QueryShape(t *testing.T) {
	r, conn := classicSupplyConn(t, nil)
	if _, err := r.ClassicCirculatingSupply(t.Context()); err != nil {
		t.Fatalf("ClassicCirculatingSupply: %v", err)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want the rollup probe and exactly 1 scan (this read is far too heavy to fan out)", len(conn.queries))
	}
	q := conn.queries[1]
	for _, s := range []string{
		"entry_type = 'trustline'",
		"change_type != 'removed'",
		"balance > 0",
		"GROUP BY asset",
		"toString(sum(toInt128(balance)))",
		"stellar.ledger_entries_current FINAL",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("ClassicCirculatingSupply query missing %q:\n%s", s, q)
		}
	}
	// A plain sum() over Int64 balances is the truncation this guards.
	if strings.Contains(q, "sum(balance)") {
		t.Errorf("ClassicCirculatingSupply sums raw Int64 balances — the total overflows int64 (ADR-0003):\n%s", q)
	}
	// No bound arguments: the read is over the whole current-state trustline
	// slice, so a stray placeholder would mean an unbound clause.
	if got := len(conn.args[1]); got != 0 {
		t.Errorf("ClassicCirculatingSupply bound %d args, want 0", got)
	}
}

// TestClassicCirculatingSupply_QueryErrorPropagates: refreshClassicSupply keeps its last-good map only
// because this errors; a nil-error empty map would be stored as truth and blank every classic market cap.
func TestClassicCirculatingSupply_QueryErrorPropagates(t *testing.T) {
	boom := errors.New("memory limit exceeded")
	r := &ExplorerReader{conn: &stubConn{respond: func(string) (driver.Rows, error) { return nil, boom }}}

	got, err := r.ClassicCirculatingSupply(t.Context())
	if err == nil {
		t.Fatalf("ClassicCirculatingSupply hid a query failure; got map %v", got)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
	if got != nil {
		t.Errorf("map = %v, want nil on error", got)
	}
}

// TestClassicCirculatingSupply_TruncatedStreamIsAnError pins that a scan dying part-way returns an error, not
// a partial map the caller would store as complete, zeroing the market cap of every unreached asset.
func TestClassicCirculatingSupply_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("connection reset mid-scan")
	r := &ExplorerReader{conn: &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{{"USDC-" + testIssuer, "500"}}, streamErr: truncated}, nil
	}}}

	got, err := r.ClassicCirculatingSupply(t.Context())
	if !errors.Is(err, truncated) {
		t.Fatalf("err = %v (map %v), want the truncation error — a partial supply map must not be reported as complete", err, got)
	}
}

// rollupSupplyConn answers the holders-rollup read with rows (asset, circulating, holders, computed_at) and the scan with scanRows.
func rollupSupplyConn(rows, scanRows [][]any) *stubConn {
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.asset_holders_counts") {
			return &stubRows{data: rows}, nil
		}
		return &stubRows{data: scanRows}, nil
	}}
}

// A fresh rollup cycle answers without the 150M-row trustline scan, keeping i128 totals as strings and dropping zero rows.
func TestClassicCirculatingSupply_ServesFreshRollup(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 70).String()
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	conn := rollupSupplyConn([][]any{
		{"USDC-" + testIssuer, huge, int64(3), at},
		{"GONE-" + testIssuer, "0", int64(0), at},
	}, nil)
	got, err := (&ExplorerReader{conn: conn}).ClassicCirculatingSupply(t.Context())
	if err != nil || len(got) != 1 || got["USDC-"+testIssuer] != huge {
		t.Fatalf("got %v, %v; want only USDC at %s", got, err, huge)
	}
	if len(conn.queries) != 1 || !strings.Contains(conn.queries[0], "FROM stellar.asset_holders_counts WHERE asset != 'native'") {
		t.Fatalf("want one rollup read and no scan, got %v", conn.queries)
	}
}

// Every reason the rollup cannot be trusted sends the read to the scan, never to a stale or partial map.
func TestClassicCirculatingSupply_ScansWhenRollupUnusable(t *testing.T) {
	fresh := time.Now().Add(-time.Minute).Truncate(time.Second)
	scan := [][]any{{"USDC-" + testIssuer, "500"}}
	for name, rows := range map[string][][]any{
		"stale cycle":           {{"USDC-" + testIssuer, "400", int64(2), time.Now().Add(-classicCirculatingRollupMaxAge - time.Minute)}},
		"cycle before column":   {{"USDC-" + testIssuer, "0", int64(2), fresh}},
		"empty table":           nil,
		"mixed cycle stamps":    {{"USDC-" + testIssuer, "400", int64(2), fresh}, {"EURC-" + testIssuer, "9", int64(1), fresh.Add(-time.Second)}},
		"unstamped (epoch) row": {{"USDC-" + testIssuer, "400", int64(2), time.Unix(0, 0)}},
	} {
		t.Run(name, func(t *testing.T) {
			conn := rollupSupplyConn(rows, scan)
			got, err := (&ExplorerReader{conn: conn}).ClassicCirculatingSupply(t.Context())
			if err != nil || len(got) != 1 || got["USDC-"+testIssuer] != "500" {
				t.Fatalf("got %v, %v; want the scan's 500", got, err)
			}
			if len(conn.queries) != 2 || !strings.Contains(conn.queries[1], "stellar.ledger_entries_current FINAL") {
				t.Fatalf("want the rollup read then the scan, got %v", conn.queries)
			}
		})
	}
}
