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

// TestMaybeTolerateTrailingMissing_CountsOnlyTolerated pins that a
// tolerated miss is visible as a counter, and that an error passed
// through is not counted. Not parallel: the counter is process-global.
func TestMaybeTolerateTrailingMissing_CountsOnlyTolerated(t *testing.T) {
	missing := func(seq string) error {
		return errors.New("ledger object containing sequence " + seq + " is missing: file does not exist")
	}
	cases := []struct {
		name      string
		cfg       Config
		err       error
		wantNil   bool
		wantDelta float64
	}{
		{"tolerated trailing miss", Config{TolerateTrailingMissing: true, TrailingMissingWindow: 10}, missing("195"), true, 1},
		{"miss beyond window", Config{TolerateTrailingMissing: true, TrailingMissingWindow: 10}, missing("150"), false, 0},
		{"flag off", Config{}, missing("195"), false, 0},
		{"unrelated error", Config{TolerateTrailingMissing: true}, errors.New("boom"), false, 0},
	}
	for _, tc := range cases {
		before := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal)
		got := maybeTolerateTrailingMissing(tc.cfg, 100, 200, 5, tc.err)
		delta := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal) - before
		if (got == nil) != tc.wantNil {
			t.Errorf("%s: err = %v, wantNil %v", tc.name, got, tc.wantNil)
		}
		if delta != tc.wantDelta {
			t.Errorf("%s: tolerated counter delta = %v, want %v", tc.name, delta, tc.wantDelta)
		}
	}
}
