package clickhouse

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// hashes builds n distinct 64-hex-ish tx hashes.
func hashes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%064x", i)
	}
	return out
}

// TestTxIndexes_ChunksAtTheBoundaryWithoutLosingHashes is the chunk-arithmetic
// contract. txIndexChunk bounds each IN-list so every chunk is a set of
// primary-key point lookups; the loop that slices `hashes` into those chunks
// is off-by-one territory, and an over-run panics while an under-run silently
// drops the tail — which for the MEV sandwich detector means transactions with
// no known intra-ledger order, i.e. quietly missing detections rather than an
// error anyone sees.
//
// The three interesting sizes are one below, exactly at, and one above the
// chunk boundary.
func TestTxIndexes_ChunksAtTheBoundaryWithoutLosingHashes(t *testing.T) {
	for _, n := range []int{0, 1, txIndexChunk - 1, txIndexChunk, txIndexChunk + 1, 2*txIndexChunk + 3} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			in := hashes(n)

			var asked [][]string
			conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
			r := &TxIndexReader{conn: conn}

			if _, err := r.TxIndexes(t.Context(), in); err != nil {
				t.Fatalf("TxIndexes: %v", err)
			}
			for _, a := range conn.args {
				if len(a) != 1 {
					t.Fatalf("chunk bound %d args, want 1 (the hash slice)", len(a))
				}
				chunk, ok := a[0].([]string)
				if !ok {
					t.Fatalf("chunk arg is %T, want []string", a[0])
				}
				asked = append(asked, chunk)
			}

			// Every chunk is within the bound …
			for i, c := range asked {
				if len(c) == 0 {
					t.Errorf("chunk %d is empty — an empty IN list is a wasted round trip", i)
				}
				if len(c) > txIndexChunk {
					t.Errorf("chunk %d holds %d hashes, exceeds txIndexChunk=%d", i, len(c), txIndexChunk)
				}
			}
			// … and the chunks concatenate back to the input, in order and
			// with nothing lost or repeated.
			var flat []string
			for _, c := range asked {
				flat = append(flat, c...)
			}
			if n == 0 {
				if len(asked) != 0 {
					t.Errorf("issued %d queries for an empty hash list, want 0", len(asked))
				}
				return
			}
			if !reflect.DeepEqual(flat, in) {
				t.Errorf("chunks concatenate to %d hashes, want the %d input hashes in order", len(flat), len(in))
			}
			wantChunks := (n + txIndexChunk - 1) / txIndexChunk
			if len(asked) != wantChunks {
				t.Errorf("issued %d chunks for %d hashes, want %d", len(asked), n, wantChunks)
			}
		})
	}
}

// TestTxIndexes_MissingHashesAreAbsentNotZero — the historical tx_hash_index
// backfill is windowed, so a hash the lake has not indexed yet legitimately
// returns no row. It must be ABSENT from the map: a zero-valued entry would
// tell the MEV detector the transaction was FIRST in its ledger, which is a
// specific and wrong claim about ordering, not a missing one.
func TestTxIndexes_MissingHashesAreAbsentNotZero(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		// Only "aa" is indexed.
		return &stubRows{data: [][]any{{"aa", uint32(7)}}}, nil
	}}
	r := &TxIndexReader{conn: conn}

	got, err := r.TxIndexes(t.Context(), []string{"aa", "bb"})
	if err != nil {
		t.Fatalf("TxIndexes: %v", err)
	}
	if got["aa"] != 7 {
		t.Errorf("index of aa = %d, want 7", got["aa"])
	}
	if _, present := got["bb"]; present {
		t.Errorf("unindexed hash bb is present as %d — callers must be able to tell "+
			"'not indexed' from 'index 0'", got["bb"])
	}
	if len(got) != 1 {
		t.Errorf("map holds %d entries, want 1", len(got))
	}
}

// TestTxIndexes_QueryShape pins the dedup and the key shape. max() collapses
// ReplacingMergeTree duplicates that have not merged (tx_index is identical
// across duplicates of one hash), and tx_hash is the table's primary key, so
// the IN-list is a set of point lookups rather than a scan.
func TestTxIndexes_QueryShape(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &TxIndexReader{conn: conn}
	if _, err := r.TxIndexes(t.Context(), []string{"aa"}); err != nil {
		t.Fatalf("TxIndexes: %v", err)
	}
	q := conn.queries[0]
	for _, s := range []string{
		"stellar.tx_hash_index",
		"WHERE tx_hash IN (?)",
		"max(tx_index)",
		"GROUP BY tx_hash",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("TxIndexes query missing %q:\n%s", s, q)
		}
	}
}

// TestTxIndexes_ChunkFailureAbortsWithoutAPartialMap — a partial map is worse
// than an error here: it is indistinguishable from "the rest were unindexed",
// so the detector would proceed on a silently truncated ordering.
func TestTxIndexes_ChunkFailureAbortsWithoutAPartialMap(t *testing.T) {
	boom := errors.New("no such table")
	calls := 0
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		calls++
		if calls == 1 {
			return &stubRows{data: [][]any{{"aa", uint32(1)}}}, nil
		}
		return nil, boom
	}}
	r := &TxIndexReader{conn: conn}

	got, err := r.TxIndexes(t.Context(), hashes(txIndexChunk+1))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if got != nil {
		t.Errorf("map = %v, want nil on error (a partial map reads as 'the rest are unindexed')", got)
	}
}

// TestTxIndexes_TruncatedStreamIsAnError.
func TestTxIndexes_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("stream truncated")
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{{"aa", uint32(1)}}, streamErr: truncated}, nil
	}}
	r := &TxIndexReader{conn: conn}
	if _, err := r.TxIndexes(t.Context(), []string{"aa", "bb"}); !errors.Is(err, truncated) {
		t.Fatalf("err = %v, want it to wrap %v", err, truncated)
	}
}
