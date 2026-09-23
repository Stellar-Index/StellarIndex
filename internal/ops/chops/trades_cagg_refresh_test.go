// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeTradesCAGGStore struct {
	from, to  time.Time
	spanErr   error
	failView  string
	armed     bool
	refreshed []string
	forced    map[string]bool
	windows   map[string][2]time.Time
}

func (f *fakeTradesCAGGStore) LedgerRangeToTimeRange(context.Context, uint32, uint32) (time.Time, time.Time, error) {
	return f.from, f.to, f.spanErr
}

func (f *fakeTradesCAGGStore) Prices1mRetentionArmed(context.Context) (bool, error) {
	return f.armed, nil
}

func (f *fakeTradesCAGGStore) RefreshContinuousAggregate(_ context.Context, view string, from, to time.Time) error {
	return f.refresh(view, from, to, false)
}

func (f *fakeTradesCAGGStore) RefreshContinuousAggregateForced(_ context.Context, view string, from, to time.Time) error {
	return f.refresh(view, from, to, true)
}

func (f *fakeTradesCAGGStore) refresh(view string, from, to time.Time, force bool) error {
	if view == f.failView {
		return errors.New("canceling statement due to statement timeout")
	}
	f.refreshed = append(f.refreshed, view)
	if f.windows == nil {
		f.windows, f.forced = map[string][2]time.Time{}, map[string]bool{}
	}
	f.windows[view] = [2]time.Time{from, to}
	f.forced[view] = force
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

// twap_1h / twap_1d read prices_1m, whose retention (migration 0156) drops
// minute rows without an invalidation. prices_1m must therefore be FORCED
// over a window containing every twap window, and the twaps forced after
// it, or a twap bucket is recomputed from dropped minute rows and its
// history deleted.
func TestRefreshTradesCAGGsOverLedgers_ForcesPrices1mOverEveryTwapWindow(t *testing.T) {
	f := &fakeTradesCAGGStore{
		from: time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC),
		to:   time.Date(2025, 3, 10, 12, 30, 0, 0, time.UTC),
	}
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 61_000_000, 61_000_100, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(timescale.CAGGsOnPrices1m) == 0 {
		t.Fatal("timescale.CAGGsOnPrices1m is empty")
	}
	minute := f.windows["prices_1m"]
	if !f.forced["prices_1m"] {
		t.Error("prices_1m was refreshed without force => true; a retention-dropped minute range stays empty")
	}
	for _, v := range timescale.CAGGsOnPrices1m {
		w, ok := f.windows[v]
		if !ok {
			t.Fatalf("%s was not refreshed", v)
		}
		if w[0].Before(minute[0]) || w[1].After(minute[1]) {
			t.Errorf("%s window [%s,%s] reaches outside the prices_1m force-rebuild [%s,%s]",
				v, w[0], w[1], minute[0], minute[1])
		}
		if !f.forced[v] {
			t.Errorf("%s was refreshed without force => true", v)
		}
	}
	for _, c := range timescale.TradesCAGGs {
		if c.Name != "prices_1m" && !slices.Contains(timescale.CAGGsOnPrices1m, c.Name) && f.forced[c.Name] {
			t.Errorf("%s reads trades, yet was forced", c.Name)
		}
	}
}

// While prices_1m's retention is armed it can drop the minute rows this
// run just rebuilt before a twap reads them: the twaps are refused, loudly,
// after every other view has been refreshed.
func TestRefreshTradesCAGGsOverLedgers_RefusesTwapsWhilePrices1mRetentionIsArmed(t *testing.T) {
	f := &fakeTradesCAGGStore{from: time.Unix(1_700_000_000, 0), to: time.Unix(1_700_100_000, 0), armed: true}
	var out bytes.Buffer
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, &out)
	if err == nil || !strings.Contains(err.Error(), "retention policy is armed") {
		t.Fatalf("err = %v, want a refusal naming the armed retention policy", err)
	}
	for _, v := range timescale.CAGGsOnPrices1m {
		if _, ok := f.windows[v]; ok {
			t.Errorf("%s was refreshed while prices_1m's retention is armed", v)
		}
	}
	if _, ok := f.windows["prices_1d"]; !ok {
		t.Error("prices_1d was not refreshed; only the prices_1m-derived views are refused")
	}
	if out.Len() != 0 {
		t.Errorf("printed a success line on refusal: %q", out.String())
	}
}
