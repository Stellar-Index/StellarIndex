package band

import (
	"testing"
	"time"
)

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input for
// both entry points; a failure here means a ts-derivation change needs its
// own cleanup run (see "Re-deriving a timestamp" in
// docs/architecture/ingest-pipeline.md).
func TestDecodeRelayArgs_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sec := func(d time.Duration) uint64 { return uint64(closedAt.Add(d).Unix()) }

	type tc struct {
		resolve  uint64
		want     time.Time
		wantDrop bool
	}
	cases := map[string]map[string]tc{
		FnRelay: {
			"past resolve_time kept": {1_745_000_000, time.Unix(1_745_000_000, 0), false},
			"equal to close kept":    {sec(0), closedAt, false},
			"close+3599s kept":       {sec(time.Hour - time.Second), closedAt.Add(time.Hour - time.Second), false},
			"close+3600s dropped":    {sec(time.Hour), time.Time{}, true},
			"close+24h dropped":      {sec(24 * time.Hour), time.Time{}, true},
			"zero dropped":           {0, time.Time{}, true},
			"pre-2001 dropped":       {999_999_999, time.Time{}, true},
			"at 2001 floor kept":     {1_000_000_000, time.Unix(1_000_000_000, 0), false},
			"u64 max dropped":        {^uint64(0), time.Time{}, true},
		},
		FnForceRelay: {
			"past resolve_time kept":      {1_745_000_000, time.Unix(1_745_000_000, 0), false},
			"close+3599s kept":            {sec(time.Hour - time.Second), closedAt.Add(time.Hour - time.Second), false},
			"close+3600s clamps to close": {sec(time.Hour), closedAt, false},
			"close+24h clamps to close":   {sec(24 * time.Hour), closedAt, false},
			"zero clamps to close":        {0, closedAt, false},
			"pre-2001 clamps to close":    {999_999_999, closedAt, false},
			"at 2001 floor kept":          {1_000_000_000, time.Unix(1_000_000_000, 0), false},
			"u64 max clamps to close":     {^uint64(0), closedAt, false},
		},
	}

	for fn, group := range cases {
		for name, c := range group {
			t.Run(fn+"/"+name, func(t *testing.T) {
				rates := encodeSymbolRatesArg(t, []struct {
					Symbol string
					Rate   uint64
				}{{"BTC", 500_000_000_000_000}})
				var args []string
				if fn == FnRelay {
					args = []string{encodeAddressArg(t, relayerG), rates, encodeU64Arg(t, c.resolve), encodeU64Arg(t, 1)}
				} else {
					args = []string{rates, encodeU64Arg(t, c.resolve), encodeU64Arg(t, 1)}
				}
				got, err := decodeRelayArgs(fn, args, adapterC, 52_000_000, "abcd", 0, "", "", closedAt)
				if c.wantDrop {
					if err == nil && len(got) != 0 {
						t.Fatalf("expected the relay to be dropped, got %d rows (ts %s)", len(got), got[0].Timestamp)
					}
					return
				}
				if err != nil {
					t.Fatalf("decodeRelayArgs: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("got %d updates, want 1", len(got))
				}
				if !got[0].Timestamp.Equal(c.want) {
					t.Errorf("ts = %s, want %s", got[0].Timestamp, c.want)
				}
			})
		}
	}
}
