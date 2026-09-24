// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// memCursors mirrors ingestion_cursors: keyed (source, sub), monotone-forward
// on write like timescale.Store.UpsertCursor.
type memCursors map[[2]string]uint32

func (m memCursors) GetCursor(_ context.Context, source, sub string) (timescale.Cursor, error) {
	l, ok := m[[2]string{source, sub}]
	if !ok {
		return timescale.Cursor{}, timescale.ErrNotFound
	}
	return timescale.Cursor{Source: source, Sub: sub, LastLedger: l}, nil
}

func (m memCursors) upsert(source, sub string, ledger uint32) {
	k := [2]string{source, sub}
	if ledger > m[k] {
		m[k] = ledger
	}
}

// TestResumeRangeStartDoesNotSkipAnEarlierRepair: a completed repair of a
// later ledger gap must not make a subsequent repair of an EARLIER gap start
// past its own -to ("nothing to do", exit 0, window left NULL).
func TestResumeRangeStartDoesNotSkipAnEarlierRepair(t *testing.T) {
	for _, src := range []string{"tag-signer", "tag-routed-via"} {
		t.Run(src, func(t *testing.T) {
			ctx := context.Background()
			store := memCursors{}

			start, sub := resumeRangeStart(ctx, store, src, 58_000_000, 58_200_000, true)
			if start != 58_000_000 {
				t.Fatalf("first repair start = %d, want 58000000", start)
			}
			store.upsert(src, sub, 58_200_000)

			start, sub2 := resumeRangeStart(ctx, store, src, 56_100_000, 56_300_000, true)
			if start != 56_100_000 {
				t.Fatalf("earlier-gap repair start = %d, want 56100000 (the 58.0M-58.2M checkpoint leaked into it)", start)
			}
			if sub2 == sub {
				t.Fatalf("both ranges share checkpoint key %q", sub)
			}

			// An interrupted run of the SAME range still resumes.
			store.upsert(src, sub2, 56_150_000)
			if start, _ = resumeRangeStart(ctx, store, src, 56_100_000, 56_300_000, true); start != 56_150_001 {
				t.Fatalf("same-range resume start = %d, want 56150001", start)
			}
			if start, _ = resumeRangeStart(ctx, store, src, 56_100_000, 56_300_000, false); start != 56_100_000 {
				t.Fatalf("-resume=false start = %d, want 56100000", start)
			}
		})
	}
}
