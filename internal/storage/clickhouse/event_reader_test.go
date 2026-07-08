// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestContractEventsFilteredQuery_OpArgsTrim pins the 2026-07-08 OOM fix's
// column trim: the WIDE op_args_xdr column is read only when the consuming
// decoder actually uses events.Event.OpArgs (redstone). The sep41 reconcile —
// millions of CAP-67 firehose rows — must never pull it.
func TestContractEventsFilteredQuery_OpArgsTrim(t *testing.T) {
	without := contractEventsFilteredQuery(nil, nil, nil, false, false)
	if strings.Contains(without, "op_args_xdr") {
		t.Errorf("withOpArgs=false query must not read op_args_xdr:\n%s", without)
	}
	with := contractEventsFilteredQuery(nil, nil, nil, false, true)
	if !strings.Contains(with, "op_args_xdr") {
		t.Errorf("withOpArgs=true query must read op_args_xdr:\n%s", with)
	}
	// The needed decode columns are present either way.
	for _, col := range []string{"topics_xdr", "data_xdr", "in_successful_call"} {
		if !strings.Contains(without, col) {
			t.Errorf("withOpArgs=false query lost required column %s", col)
		}
	}
}

// TestContractEventsFilteredQuery_BoundedSettings pins the per-query memory
// bounds (the connection-level class alone did not stop the server-wide
// OvercommitTracker kills): low max_threads + tracked ceiling + external
// spill must ride ON the query text.
func TestContractEventsFilteredQuery_BoundedSettings(t *testing.T) {
	q := contractEventsFilteredQuery(nil, nil, nil, true, false)
	for _, s := range []string{
		"SETTINGS",
		"max_threads = 2",
		"max_memory_usage = 8589934592",
		"max_bytes_before_external_sort = 4294967296",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("query missing bounded-scan setting %q:\n%s", s, q)
		}
	}
}

// TestContractEventsFilteredQuery_FinalAndPrefilters — FINAL toggling and the
// three prefilter clauses render as before.
func TestContractEventsFilteredQuery_FinalAndPrefilters(t *testing.T) {
	q := contractEventsFilteredQuery(
		[]string{"CAAA"}, []string{"transfer"}, []string{"mint", "burn"}, true, true)
	for _, s := range []string{
		"FROM stellar.contract_events FINAL",
		"contract_id IN ('CAAA')",
		"topic_0_sym IN ('transfer')",
		"topic_0_sym NOT IN ('mint','burn')",
		"ORDER BY ledger_seq, tx_hash, op_index, event_index",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("query missing %q:\n%s", s, q)
		}
	}
	if noFinal := contractEventsFilteredQuery(nil, nil, nil, false, true); strings.Contains(noFinal, "FINAL") {
		t.Errorf("useFinal=false query must not carry FINAL:\n%s", noFinal)
	}
}

// TestForEachLedgerWindow — the windows cover [from,to] exactly (contiguous,
// no overlap, no spill past to) and align to multiple-of-stride boundaries so
// each query touches the minimum number of 1M-ledger lake partitions.
func TestForEachLedgerWindow(t *testing.T) {
	type win struct{ lo, hi uint32 }
	collect := func(from, to, stride uint32) []win {
		var got []win
		if err := forEachLedgerWindow(from, to, stride, func(lo, hi uint32) error {
			got = append(got, win{lo, hi})
			return nil
		}); err != nil {
			t.Fatalf("forEachLedgerWindow(%d,%d,%d): %v", from, to, stride, err)
		}
		return got
	}

	tests := []struct {
		name             string
		from, to, stride uint32
		want             []win
	}{
		{"empty range", 100, 99, 10, nil},
		{"zero stride is a no-op", 1, 100, 0, nil},
		{"single ledger", 42, 42, 10, []win{{42, 42}}},
		{"aligned to stride boundaries", 5, 25, 10, []win{{5, 9}, {10, 19}, {20, 25}}},
		{"from on a boundary", 10, 29, 10, []win{{10, 19}, {20, 29}}},
		{"to on a boundary end", 5, 19, 10, []win{{5, 9}, {10, 19}}},
		{"whole range inside one window", 12, 15, 1000, []win{{12, 15}}},
		{
			"partition-aligned like the recognition scan",
			50_457_424, 52_100_000, 1_000_000,
			[]win{{50_457_424, 50_999_999}, {51_000_000, 51_999_999}, {52_000_000, 52_100_000}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collect(tt.from, tt.to, tt.stride)
			if len(got) != len(tt.want) {
				t.Fatalf("windows = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("windows = %v, want %v", got, tt.want)
				}
			}
			// Invariants: contiguous cover of [from,to].
			for i, w := range got {
				if w.hi < w.lo {
					t.Errorf("window %d inverted: %v", i, w)
				}
				if i > 0 && w.lo != got[i-1].hi+1 {
					t.Errorf("window %d not contiguous with previous: %v after %v", i, w, got[i-1])
				}
			}
			if len(got) > 0 {
				if got[0].lo != tt.from || got[len(got)-1].hi != tt.to {
					t.Errorf("cover = [%d,%d], want [%d,%d]", got[0].lo, got[len(got)-1].hi, tt.from, tt.to)
				}
			}
		})
	}
}

// TestSQLQuoteEscaped — lake-sourced strings are escaped before being inlined
// into the exemplar query text.
func TestSQLQuoteEscaped(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
		{`both\'`, `'both\\\''`},
	}
	for _, tt := range tests {
		if got := sqlQuoteEscaped(tt.in); got != tt.want {
			t.Errorf("sqlQuoteEscaped(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
