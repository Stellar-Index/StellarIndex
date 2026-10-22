package clickhouse

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestIsSupplyFlowSym(t *testing.T) {
	for _, s := range []string{"mint", "burn", "clawback"} {
		if !IsSupplyFlowSym(s) {
			t.Errorf("IsSupplyFlowSym(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"transfer", "approve", "set_admin", "set_authorized", "swap", ""} {
		if IsSupplyFlowSym(s) {
			t.Errorf("IsSupplyFlowSym(%q) = true, want false", s)
		}
	}
}

func TestMustBig(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"123", "123"},
		{"-45", "-45"},
		{"", "0"},
		{"not-a-number", "0"},
		{"170141183460469231731687303715884105727", "170141183460469231731687303715884105727"}, // i128 max
	}
	for _, tt := range tests {
		if got := mustBig(tt.in).String(); got != tt.want {
			t.Errorf("mustBig(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// TestAssembleTokenSupply_NegativeMarkedIncomplete pins that a negative lake-flows net total (burns above
// mints) is flagged Incomplete, so the serving consumers refuse it rather than publish a negative (ADR-0003).
func TestAssembleTokenSupply_NegativeMarkedIncomplete(t *testing.T) {
	// burn alone exceeds mint → negative net total.
	if ts := assembleTokenSupply("C_BURN", "100", "150", "0", 3); ts.Total.String() != "-50" || !ts.Incomplete {
		t.Errorf("mint=100 burn=150: Total=%s Incomplete=%v, want -50/true", ts.Total, ts.Incomplete)
	}
	// clawback alone pushes it negative → exercised independently.
	if ts := assembleTokenSupply("C_CLAW", "100", "0", "150", 3); ts.Total.String() != "-50" || !ts.Incomplete {
		t.Errorf("mint=100 clawback=150: Total=%s Incomplete=%v, want -50/true", ts.Total, ts.Incomplete)
	}
	// A normal token (mint >= burn+clawback) is NOT flagged and Total is
	// byte-identical to the pre-guard behaviour.
	if ts := assembleTokenSupply("C_OK", "1000", "80", "20", 7); ts.Total.String() != "900" || ts.Incomplete {
		t.Errorf("normal token: Total=%s Incomplete=%v, want 900/false", ts.Total, ts.Incomplete)
	}
	// Exact zero (fully burned) is a legitimate non-negative reading — the
	// guard must not over-reach into the real zero case.
	if ts := assembleTokenSupply("C_ZERO", "100", "100", "0", 2); ts.Total.Sign() != 0 || ts.Incomplete {
		t.Errorf("fully-burned token: Total=%s Incomplete=%v, want 0/false", ts.Total, ts.Incomplete)
	}
}

func TestDecodeSupplyAmount(t *testing.T) {
	// Bare i128, positive value 1_000_000 (Hi=0, Lo=value).
	i128 := xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: 1_000_000}}
	amt, _, ok := DecodeSupplyAmount(i128)
	if !ok {
		t.Fatal("DecodeSupplyAmount(i128) ok=false, want true")
	}
	if amt.String() != "1000000" {
		t.Errorf("DecodeSupplyAmount(i128) = %s, want 1000000", amt.String())
	}

	// A non-amount type (bool) is undecodable → ok=false, not a panic.
	b := xdr.ScVal{Type: xdr.ScValTypeScvBool, B: new(bool)}
	if _, reason, ok := DecodeSupplyAmount(b); ok {
		t.Errorf("DecodeSupplyAmount(bool) ok=true, want false (reason=%q)", reason)
	}
}

// bulkSupplyConn returns a SupplyReader whose single query answers with the
// given (contract_id, mint, burn, clawback, flows) rows, plus the recorder so
// the SQL and its bound args can be inspected.
func bulkSupplyConn(t *testing.T, rows [][]any) (*SupplyReader, *stubConn) {
	t.Helper()
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "stellar.supply_flows") {
			t.Fatalf("unexpected query: %s", q)
		}
		switch {
		case strings.Contains(q, "max(ledger_seq)"):
			return &stubRows{data: [][]any{{uint32(0)}}}, nil
		case strings.Contains(q, ">= ?"):
			return &stubRows{}, nil
		}
		return &stubRows{data: rows}, nil
	}}
	return &SupplyReader{conn: conn}, conn
}

// TestTokenSupplyForContracts_SumsPerContract is the basic contract: one read,
// one entry per contract, net = mint − (burn + clawback).
func TestTokenSupplyForContracts_SumsPerContract(t *testing.T) {
	r, conn := bulkSupplyConn(t, [][]any{
		{"CAAA", "1000", "100", "0", uint64(9)},
		{"CBBB", "500", "0", "50", uint64(3)},
	})

	got, err := r.TokenSupplyForContracts(t.Context(), []string{"CBBB", "CAAA"})
	if err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if v := got["CAAA"].Total.String(); v != "900" {
		t.Errorf("CAAA total = %s, want 900 (1000 mint − 100 burn)", v)
	}
	if v := got["CBBB"].Total.String(); v != "450" {
		t.Errorf("CBBB total = %s, want 450 (500 mint − 50 clawback)", v)
	}
	if got["CAAA"].Incomplete || got["CBBB"].Incomplete {
		t.Errorf("neither positive net total is incomplete: %+v", got)
	}
	if n := len(conn.queries); n != 3 {
		t.Errorf("issued %d queries for 2 contracts, want 3 (settled ledger, base, recent; the bulk read is the point): %v", n, conn.queries)
	}
	if len(conn.args) != 3 || len(conn.args[1]) != 2 {
		t.Fatalf("want the base read to bind the IN-list and the settled ledger, got %v", conn.args)
	}
	ids, ok := conn.args[1][0].([]string)
	if !ok || len(ids) != 2 {
		t.Fatalf("IN-list must bind the contract ids as a slice, got %#v", conn.args[0][0])
	}
}

// TestTokenSupplyForContracts_MissingContractIsAbsentNotZero pins that a contract with no flows is absent
// from the map, never zero: zero claims the token is fully burned and would understate an unseen token to
// nothing.
func TestTokenSupplyForContracts_MissingContractIsAbsentNotZero(t *testing.T) {
	r, _ := bulkSupplyConn(t, [][]any{
		{"CAAA", "1000", "0", "0", uint64(1)},
	})

	got, err := r.TokenSupplyForContracts(t.Context(), []string{"CAAA", "CNOFLOWS"})
	if err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	if _, present := got["CNOFLOWS"]; present {
		t.Errorf("a contract with no flows must be ABSENT, not a zero reading; got %+v", got["CNOFLOWS"])
	}
}

// TestTokenSupplyForContracts_NegativeNetIsIncomplete — a net below zero is
// incomplete seeding, not negative supply, and the flag must survive the bulk
// path exactly as it does the single-contract one.
func TestTokenSupplyForContracts_NegativeNetIsIncomplete(t *testing.T) {
	r, _ := bulkSupplyConn(t, [][]any{
		{"CUNDER", "10", "99", "0", uint64(4)},
	})

	got, err := r.TokenSupplyForContracts(t.Context(), []string{"CUNDER"})
	if err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	if !got["CUNDER"].Incomplete {
		t.Errorf("Σ(burn+clawback) > Σmint must mark Incomplete; got %+v", got["CUNDER"])
	}
}

// TestTokenSupplyForContracts_PreservesAboveInt64Totals is the ADR-0003
// assertion for the bulk path: the per-contract total is summed as Int256 and
// carried as a STRING, so a supply past int64's ceiling survives intact.
func TestTokenSupplyForContracts_PreservesAboveInt64Totals(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 70).String()
	if len(huge) <= len("9223372036854775807") {
		t.Fatalf("test premise broken: %s is not wider than int64", huge)
	}
	r, _ := bulkSupplyConn(t, [][]any{{"CBIG", huge, "0", "0", uint64(1)}})

	got, err := r.TokenSupplyForContracts(t.Context(), []string{"CBIG"})
	if err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	if v := got["CBIG"].Total.String(); v != huge {
		t.Errorf("CBIG total = %q, want %q (ADR-0003: an i128+ total must not be truncated)", v, huge)
	}
}

// TestTokenSupplyForContracts_EmptyInputIssuesNoQuery: an empty candidate set must not reach ClickHouse,
// since `WHERE contract_id IN ()` is a syntax error and a wasted round trip on a hot path.
func TestTokenSupplyForContracts_EmptyInputIssuesNoQuery(t *testing.T) {
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		t.Fatalf("empty input must issue no query; got: %s", q)
		return nil, nil
	}}
	r := &SupplyReader{conn: conn}

	got, err := r.TokenSupplyForContracts(t.Context(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("TokenSupplyForContracts(nil) = (%v, %v), want (empty, nil)", got, err)
	}
}

// TestTokenSupplyForContracts_QueryErrorPropagates — the caller decides how to
// degrade; the reader must not hide a failed read behind an empty map, which
// would be indistinguishable from "the lake knows nothing about these tokens".
func TestTokenSupplyForContracts_QueryErrorPropagates(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		return nil, errors.New("clickhouse down")
	}}
	r := &SupplyReader{conn: conn}

	got, err := r.TokenSupplyForContracts(t.Context(), []string{"CAAA"})
	if err == nil {
		t.Fatalf("TokenSupplyForContracts hid a query failure; got map %v", got)
	}
}

// TestTokenSupplyForContracts_QueryShape pins the summed population: FINAL is load-bearing
// (ReplacingMergeTree double-counts re-ingested events), the IN-list keeps the scan to the contracts' key
// ranges, and Int256 accumulation is ADR-0003.
func TestTokenSupplyForContracts_QueryShape(t *testing.T) {
	r, conn := bulkSupplyConn(t, nil)
	if _, err := r.TokenSupplyForContracts(t.Context(), []string{"CAAA"}); err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	q := conn.queries[1]
	for _, s := range []string{
		"stellar.supply_flows FINAL",
		"WHERE contract_id IN (?) AND ledger_seq < ?",
		"GROUP BY contract_id",
		"toInt256",
		"toString(",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("bulk supply query missing %q:\n%s", s, q)
		}
	}
}

// TestTokenSupplyForContracts_RereadsOnlyAboveSettledBase pins the memo: a refresh sums only flows at or above the settled ledger the
// base stopped below, adds them to the memoised base exactly (above int64), and the base expires after supplyBaseMaxAge.
func TestTokenSupplyForContracts_RereadsOnlyAboveSettledBase(t *testing.T) {
	big1 := "170141183460469231731687303715884105000"
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "max(ledger_seq)"):
			return &stubRows{data: [][]any{{uint32(64_900_000)}}}, nil
		case strings.Contains(q, "ledger_seq < ?"):
			return &stubRows{data: [][]any{{"CBIG", big1, "0", "0", uint64(10)}}}, nil
		}
		return &stubRows{data: [][]any{
			{"CBIG", "700", "0", "0", uint64(1)},
			{"CNEW", "5", "0", "0", uint64(1)},
		}}, nil
	}}
	clock := time.Unix(1_700_000_000, 0)
	r := &SupplyReader{conn: conn, now: func() time.Time { return clock }}
	ids := []string{"CBIG", "CNEW", "CNONE"}
	want, _ := new(big.Int).SetString(big1, 10)
	want.Add(want, big.NewInt(700))
	check := func(label string, got map[string]TokenSupply) {
		t.Helper()
		if s := got["CBIG"]; s.Total == nil || s.Total.Cmp(want) != 0 || s.FlowCount != 11 {
			t.Errorf("%s: CBIG = %v/%d flows, want base+recent %s/11", label, s.Total, s.FlowCount, want)
		}
		if s := got["CNEW"]; s.Total == nil || s.Total.String() != "5" {
			t.Errorf("%s: CNEW's only flow is above the base; want total 5, got %+v", label, s)
		}
		if _, ok := got["CNONE"]; ok {
			t.Errorf("%s: CNONE has no flows; it must be absent", label)
		}
	}

	first, err := r.TokenSupplyForContracts(t.Context(), ids)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	check("cold", first)

	clock = clock.Add(supplyBaseMaxAge - time.Second)
	before := len(conn.queries)
	second, err := r.TokenSupplyForContracts(t.Context(), ids)
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	check("warm", second)
	if n := len(conn.queries) - before; n != 1 || !strings.Contains(conn.queries[before], "ledger_seq >= ?") {
		t.Fatalf("warm read must be one recent-flows query, got %d: %v", n, conn.queries[before:])
	}
	if b := conn.args[before][1]; b != uint32(64_900_000) {
		t.Errorf("recent-flows bound %v, want the settled ledger 64900000", b)
	}

	clock = clock.Add(time.Second)
	before = len(conn.queries)
	if _, err := r.TokenSupplyForContracts(t.Context(), ids); err != nil {
		t.Fatalf("expired read: %v", err)
	}
	if n := len(conn.queries) - before; n != 3 {
		t.Errorf("an expired base must be re-read in full (3 queries), got %d", n)
	}
}
