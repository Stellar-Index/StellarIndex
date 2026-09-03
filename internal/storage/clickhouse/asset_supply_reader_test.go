package clickhouse

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// classicSupplyConn returns an ExplorerReader whose single query answers with
// the given (asset, circulating) rows, plus the recorder so the SQL can be
// inspected.
func classicSupplyConn(t *testing.T, rows [][]any) (*ExplorerReader, *stubConn) {
	t.Helper()
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "stellar.ledger_entries_current") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{data: rows}, nil
	}}
	return &ExplorerReader{conn: conn}, conn
}

// TestClassicCirculatingSupply_PreservesAboveInt64Totals is the ADR-0003
// assertion for this reader. The per-asset total is a SUM over every trustline
// balance for a classic asset; the query sums as Int128 and stringifies
// (`toString(sum(toInt128(balance)))`) precisely because the running total can
// exceed int64, and the Go side must carry the result as a STRING end to end.
//
// The value used is 2^70 stroops — comfortably past int64's 9.22e18 ceiling —
// so any int64 hop anywhere on this path truncates it visibly rather than
// silently rounding a plausible number.
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

// TestClassicCirculatingSupply_OmitsUnusableRows — the reader drops rows whose
// asset or total is empty, and drops an exact "0". The zero case is the
// interesting one: a classic asset whose trustlines net to zero has NO
// circulating supply to report, and publishing "0" as a fact would feed a
// market-cap of exactly zero into /v1/assets rather than leaving the field
// absent (the caller's documented degrade path).
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

// TestClassicCirculatingSupply_QueryShape pins the population the total is
// summed over. Each predicate is a correctness predicate, not a filter of
// convenience:
//
//   - entry_type = 'trustline': circulating supply for a classic asset IS the
//     amount held by non-issuer accounts, which is exactly the trustline set.
//     Admitting account entries would fold XLM balances into every asset.
//   - change_type != 'removed': a deleted trustline holds nothing; counting it
//     resurrects the balance it had when it was closed.
//   - balance > 0: a zero/negative row contributes nothing and only costs scan.
//   - GROUP BY asset: the per-asset key. Losing it collapses every asset into
//     one total.
//   - toInt128 + toString: the ADR-0003 no-truncation contract, in the SQL.
func TestClassicCirculatingSupply_QueryShape(t *testing.T) {
	r, conn := classicSupplyConn(t, nil)
	if _, err := r.ClassicCirculatingSupply(t.Context()); err != nil {
		t.Fatalf("ClassicCirculatingSupply: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want exactly 1 (this read is far too heavy to fan out)", len(conn.queries))
	}
	q := conn.queries[0]
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
	if got := len(conn.args[0]); got != 0 {
		t.Errorf("ClassicCirculatingSupply bound %d args, want 0", got)
	}
}

// TestClassicCirculatingSupply_QueryErrorPropagates — the caller
// (Server.refreshClassicSupply) keeps serving its last-good map ONLY because
// this returns an error; a nil-error empty map would be stored as the new
// truth and blank every classic market cap.
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

// TestClassicCirculatingSupply_TruncatedStreamIsAnError — a scan that dies
// part-way (this one legitimately reaches ~1.7 GB, see the deliverable) would
// otherwise hand back a PARTIAL map that the caller stores as complete,
// zeroing the market cap of every asset the scan never reached.
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
