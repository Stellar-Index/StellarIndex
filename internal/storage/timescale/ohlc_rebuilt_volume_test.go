// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Both OHLC readers take each bar's quote leg (and a flipped row's base
// leg) from the stored volume_quote (migration 0187). Rebuilding it as
// vwap·volume counts a zero-quote trade's base at the bucket price and
// drops a zero-base trade's quote; test/integration's
// TestOHLCVolumeLegsReadVolumeQuote executes it on such a bucket.
func TestOHLCVolumeLegsReadVolumeQuote(t *testing.T) {
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
		// One stored leg per direction: the requested row's quote, the
		// flipped row's base.
		if n := strings.Count(sql, "ELSE volume_quote END"); n != 1 {
			t.Errorf("%s: flipped base leg not read from volume_quote:\n%s", name, sql)
		}
		if n := strings.Count(sql, "THEN volume_quote ELSE"); n != 1 {
			t.Errorf("%s: requested quote leg not read from volume_quote:\n%s", name, sql)
		}
		if strings.Contains(sql, "vwap * volume") {
			t.Errorf("%s still rebuilds a volume leg from vwap*volume:\n%s", name, sql)
		}
	}
}
