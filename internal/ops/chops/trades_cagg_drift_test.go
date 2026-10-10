// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"
	"time"
)

func TestTradesDriftSampleWindows(t *testing.T) {
	minute := func(d, h, m int) time.Time { return time.Date(2025, 3, d, h, m, 0, 0, time.UTC) }
	far := minute(28, 0, 0)

	t.Run("long span: n windows from the first minute to the last", func(t *testing.T) {
		from, to := minute(1, 0, 0).Add(30*time.Second), minute(21, 5, 7).Add(45*time.Second)
		wins := tradesDriftSampleWindows(from, to, far)
		if len(wins) != 8 {
			t.Fatalf("%d windows, want 8", len(wins))
		}
		if !wins[0][0].Equal(minute(1, 0, 0)) {
			t.Errorf("first window starts %s, want the span's first minute %s", wins[0][0], minute(1, 0, 0))
		}
		if !wins[7][1].Equal(minute(21, 5, 8)) {
			t.Errorf("last window ends %s, want just past the span's last minute %s", wins[7][1], minute(21, 5, 8))
		}
		for i, w := range wins {
			if w[1].Sub(w[0]) != time.Hour || !w[0].Equal(w[0].Truncate(time.Minute)) {
				t.Errorf("window %d = %v, want one whole-minute hour", i, w)
			}
			if i > 0 && !w[0].After(wins[i-1][0]) {
				t.Errorf("window %d does not advance: %v after %v", i, w, wins[i-1])
			}
		}
	})

	t.Run("short span: compared whole", func(t *testing.T) {
		wins := tradesDriftSampleWindows(minute(1, 0, 3).Add(time.Second), minute(1, 2, 9), far)
		if len(wins) != 1 || !wins[0][0].Equal(minute(1, 0, 3)) || !wins[0][1].Equal(minute(1, 2, 10)) {
			t.Fatalf("windows = %v, want the one span [00:03,02:10)", wins)
		}
	})

	t.Run("minutes at or after the cutoff are left out", func(t *testing.T) {
		wins := tradesDriftSampleWindows(minute(1, 0, 0), minute(1, 2, 0), minute(1, 1, 30).Add(10*time.Second))
		if len(wins) != 1 || !wins[0][1].Equal(minute(1, 1, 30)) {
			t.Fatalf("windows = %v, want one ending at the cutoff 01:30", wins)
		}
		if got := tradesDriftSampleWindows(minute(1, 0, 0), minute(1, 2, 0), minute(1, 0, 0)); len(got) != 0 {
			t.Errorf("windows = %v for a span wholly past the cutoff, want none", got)
		}
	})
}
