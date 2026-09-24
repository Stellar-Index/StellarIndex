// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// #782: a refresh that returns is not proof prices_1m matches `trades`.
// Nothing compared them, so a bucket left on the pre-repair rows served
// stale OHLC / volume with the rebuild reporting success. The refresh now
// fails on any drift, and the drifting minute may sit on either edge of
// the span — where a mis-padded refresh would leave it.
func TestRefreshTradesCAGGsOverLedgers_FailsWhenPrices1mDriftsFromTrades(t *testing.T) {
	from := time.Date(2025, 3, 10, 12, 0, 30, 0, time.UTC)
	to := time.Date(2025, 5, 14, 12, 7, 45, 0, time.UTC)
	for name, at := range map[string]time.Time{
		"first minute": from.Truncate(time.Minute),
		"last minute":  to.Truncate(time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTradesCAGGStore{from: from, to: to, driftAt: at}
			var out bytes.Buffer
			err := refreshTradesCAGGsOverLedgers(context.Background(), f, 61_000_000, 61_999_999, testCAGGNow, &out)
			if err == nil || !strings.Contains(err.Error(), "prices_1m disagrees with trades in 1 of 8 sampled window(s)") {
				t.Fatalf("err = %v, want the drift in 1 of 8 windows reported as a failure", err)
			}
			if !strings.Contains(err.Error(), "ledgers [61000000,61999999]") {
				t.Errorf("err = %v does not name the ledger range", err)
			}
			got := out.String()
			if !strings.Contains(got, "native/fiat:USD trades n=2 vol=31 usd=2  prices_1m n=1 vol=30 usd=1") {
				t.Errorf("stdout does not show the drifting pair's two sides:\n%s", got)
			}
			if strings.Contains(got, tradesCAGGRefreshedPrefix) {
				t.Errorf("printed the success line despite drift:\n%s", got)
			}
		})
	}
}

func TestRefreshTradesCAGGsOverLedgers_ReportsTheWindowsItChecked(t *testing.T) {
	from := time.Date(2025, 3, 10, 12, 0, 30, 0, time.UTC)
	f := &fakeTradesCAGGStore{from: from, to: from.Add(40 * 24 * time.Hour)}
	var out bytes.Buffer
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.compared) != tradesDriftSamples {
		t.Fatalf("compared %d window(s), want %d", len(f.compared), tradesDriftSamples)
	}
	if !strings.Contains(out.String(), "drift-windows=8\n") {
		t.Errorf("success line does not account for the windows checked: %q", out.String())
	}
}

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
