// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// bulkSupplyConn returns a SupplyReader whose single query answers with the
// given (contract_id, mint, burn, clawback, flows) rows, plus the recorder so
// the SQL and its bound args can be inspected.
func bulkSupplyConn(t *testing.T, rows [][]any) (*SupplyReader, *stubConn) {
	t.Helper()
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "stellar.supply_flows") {
			t.Fatalf("unexpected query: %s", q)
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
	if n := len(conn.queries); n != 1 {
		t.Errorf("issued %d queries for 2 contracts, want 1 (the bulk read is the point): %v", n, conn.queries)
	}
	if len(conn.args) != 1 || len(conn.args[0]) != 1 {
		t.Fatalf("want exactly one bound arg (the IN-list), got %v", conn.args)
	}
	ids, ok := conn.args[0][0].([]string)
	if !ok || len(ids) != 2 {
		t.Fatalf("IN-list must bind the contract ids as a slice, got %#v", conn.args[0][0])
	}
}

// TestTokenSupplyForContracts_MissingContractIsAbsentNotZero is the
// distinction the caller's fallback logic rests on. A contract the lake has no
// flows for must be ABSENT from the map, never present with a zero total: zero
// is the claim "this token is fully burned", and publishing it for a token the
// lake simply hasn't seen would understate it to nothing.
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

// TestTokenSupplyForContracts_EmptyInputIssuesNoQuery — an empty candidate set
// must not reach ClickHouse at all. `WHERE contract_id IN ()` is both a
// syntax error and a pointless round trip on a hot path that will often have
// nothing to ask about.
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

// TestTokenSupplyForContracts_QueryShape pins the population the totals are
// summed over. FINAL is load-bearing (the ReplacingMergeTree double-counts
// re-ingested events without it), the IN-list is what keeps the scan to the
// requested contracts' primary-key ranges, and Int256 accumulation is ADR-0003.
func TestTokenSupplyForContracts_QueryShape(t *testing.T) {
	r, conn := bulkSupplyConn(t, nil)
	if _, err := r.TokenSupplyForContracts(t.Context(), []string{"CAAA"}); err != nil {
		t.Fatalf("TokenSupplyForContracts: %v", err)
	}
	q := conn.queries[0]
	for _, s := range []string{
		"stellar.supply_flows FINAL",
		"WHERE contract_id IN (?)",
		"GROUP BY contract_id",
		"toInt256",
		"toString(",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("bulk supply query missing %q:\n%s", s, q)
		}
	}
}
