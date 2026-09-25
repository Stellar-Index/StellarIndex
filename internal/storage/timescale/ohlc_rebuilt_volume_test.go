// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The price CAGGs store Σ(base) as `volume` but no Σ(quote), so both OHLC
// readers rebuild the missing leg from vwap·volume. vwap is a rounded
// NUMERIC division, so the bare product is an integer sum plus a
// fractional residue (1e6/3e6 · 3e6 = 999999.99999999999999), served
// verbatim as a volume documented to be an integer smallest-unit sum.
// Every rebuilt leg must therefore be rounded back to the integer it is;
// test/integration's TestOHLCRebuiltVolumeIsTheIntegerSum executes it.
func TestOHLCRebuiltVolumeLegsAreRounded(t *testing.T) {
	pair := ohlcSourcesPair(t)
	bucket := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for name, run := range map[string]func(*Store) error{
		"OHLCSeries": func(s *Store) error {
			_, err := s.OHLCSeries(context.Background(), pair, Granularity1h, bucket, bucket.Add(time.Hour), 10)
			return err
		},
		"OHLCSeriesReBucketed": func(s *Store) error {
			_, err := s.OHLCSeriesReBucketed(context.Background(), pair, Granularity1h, "4 hours",
				bucket, bucket.Add(4*time.Hour), 10)
			return err
		},
	} {
		store, conn := newScriptedStore(t, scriptedResult{cols: ohlcSourcesCols})
		if err := run(store); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sql := conn.statements()[0]
		// One rebuilt leg per direction: the requested row's quote, the
		// flipped row's base.
		if n := strings.Count(sql, "vwap * volume"); n != 2 {
			t.Fatalf("%s rebuilds %d volume legs from vwap*volume, want 2:\n%s", name, n, sql)
		}
		if n := strings.Count(sql, "round(vwap * volume)"); n != 2 {
			t.Errorf("%s: %d of 2 rebuilt volume legs are rounded; an unrounded one serves "+
				"the division's residue as a fractional stroop sum:\n%s", name, n, sql)
		}
	}
}
