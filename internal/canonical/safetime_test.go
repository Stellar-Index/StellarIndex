// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"math"
	"testing"
	"time"
)

// Ported from the redstone pickTimestamp suite when the guard was
// extracted here (D3 cluster 9) — these pin the overflow classes the
// three oracle decoders depend on.

func TestSafeUnixMillis_zeroFallsBackToClosedAt(t *testing.T) {
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	if got := SafeUnixMillis(0, closedAt); !got.Equal(closedAt) {
		t.Errorf("got %v, want %v (closedAt fallback)", got, closedAt)
	}
}

func TestSafeUnixMillis_inRangeHonoured(t *testing.T) {
	// Non-zero sane values must be honoured even when closedAt is
	// wildly later.
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rawMs := uint64(1_700_000_000_000) // 2023-11-14T22:13:20Z
	got := SafeUnixMillis(rawMs, closedAt)
	want := time.UnixMilli(1_700_000_000_000).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSafeUnixMillis_farFutureClampsToClosedAt(t *testing.T) {
	// A sentinel / garbage far-future value (same overflow class as
	// the soroswap-router deadline_ts) must fall back to the ledger
	// close time instead of overflowing the timestamptz INSERT.
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rawMs := uint64(3_000_000_000_000_000) // ~year 96000
	if got := SafeUnixMillis(rawMs, closedAt); !got.Equal(closedAt) {
		t.Errorf("got %v, want %v (far-future must clamp to closedAt)", got, closedAt)
	}
}

func TestSafeUnixMillis_overflowWrapsClampToClosedAt(t *testing.T) {
	// Values ABOVE math.MaxInt64 wrap NEGATIVE in the int64 cast → a
	// far-PAST time that a cast-first future-only After() guard
	// misses and that overflows the timestamptz INSERT. Must clamp.
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	for name, raw := range map[string]uint64{
		"justOverMaxInt64": uint64(math.MaxInt64) + 1,
		"1e19wrapsFarPast": 10_000_000_000_000_000_000,
		"maxUint64":        math.MaxUint64,
	} {
		t.Run(name, func(t *testing.T) {
			if got := SafeUnixMillis(raw, closedAt); !got.Equal(closedAt) {
				t.Errorf("got %v, want %v (>MaxInt64 must clamp, not wrap)", got, closedAt)
			}
		})
	}
}

func TestSafeUnixMillis_preEpochFloorClampsToClosedAt(t *testing.T) {
	// Pre-2001 values (e.g. a mis-scaled seconds value landing in the
	// millis slot) are garbage for every source we ingest.
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rawMs := uint64(999_999_999_999) // 2001-09-09T01:46:39.999Z
	if got := SafeUnixMillis(rawMs, closedAt); !got.Equal(closedAt) {
		t.Errorf("got %v, want %v (pre-epoch-floor must clamp)", got, closedAt)
	}
}

func TestSafeUnixMillis_withinSkewWindowHonoured(t *testing.T) {
	// Slightly after the ledger close (clock skew) but within the
	// sanity window is still honoured.
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rawMs := uint64(closedAt.Add(time.Hour).UnixMilli())
	got := SafeUnixMillis(rawMs, closedAt)
	want := time.UnixMilli(int64(rawMs)).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (in-window value honoured)", got, want)
	}
}

func TestSafeUnixMillis_resultIsUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no timezone data")
	}
	got := SafeUnixMillis(0, time.Date(2026, 4, 26, 12, 0, 0, 0, loc))
	if got.Location() != time.UTC {
		t.Errorf("Location() = %v, want UTC", got.Location())
	}
}

func TestSafeUnixSeconds_bounds(t *testing.T) {
	closedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		raw  uint64
		want time.Time
	}{
		"zero":           {0, closedAt},
		"preEpochFloor":  {999_999_999, closedAt},
		"epochFloor":     {1_000_000_000, time.Unix(1_000_000_000, 0).UTC()},
		"sane2023":       {1_700_000_000, time.Unix(1_700_000_000, 0).UTC()},
		"atCloseSkewMax": {uint64(closedAt.Add(SafeUnixFutureWindow).Unix()), closedAt.Add(SafeUnixFutureWindow)},
		"overSkewMax":    {uint64(closedAt.Add(SafeUnixFutureWindow).Unix()) + 1, closedAt},
		"maxUint64":      {math.MaxUint64, closedAt},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SafeUnixSeconds(tc.raw, closedAt); !got.Equal(tc.want) {
				t.Errorf("SafeUnixSeconds(%d) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
