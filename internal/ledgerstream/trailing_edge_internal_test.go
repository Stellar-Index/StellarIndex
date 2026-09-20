package ledgerstream

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestParseTrailingMissingSeq covers the SDK error-message parse —
// the only reliable identifier of "trailing-edge missing file"
// given the SDK uses pkg/errors.Wrapf with no typed sentinel
// (github.com/stellar/go-stellar-sdk/ingest/ledgerbackend.ledger_buffer).
func TestParseTrailingMissingSeq(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		err     error
		wantSeq uint32
		wantOK  bool
	}{
		{
			name:    "nil error",
			err:     nil,
			wantSeq: 0,
			wantOK:  false,
		},
		{
			name:    "unrelated error",
			err:     errors.New("postgres: connection refused"),
			wantSeq: 0,
			wantOK:  false,
		},
		{
			name: "bare SDK wrap from real r1 incident",
			err: errors.New(
				"ledger object containing sequence 62642880 is missing: " +
					"unable to retrieve file: " +
					"FC44EBFF--62592000-62655999/FC44253F--62642880.xdr.zst: " +
					"file does not exist"),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "wrapped through streamTiered prefix",
			err: errors.New("ledgerstream: get ledger 62642880: " +
				"ledger object containing sequence 62642880 is missing: ..."),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "wrapped through backfill chunk prefix",
			err: errors.New("backfill: chunk 11 [61639496, 62656054]: stream: " +
				"error getting ledger, failed getting next ledger batch from queue: " +
				"ledger object containing sequence 62642880 is missing: ..."),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "max retries variant (different SDK wrap — does NOT match)",
			err: errors.New(
				"maximum retries exceeded for downloading object containing sequence 12345"),
			wantSeq: 0,
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSeq, gotOK := parseTrailingMissingSeq(tc.err)
			if gotSeq != tc.wantSeq || gotOK != tc.wantOK {
				t.Errorf("parseTrailingMissingSeq() = (%d, %v), want (%d, %v)",
					gotSeq, gotOK, tc.wantSeq, tc.wantOK)
			}
		})
	}
}

// TestMaybeTolerateTrailingMissing_EmitsCounterWithScope pins RLT-140
// (a tolerated trailing-edge error must move an aggregatable counter,
// not only a log line — the sibling retry path already does this via
// obs.LedgerstreamLiveStartRetriesTotal) and Q058 (a tolerate event
// whose window covers the ENTIRE requested range — indistinguishable
// from swallowing a genuine mid-history hole — must be labelled
// differently from a true trailing-edge tolerate, so it is visible to
// an operator even when the caller has no coverage check of its own).
func TestMaybeTolerateTrailingMissing_EmitsCounterWithScope(t *testing.T) {
	missing8 := errors.New("ledger object containing sequence 8 is missing: unable to retrieve file: ...")

	t.Run("trailing_edge: window narrower than the requested range", func(t *testing.T) {
		before := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal.WithLabelValues("trailing_edge"))

		// Range [5,24] (width 19), window 16: seq 8 is within the window
		// of `to` (24-8=16<=16) AND the range is wider than the window
		// (19>=16), so a mid-history hole earlier than seq 24-16=8 would
		// still have errored — a true trailing-edge tolerate.
		err := maybeTolerateTrailingMissing(
			Config{TolerateTrailingMissing: true, TrailingMissingWindow: 16},
			5, 24, 3, missing8,
		)
		if err != nil {
			t.Fatalf("maybeTolerateTrailingMissing() = %v, want nil (tolerated)", err)
		}

		after := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal.WithLabelValues("trailing_edge"))
		if after != before+1 {
			t.Errorf("scope=trailing_edge counter = %v -> %v, want +1 (RLT-140: tolerate path must increment a counter, not only log)", before, after)
		}
	})

	t.Run("whole_range: window covers the entire requested range (Q058)", func(t *testing.T) {
		before := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal.WithLabelValues("whole_range"))

		// Range [5,9] (width 4), window 65536 default (TrailingMissingWindow
		// left zero): the whole range fits inside the window, so this
		// tolerate event cannot be told apart from a real interior hole.
		err := maybeTolerateTrailingMissing(
			Config{TolerateTrailingMissing: true},
			5, 9, 3, missing8,
		)
		if err != nil {
			t.Fatalf("maybeTolerateTrailingMissing() = %v, want nil (tolerated)", err)
		}

		after := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal.WithLabelValues("whole_range"))
		if after != before+1 {
			t.Errorf("scope=whole_range counter = %v -> %v, want +1 (Q058: a tolerate event indistinguishable from a mid-history hole must be labelled distinctly)", before, after)
		}
	})
}
