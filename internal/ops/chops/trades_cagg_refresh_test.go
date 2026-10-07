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
	from, to time.Time
	spanErr  error
	failView string
	armed    bool
	// earliest is prices_1m's earliest bucket; zero puts no floor in the way.
	earliest    time.Time
	earliestErr error
	refreshed   []string
	forced      map[string]bool
	windows     map[string][2]time.Time
	// driftAt, when set, is a minute where prices_1m disagrees with trades.
	driftAt  time.Time
	compared [][2]time.Time
	fakeInvalidated
}

// testCAGGNow is well after every span these tests refresh.
var testCAGGNow = time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

func (f *fakeTradesCAGGStore) TradesPrices1mDrift(_ context.Context, from, to time.Time) ([]timescale.TradesPrices1mDrift, error) {
	f.compared = append(f.compared, [2]time.Time{from, to})
	return fakeDrift(f.driftAt, from, to), nil
}

func fakeDrift(at, from, to time.Time) []timescale.TradesPrices1mDrift {
	if at.IsZero() || at.Before(from) || !at.Before(to) {
		return nil
	}
	return []timescale.TradesPrices1mDrift{{
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		TradeCount: "2", CAGGCount: "1",
		TradeVolume: "31", CAGGVolume: "30",
		TradeUSD: "2", CAGGUSD: "1",
	}}
}

// fakeInvalidated is each view's pending invalidation ranges.
type fakeInvalidated map[string][][2]time.Time

func (f fakeInvalidated) CAGGInvalidatedRanges(_ context.Context, view string, from, to time.Time) ([][2]time.Time, error) {
	return clipRanges(f[view], [2]time.Time{from, to}), nil
}

func (f *fakeTradesCAGGStore) LedgerRangeToTimeRange(context.Context, uint32, uint32) (time.Time, time.Time, error) {
	return f.from, f.to, f.spanErr
}

func (f *fakeTradesCAGGStore) Prices1mRetentionArmed(context.Context) (bool, error) {
	return f.armed, nil
}

func (f *fakeTradesCAGGStore) Prices1mEarliestBucket(context.Context) (time.Time, error) {
	return f.earliest, f.earliestErr
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
	if f.windows == nil {
		f.windows, f.forced = map[string][2]time.Time{}, map[string]bool{}
	}
	// RunCAGGRefreshStep cuts a long window into consecutive CALLs; record
	// them as the one per-view refresh they make up.
	if n := len(f.refreshed); n > 0 && f.refreshed[n-1] == view && f.windows[view][1].Equal(from) {
		from = f.windows[view][0]
	} else {
		f.refreshed = append(f.refreshed, view)
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
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 61_000_000, 61_999_999, true, testCAGGNow, &out); err != nil {
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

// A late trade lands in a bucket older than prices_1m's 15-minute policy
// start_offset (migration 0187): only this explicit refresh reaches it.
func TestRefreshTradesCAGGsOverLedgers_LateTradeBucketOlderThanPolicyLookback(t *testing.T) {
	f := &fakeTradesCAGGStore{
		from: testCAGGNow.Add(-40 * time.Minute),
		to:   testCAGGNow.Add(-35 * time.Minute),
	}
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 62_000_000, 62_000_100, true, testCAGGNow, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, ok := f.windows["prices_1m"]
	if !ok {
		t.Fatalf("prices_1m not refreshed; refreshed %v", f.refreshed)
	}
	if w[0].After(f.from) || w[1].Before(f.to) {
		t.Errorf("prices_1m window [%s,%s] does not cover the late span [%s,%s]", w[0], w[1], f.from, f.to)
	}
}

// A view built on a failed one would re-materialise from stale input.
func TestRefreshTradesCAGGsOverLedgers_StopsAtTheFirstFailure(t *testing.T) {
	f := &fakeTradesCAGGStore{from: time.Unix(1_700_000_000, 0), to: time.Unix(1_700_100_000, 0), failView: "prices_1m"}
	var out bytes.Buffer
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, true, testCAGGNow, &out)
	if err == nil || !strings.Contains(err.Error(), "prices_1m") {
		t.Fatalf("err = %v, want the prices_1m failure", err)
	}
	if len(f.refreshed) != 0 || out.Len() != 0 {
		t.Errorf("carried on past the failure: refreshed %v, stdout %q", f.refreshed, out.String())
	}
}

func TestRefreshTradesCAGGsOverLedgers_EmptyRangeIsAnError(t *testing.T) {
	f := &fakeTradesCAGGStore{spanErr: timescale.ErrNotFound}
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, true, testCAGGNow, &bytes.Buffer{})
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
	if err := refreshTradesCAGGsOverLedgers(context.Background(), f, 61_000_000, 61_000_100, true, testCAGGNow, &bytes.Buffer{}); err != nil {
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
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, true, testCAGGNow, &out)
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

func TestParseTradesCAGGRefreshArgs(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		want    tradesCAGGRefreshArgs
		wantErr string
	}{
		{
			"default is forced",
			[]string{"-config", "c", "-from", "1", "-to", "2", "-write"},
			tradesCAGGRefreshArgs{cfgPath: "c", from: 1, to: 2, force: true, write: true},
			"",
		},
		{
			"non-forced",
			[]string{"-config", "c", "-from", "1", "-to", "2", "-force=false", "-write"},
			tradesCAGGRefreshArgs{cfgPath: "c", from: 1, to: 2, write: true},
			"",
		},
		{"dry run", []string{"-config", "c", "-from", "1", "-to", "2", "-dry-run"}, tradesCAGGRefreshArgs{cfgPath: "c", from: 1, to: 2, force: true}, ""},
		{"no mode", []string{"-config", "c", "-from", "1", "-to", "2"}, tradesCAGGRefreshArgs{}, "state the mode"},
		{"size is read-only", []string{"-config", "c", "-size", "-write"}, tradesCAGGRefreshArgs{}, "takes only -config, not -write"},
		{"size", []string{"-config", "c", "-size"}, tradesCAGGRefreshArgs{cfgPath: "c", size: true}, ""},
		{"size takes no range", []string{"-config", "c", "-size", "-from", "1"}, tradesCAGGRefreshArgs{}, "takes only -config, not -from"},
		{"size takes no force", []string{"-config", "c", "-size", "-force=false"}, tradesCAGGRefreshArgs{}, "takes only -config, not -force"},
		{"no config", []string{"-from", "1", "-to", "2"}, tradesCAGGRefreshArgs{}, "-config is required"},
		{"spaced bool", []string{"-config", "c", "-from", "1", "-to", "2", "-force", "false"}, tradesCAGGRefreshArgs{}, `unexpected argument "false"`},
		{"no range", []string{"-config", "c", "-force=false"}, tradesCAGGRefreshArgs{}, "-from and -to are required"},
		{"inverted range", []string{"-config", "c", "-from", "3", "-to", "2"}, tradesCAGGRefreshArgs{}, "-from and -to are required"},
		{"range past uint32", []string{"-config", "c", "-from", "1", "-to", "4294967296"}, tradesCAGGRefreshArgs{}, "-from and -to are required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseTradesCAGGRefreshArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

// -force=false runs the forced plan's views, order and windows, with no
// CALL forced: only the buckets the invalidation log names are rebuilt.
func TestRefreshTradesCAGGsOverLedgers_NonForcedForcesNothing(t *testing.T) {
	span := func() *fakeTradesCAGGStore {
		return &fakeTradesCAGGStore{
			from: time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC),
			to:   time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC),
		}
	}
	forced, plain := span(), span()
	if err := refreshTradesCAGGsOverLedgers(context.Background(), forced, 1, 2, true, testCAGGNow, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := refreshTradesCAGGsOverLedgers(context.Background(), plain, 1, 2, false, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plain.refreshed, forced.refreshed) {
		t.Fatalf("non-forced refreshed %v, want the forced order %v", plain.refreshed, forced.refreshed)
	}
	for _, v := range plain.refreshed {
		if plain.forced[v] {
			t.Errorf("%s was refreshed with force => true under -force=false", v)
		}
		if plain.windows[v] != forced.windows[v] {
			t.Errorf("%s window %v, want the forced plan's %v", v, plain.windows[v], forced.windows[v])
		}
	}
	if !strings.Contains(out.String(), " forced=false ") {
		t.Errorf("success line does not say the run was non-forced: %q", out.String())
	}
}

func TestRefreshTradesCAGGsOverLedgers_NonForcedRefusesTwapsWhileRetentionIsArmed(t *testing.T) {
	f := &fakeTradesCAGGStore{from: time.Unix(1_700_000_000, 0), to: time.Unix(1_700_100_000, 0), armed: true}
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, false, testCAGGNow, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "retention policy is armed") {
		t.Fatalf("err = %v, want a refusal naming the armed retention policy", err)
	}
	for _, v := range timescale.CAGGsOnPrices1m {
		if _, ok := f.windows[v]; ok {
			t.Errorf("%s was refreshed non-forced while prices_1m's retention is armed", v)
		}
	}
}

type fakeTradesCAGGSizer struct {
	backlogs    []timescale.CAGGInvalidationBacklog
	ledgersErr  error
	asked       [2]time.Time
	earliest    time.Time
	earliestErr error
	cutoff      time.Time
	driftAt     time.Time
	fakeInvalidated
}

func (f *fakeTradesCAGGSizer) TradesPrices1mDrift(_ context.Context, from, to time.Time) ([]timescale.TradesPrices1mDrift, error) {
	return fakeDrift(f.driftAt, from, to), nil
}

func (f *fakeTradesCAGGSizer) Prices1mEarliestBucket(context.Context) (time.Time, error) {
	return f.earliest, f.earliestErr
}

// CAGGInvalidationBacklogs splits each log's hull at cutoff, as the
// store splits each entry.
func (f *fakeTradesCAGGSizer) CAGGInvalidationBacklogs(_ context.Context, views []string, cutoff time.Time) ([]timescale.CAGGInvalidationBacklog, error) {
	f.cutoff = cutoff
	out := make([]timescale.CAGGInvalidationBacklog, 0, len(views))
	for _, v := range views {
		b := timescale.CAGGInvalidationBacklog{View: v}
		for _, fb := range f.backlogs {
			if fb.View == v {
				b = fb
			}
		}
		widen := func(lo, hi *time.Time, from, to time.Time) {
			if lo.IsZero() || from.Before(*lo) {
				*lo = from
			}
			if hi.IsZero() || to.After(*hi) {
				*hi = to
			}
		}
		for _, r := range []struct {
			n        int64
			from, to time.Time
		}{{b.Ranges, b.From, b.To}, {b.SourceRanges, b.SourceFrom, b.SourceTo}} {
			if r.n == 0 {
				continue
			}
			if r.from.Before(cutoff) {
				b.Below += r.n
				widen(&b.BelowFrom, &b.BelowTo, r.from, minTime(r.to, cutoff))
			}
			if !r.to.Before(cutoff) {
				widen(&b.AboveFrom, &b.AboveTo, maxTime(r.from, cutoff), r.to)
			}
		}
		out = append(out, b)
	}
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (f *fakeTradesCAGGSizer) TradeLedgersInTimeRange(_ context.Context, from, to time.Time) (uint32, uint32, error) {
	f.asked = [2]time.Time{from, to}
	return 61_000_000, 61_500_000, f.ledgersErr
}

func TestSizeTradesCAGGBacklog(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2025, 3, day, 0, 0, 0, 0, time.UTC) }
	f := &fakeTradesCAGGSizer{backlogs: []timescale.CAGGInvalidationBacklog{
		{View: "prices_1m", Ranges: 2, From: d(12), To: d(13), Span: 2 * time.Hour, OpenEnded: 2},
		{
			View: "prices_1d", Ranges: 1, From: d(11), To: d(12), Span: 24 * time.Hour, OpenEnded: 2,
			SourceRanges: 1, SourceFrom: d(14), SourceTo: d(15),
		},
	}}
	var out bytes.Buffer
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(timescale.TradesCAGGs)+1 {
		t.Fatalf("%d lines, want one per trades aggregate plus the hull:\n%s", len(lines), out.String())
	}
	for _, want := range []string{
		"trades-cagg-refresh: pending prices_1m ranges=2 span=2h0m0s from=2025-03-12T00:00:00Z to=2025-03-13T00:00:00Z open-ended=2 source-log=0",
		"trades-cagg-refresh: pending prices_1d ranges=1 span=24h0m0s from=2025-03-11T00:00:00Z to=2025-03-12T00:00:00Z open-ended=2 source-log=1",
		"trades-cagg-refresh: pending twap_1d ranges=0 span=0s from=- to=- open-ended=0 source-log=0",
		"trades-cagg-refresh: pending hull=[2025-03-11T00:00:00Z,2025-03-15T00:00:00Z] ledgers=[61000000,61500000] catch-up: -force=false -from 61000000 -to 61500000 -write",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing line %q in:\n%s", want, out.String())
		}
	}
	// The source log's unmoved entries widen the hull: every view inherits them.
	if f.asked != [2]time.Time{d(11), d(15)} {
		t.Errorf("ledgers looked up over %v, want the hull [03-11, 03-15]", f.asked)
	}

	out.Reset()
	f.ledgersErr = timescale.ErrNotFound
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil || !strings.Contains(out.String(), "holds no trades") {
		t.Errorf("hull with no trades: err = %v, out %q", err, out.String())
	}

	out.Reset()
	if err := sizeTradesCAGGBacklog(context.Background(), &fakeTradesCAGGSizer{}, testCAGGNow, &out); err != nil ||
		!strings.HasSuffix(out.String(), "trades-cagg-refresh: pending none: no bounded invalidation range on any trades aggregate\n") {
		t.Errorf("nothing pending: err = %v, out %q", err, out.String())
	}
}

// A drop-then-disarm: retention emptied prices_1m below its earliest
// bucket and is disarmed again. A non-forced run whose twap windows reach
// below that bucket refuses before any CALL; one starting at the floor runs.
func TestRefreshTradesCAGGsOverLedgers_NonForcedRefusesTwapWindowsBelowPrices1mFloor(t *testing.T) {
	earliest := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	floor := tradesCAGGNonForcedFloor(earliest)
	if want := earliest.Add(36 * time.Hour); !floor.Equal(want) {
		t.Fatalf("floor = %s, want %s: twap_1d's half MinWindow past the earliest bucket", floor, want)
	}
	for _, c := range []struct {
		name    string
		from    time.Time
		force   bool
		wantErr string
	}{
		{"below the floor", floor.Add(-time.Second), false, "the twap_1d window [2025-02-28T23:59:59Z"},
		{"far below", time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC), false, "narrow -from to a ledger whose first trade is at or after 2025-03-02T12:00:00Z"},
		{"at the floor", floor, false, ""},
		{"forced below", time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC), true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTradesCAGGStore{from: c.from, to: c.from.Add(time.Hour), earliest: earliest}
			var out bytes.Buffer
			err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, c.force, testCAGGNow, &out)
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "-force=true") {
				t.Fatalf("err = %v, want a refusal containing %q and naming -force=true", err, c.wantErr)
			}
			if len(f.refreshed) != 0 {
				t.Errorf("refreshed %v before refusing", f.refreshed)
			}
		})
	}

	f := &fakeTradesCAGGStore{from: floor, to: floor.Add(time.Hour), earliestErr: timescale.ErrNotFound}
	err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, false, testCAGGNow, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "prices_1m holds no materialised bucket") || len(f.refreshed) != 0 {
		t.Fatalf("empty prices_1m: err = %v, refreshed %v; want a refusal before any CALL", err, f.refreshed)
	}
}

// -size leaves ranges below the floor out of the catch-up and names them.
func TestSizeTradesCAGGBacklog_RangesBelowPrices1mFloor(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2025, 3, day, 0, 0, 0, 0, time.UTC) }
	f := &fakeTradesCAGGSizer{earliest: d(10), backlogs: []timescale.CAGGInvalidationBacklog{
		{View: "prices_1m", Ranges: 1, From: d(20), To: d(21)},
		// A retention drop's invalidation, and one straddling the floor.
		{View: "twap_1d", Ranges: 2, From: d(1), To: d(15), SourceRanges: 1, SourceFrom: d(2), SourceTo: d(5)},
	}}
	var out bytes.Buffer
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"trades-cagg-refresh: pending twap_1d below-floor from=2025-03-01T00:00:00Z to=2025-03-11T12:00:00Z: starts before " +
			"2025-03-11T12:00:00Z, where twap windows reach below prices_1m's earliest bucket 2025-03-10T00:00:00Z, so -force=false " +
			"refuses it; refresh it with -force=true\n",
		"trades-cagg-refresh: pending hull=[2025-03-11T12:00:00Z,2025-03-21T00:00:00Z] ledgers=[61000000,61500000] catch-up: -force=false -from 61000000 -to 61500000 -write\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "prices_1m below-floor") {
		t.Errorf("flagged prices_1m, whose range is above the floor:\n%s", out.String())
	}
	if f.asked != [2]time.Time{d(11).Add(12 * time.Hour), d(21)} {
		t.Errorf("catch-up ledgers looked up over %v, want [floor, 03-21]", f.asked)
	}

	out.Reset()
	f.backlogs = f.backlogs[1:]
	f.backlogs[0].To = d(9)
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil ||
		!strings.HasSuffix(out.String(), "trades-cagg-refresh: pending catch-up none: every bounded range is below-floor\n") {
		t.Errorf("all below the floor: err = %v, out %q", err, out.String())
	}

	out.Reset()
	f.earliestErr = timescale.ErrNotFound
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil ||
		!strings.Contains(out.String(), "twap_1d below-floor from=2025-03-01T00:00:00Z to=2025-03-09T00:00:00Z: prices_1m holds no materialised bucket") {
		t.Errorf("empty prices_1m: err = %v, out %q", err, out.String())
	}
}

// A dropped stretch of prices_1m above its earliest bucket: the floor lets
// the run through, so the twap buckets it would recompute must be proven
// against trades first, less the minutes prices_1m's own step rebuilds.
func TestRefreshTradesCAGGsOverLedgers_NonForcedRefusesTwapReadsPrices1mLacks(t *testing.T) {
	h := func(hour int) time.Time { return time.Date(2025, 3, 10, hour, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		name    string
		driftAt time.Time
		force   bool
		wantErr string
	}{
		{"missing minute in a pending twap bucket", h(6), false, "prices_1m disagrees with trades over [2025-03-10T05:01:00Z,2025-03-11T00:00:00Z)"},
		{"stale minute prices_1m rebuilds first", h(5).Add(30 * time.Second), false, ""},
		{"no pending twap bucket reads it", h(5).Add(-48 * time.Hour), false, ""},
		{"forced", h(6), true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTradesCAGGStore{
				from: h(12), to: h(13), earliest: h(0).AddDate(0, -2, 0), driftAt: c.driftAt,
				fakeInvalidated: fakeInvalidated{
					"twap_1d":   {{h(3), h(4)}},
					"prices_1m": {{h(5).Add(10 * time.Second), h(5).Add(20 * time.Second)}},
				},
			}
			var out bytes.Buffer
			err := refreshTradesCAGGsOverLedgers(context.Background(), f, 1, 2, c.force, testCAGGNow, &out)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v\n%s", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "-force=true") {
				t.Fatalf("err = %v, want a refusal containing %q and naming -force=true", err, c.wantErr)
			}
			if len(f.refreshed) != 0 {
				t.Errorf("refreshed %v before refusing", f.refreshed)
			}
			if !strings.Contains(out.String(), "native/fiat:USD trades n=2") {
				t.Errorf("stdout does not show the drifting pair:\n%s", out.String())
			}
		})
	}
}

// -size raises the floor past the newest prices_1m gap its catch-up's twap
// buckets would read, so it never suggests a non-forced run over one.
func TestSizeTradesCAGGBacklog_GapAbovePrices1mFloor(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2025, 3, day, 0, 0, 0, 0, time.UTC) }
	f := &fakeTradesCAGGSizer{
		earliest:        d(1),
		backlogs:        []timescale.CAGGInvalidationBacklog{{View: "twap_1d", Ranges: 1, From: d(5), To: d(20)}},
		driftAt:         d(9).Add(12 * time.Hour),
		fakeInvalidated: fakeInvalidated{"twap_1d": {{d(5), d(20)}}},
	}
	var out bytes.Buffer
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"trades-cagg-refresh: pending prices_1m gap [2025-03-09T00:00:00Z,2025-03-10T00:00:00Z): ",
		"trades-cagg-refresh: pending twap_1d below-floor from=2025-03-05T00:00:00Z to=2025-03-11T12:00:00Z: starts before " +
			"2025-03-11T12:00:00Z, where twap windows reach the prices_1m gap [2025-03-09T00:00:00Z,2025-03-10T00:00:00Z), " +
			"so -force=false refuses it; refresh it with -force=true\n",
		"trades-cagg-refresh: pending hull=[2025-03-11T12:00:00Z,2025-03-20T00:00:00Z] ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	f.driftAt = time.Time{}
	if err := sizeTradesCAGGBacklog(context.Background(), f, testCAGGNow, &out); err != nil ||
		strings.Contains(out.String(), " gap ") || !strings.Contains(out.String(), "hull=[2025-03-05T00:00:00Z,2025-03-20T00:00:00Z]") {
		t.Errorf("no gap: err = %v, want the hull from the first range, out %q", err, out.String())
	}
}

func TestSubtractRanges(t *testing.T) {
	m := func(a, b int) [2]time.Time {
		return [2]time.Time{time.Unix(int64(a)*60, 0), time.Unix(int64(b)*60, 0)}
	}
	rs := mergeRanges([][2]time.Time{m(10, 20), m(0, 5), m(5, 8), m(15, 30)})
	if want := [][2]time.Time{m(0, 8), m(10, 30)}; !slices.Equal(rs, want) {
		t.Fatalf("mergeRanges = %v, want %v", rs, want)
	}
	got := subtractRanges(rs, [][2]time.Time{m(2, 3), m(7, 12), m(20, 25), m(29, 40)})
	if want := [][2]time.Time{m(0, 2), m(3, 7), m(12, 20), m(25, 29)}; !slices.Equal(got, want) {
		t.Errorf("subtractRanges = %v, want %v", got, want)
	}
}

// Without -write the plan is printed and no aggregate is refreshed.
func TestPreviewTradesCAGGRefresh_RefreshesNothing(t *testing.T) {
	f := &fakeTradesCAGGStore{
		from: time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC),
		to:   time.Date(2025, 5, 14, 12, 0, 0, 0, time.UTC),
	}
	var out bytes.Buffer
	if err := previewTradesCAGGRefresh(context.Background(), f, 61_000_000, 61_999_999, true, testCAGGNow, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.refreshed) != 0 {
		t.Fatalf("preview refreshed %v", f.refreshed)
	}
	for _, c := range timescale.TradesCAGGs {
		if !strings.Contains(out.String(), "would refresh "+c.Name+" ") {
			t.Errorf("preview does not name %s:\n%s", c.Name, out.String())
		}
	}
}
