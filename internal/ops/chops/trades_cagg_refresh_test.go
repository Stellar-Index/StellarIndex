// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeTradesCAGGStore struct {
	from, to  time.Time
	spanErr   error
	failView  string
	refreshed []string
	windows   map[string][2]time.Time
}

func (f *fakeTradesCAGGStore) LedgerRangeToTimeRange(context.Context, uint32, uint32) (time.Time, time.Time, error) {
	return f.from, f.to, f.spanErr
}

func (f *fakeTradesCAGGStore) RefreshContinuousAggregate(_ context.Context, view string, from, to time.Time) error {
	if view == f.failView {
		return errors.New("canceling statement due to statement timeout")
	}
	f.refreshed = append(f.refreshed, view)
	if f.windows == nil {
		f.windows = map[string][2]time.Time{}
	}
	f.windows[view] = [2]time.Time{from, to}
	return nil
}

// bucketOf is each view's time_bucket width, read off its name; 1mo is
// the longest calendar month.
func bucketOf(t *testing.T, view string) time.Duration {
	t.Helper()
	switch view[strings.LastIndex(view, "_")+1:] {
	case "1m":
		return time.Minute
	case "15m":
		return 15 * time.Minute
	case "1h":
		return time.Hour
	case "4h":
		return 4 * time.Hour
	case "1d":
		return 24 * time.Hour
	case "1w":
		return 7 * 24 * time.Hour
	case "1mo":
		return 31 * 24 * time.Hour
	}
	t.Fatalf("no bucket width known for %q — add it here", view)
	return 0
}

// Every trades aggregate, in TradesCAGGs order (twap_* after prices_1m),
// and each window reaches a whole bucket past both ends of the span:
// Timescale refreshes only buckets wholly inside the window.
func TestRefreshTradesCAGGsOverLedgers_EveryViewInOrderCoveringTheEdgeBuckets(t *testing.T) {
	f := &fakeTradesCAGGStore{
		from: time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC),
		to:   time.Date(2025, 5, 14, 12, 0, 0, 0, time.UTC),
	}
	var out bytes.Buffer
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 61_000_000, 61_999_999, &out); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, c := range timescale.TradesCAGGs {
		want = append(want, c.Name)
	}
	if got := strings.Join(f.refreshed, ","); got != strings.Join(want, ",") {
		t.Fatalf("refreshed %s, want %s", got, strings.Join(want, ","))
	}
	for _, c := range timescale.TradesCAGGs {
		w, b := f.windows[c.Name], bucketOf(t, c.Name)
		if f.from.Sub(w[0]) < b || w[1].Sub(f.to) < b {
			t.Errorf("%s window [%s,%s] does not reach a %s bucket past the span [%s,%s]",
				c.Name, w[0], w[1], b, f.from, f.to)
		}
		if w[1].Sub(w[0]) < c.MinWindow {
			t.Errorf("%s window %s is below Timescale's minimum %s", c.Name, w[1].Sub(w[0]), c.MinWindow)
		}
	}
	if !strings.HasPrefix(out.String(), tradesCAGGRefreshedPrefix+" [61000000,61999999]") {
		t.Errorf("stdout = %q", out.String())
	}
}

// A view built on a failed one would re-materialise from stale input.
func TestRefreshTradesCAGGsOverLedgers_StopsAtTheFirstFailure(t *testing.T) {
	f := &fakeTradesCAGGStore{from: time.Unix(1_700_000_000, 0), to: time.Unix(1_700_100_000, 0), failView: "prices_1m"}
	var out bytes.Buffer
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, &out)
	if err == nil || !strings.Contains(err.Error(), "prices_1m") {
		t.Fatalf("err = %v, want the prices_1m failure", err)
	}
	if len(f.refreshed) != 0 || out.Len() != 0 {
		t.Errorf("carried on past the failure: refreshed %v, stdout %q", f.refreshed, out.String())
	}
}

func TestRefreshTradesCAGGsOverLedgers_EmptyRangeIsAnError(t *testing.T) {
	f := &fakeTradesCAGGStore{spanErr: timescale.ErrNotFound}
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no trades in ledgers [1,2]") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(f.refreshed) != 0 {
		t.Errorf("refreshed %v over an unknown span", f.refreshed)
	}
}
