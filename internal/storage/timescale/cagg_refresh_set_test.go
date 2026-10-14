// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"

	"github.com/jackc/pgx/v5/pgconn"
)

// The backfill refresh set must cover every SERVED rung: a granularity missing from [CAGGsLiveForever] leaves
// a permanent hole in backfilled ranges. The tests derive the served set from [AllHistoryGranularities] and
// source consts, so a new granularity cannot reach the API without joining the refresh set.

// TestDeclaredGranularityConstsAreAllServed requires every package-level `const ... HistoryGranularity` to
// appear in [AllHistoryGranularities]; one that skips the slice fails Validate, so callers get a 400 for a
// declared granularity.
func TestDeclaredGranularityConstsAreAllServed(t *testing.T) {
	t.Parallel()
	declared := declaredGranularityConsts(t)
	if len(declared) == 0 {
		t.Fatal("found zero HistoryGranularity constants in this package's source — " +
			"the declaration shape changed under this guard's feet, and it is now " +
			"asserting nothing")
	}
	for name, value := range declared {
		if err := HistoryGranularity(value).Validate(); err != nil {
			t.Errorf("const %s = %q is declared but absent from AllHistoryGranularities, "+
				"so Validate rejects it (%v) — add it to the slice, and to CAGGsLiveForever "+
				"with it, or delete the constant", name, value, err)
		}
	}
	// And the reverse: nothing in the slice may be a bare string with
	// no constant behind it, which is how a typo'd rung gets served.
	byValue := map[string]bool{}
	for _, v := range declared {
		byValue[v] = true
	}
	for _, g := range AllHistoryGranularities {
		if !byValue[string(g)] {
			t.Errorf("AllHistoryGranularities holds %q, which no HistoryGranularity constant "+
				"in this package declares", g)
		}
	}
}

// declaredGranularityConsts parses this package's non-test sources and returns {const name: wire value} for
// every package-level HistoryGranularity constant, syntactically. A grouped const block carries the type
// forward as the language does.
func declaredGranularityConsts(t *testing.T) map[string]string {
	t.Helper()
	return astguard.ByName(astguard.Typed(t, astguard.PackageConsts(t, "."), "HistoryGranularity"))
}

// TestCAGGsLiveForeverCoversEveryServedGranularity is the assertion
// that matters: every granularity the API serves must be refreshed by
// the backfill, or that grain is permanently empty over the range.
func TestCAGGsLiveForeverCoversEveryServedGranularity(t *testing.T) {
	t.Parallel()
	inSet := map[string]CAGGSpec{}
	for _, spec := range CAGGsLiveForever {
		if _, dup := inSet[spec.Name]; dup {
			t.Errorf("CAGGsLiveForever lists %s twice", spec.Name)
		}
		inSet[spec.Name] = spec
	}
	for _, g := range AllHistoryGranularities {
		view := "prices_" + string(g)
		if _, ok := inSet[view]; !ok {
			t.Errorf("granularity %q is served (OHLCSeries / HistoryPointsInRange read %s) but the "+
				"backfill never refreshes %s — every backfilled range keeps a permanent hole at that "+
				"resolution", g, view, view)
		}
	}
	// The reverse direction: a view in the set that no read serves
	// would be work with no reader, and a name RefreshContinuousAggregate
	// would reject at runtime rather than at build time.
	for _, spec := range CAGGsLiveForever {
		if !allowedCAGGViews[spec.Name] {
			t.Errorf("CAGGsLiveForever lists %s, which RefreshContinuousAggregate's allow-list rejects — "+
				"every chunk's refresh of it would fail", spec.Name)
		}
		g := HistoryGranularity(strings.TrimPrefix(spec.Name, "prices_"))
		if err := g.Validate(); err != nil {
			t.Errorf("CAGGsLiveForever refreshes %s, which maps to no served granularity: %v", spec.Name, err)
		}
	}
}

// TestCAGGsLiveForeverRefreshesPrices1mFirst pins the ordering convention. No member reads prices_1m today,
// so it is defensive: it is the only view another aggregate builds on, so a future rung reading it finds it
// current (as chops.xlmBaseRestampCAGGs).
func TestCAGGsLiveForeverRefreshesPrices1mFirst(t *testing.T) {
	t.Parallel()
	if len(CAGGsLiveForever) == 0 {
		t.Fatal("CAGGsLiveForever is empty")
	}
	if got := CAGGsLiveForever[0].Name; got != "prices_1m" {
		t.Errorf("first refreshed view is %s, want prices_1m — it is the view the hierarchical TWAP "+
			"aggregates are built on, so refreshing it late would rebuild them from stale input", got)
	}
}

// TestCAGGsLiveForeverMinWindowsClearTimescaleMinimum checks each entry against the rule that
// produced it: refresh_continuous_aggregate rejects a window narrower than 2x the bucket width
// (SQLSTATE 22023), which would ship a partially-stale surface rather than fail loudly.
func TestCAGGsLiveForeverMinWindowsClearTimescaleMinimum(t *testing.T) {
	t.Parallel()
	for _, spec := range CAGGsLiveForever {
		g := HistoryGranularity(strings.TrimPrefix(spec.Name, "prices_"))
		bucket := g.BucketDuration()
		if bucket == 0 {
			t.Errorf("%s maps to no bucket duration", spec.Name)
			continue
		}
		if spec.MinWindow < 2*bucket {
			t.Errorf("%s MinWindow %v is under Timescale's 2-bucket minimum (%v) — the refresh call "+
				"would be rejected with SQLSTATE 22023", spec.Name, spec.MinWindow, 2*bucket)
		}
	}
}

// TestPadRefreshWindowCoversTheChunkAtEveryRung proves the fine rungs are refreshed over the same range as
// the coarse ones: for a chunk shorter than any minimum, every entry's padded window must still contain the
// chunk end to end.
func TestPadRefreshWindowCoversTheChunkAtEveryRung(t *testing.T) {
	t.Parallel()
	from := time.Date(2024, 3, 12, 9, 0, 0, 0, time.UTC)
	to := from.Add(90 * time.Second) // shorter than every MinWindow
	for _, spec := range CAGGsLiveForever {
		padFrom, padTo := PadRefreshWindow(from, to, spec.MinWindow)
		if padFrom.After(from) || padTo.Before(to) {
			t.Errorf("%s padded window [%s, %s] does not cover the chunk [%s, %s]",
				spec.Name, padFrom, padTo, from, to)
		}
		if got := padTo.Sub(padFrom); got < spec.MinWindow {
			t.Errorf("%s padded to %v, want >= its MinWindow %v", spec.Name, got, spec.MinWindow)
		}
	}
}

// TestHistoryGranularityListMatchesTheServedSet pins the generated enumeration to the slice. The
// 400 bodies for /v1/chart and /v1/history/since-inception render it, so a list that omits a
// served grain (or advertises one Validate rejects) tells callers the wrong contract.
func TestHistoryGranularityListMatchesTheServedSet(t *testing.T) {
	t.Parallel()
	listed := strings.Split(HistoryGranularityList(), historyGranularitySep)
	if len(listed) != len(AllHistoryGranularities) {
		t.Fatalf("HistoryGranularityList() renders %d entries, AllHistoryGranularities holds %d",
			len(listed), len(AllHistoryGranularities))
	}
	for i, raw := range listed {
		if HistoryGranularity(raw) != AllHistoryGranularities[i] {
			t.Errorf("entry %d is %q, want %q", i, raw, AllHistoryGranularities[i])
		}
	}
	if err := HistoryGranularity("definitely-not-a-granularity").Validate(); err == nil {
		t.Error("Validate accepted a bogus granularity")
	} else if !strings.Contains(err.Error(), HistoryGranularityList()) {
		t.Errorf("Validate's rejection no longer carries the generated enumeration: %v", err)
	}
}

func TestBucketCeil(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		in     string
		bucket time.Duration
		want   string
	}{
		{"2024-01-10T12:00:30Z", time.Minute, "2024-01-10T12:01:00Z"},
		{"2024-01-10T12:01:00Z", time.Minute, "2024-01-10T12:01:00Z"},
		{"2024-01-10T01:00:00Z", 4 * time.Hour, "2024-01-10T04:00:00Z"},
		{"2024-01-10T00:00:01Z", 24 * time.Hour, "2024-01-11T00:00:00Z"},
		{"2024-01-10T12:00:00Z", 7 * 24 * time.Hour, "2024-01-15T00:00:00Z"}, // Wednesday → Monday
		{"1999-12-29T12:00:00Z", 7 * 24 * time.Hour, "2000-01-03T00:00:00Z"}, // before the origin
		{"2024-01-31T12:00:00Z", MonthBucket, "2024-02-01T00:00:00Z"},
		{"2024-12-15T00:00:00Z", MonthBucket, "2025-01-01T00:00:00Z"},
		{"2024-03-01T00:00:00Z", MonthBucket, "2024-03-01T00:00:00Z"},
	}
	for _, c := range cases {
		if got := bucketCeil(at(c.in), c.bucket); !got.Equal(at(c.want)) {
			t.Errorf("bucketCeil(%s, %s) = %s, want %s", c.in, c.bucket, got.Format(time.RFC3339), c.want)
		}
	}
}

// The pieces must refresh exactly the buckets the whole window would:
// contiguous, covering [From, To), every cut on the view's grid, and each
// at least MinWindow so no CALL is rejected as too small.
func TestRefreshPiecesCoverTheWindowOnTheBucketGrid(t *testing.T) {
	from := time.Date(2024, 1, 10, 13, 17, 23, 0, time.UTC)
	for _, spec := range slices.Concat(TradesCAGGs, OracleCAGGs) {
		if spec.Bucket == 0 {
			t.Errorf("%s has no Bucket, so its refresh is never cut", spec.Name)
			continue
		}
		span := max(CAGGRefreshPieceSpan, spec.MinWindow)
		for _, length := range []time.Duration{spec.MinWindow, span, span*5/2 + 7*time.Second, 400 * 24 * time.Hour} {
			st := CAGGRefreshStep{View: spec.Name, From: from, To: from.Add(length), Force: true, Bucket: spec.Bucket, MinWindow: spec.MinWindow}
			pieces := RefreshPieces(st)
			if length >= 2*span+spec.MinWindow && len(pieces) < 2 {
				t.Errorf("%s over %s: one CALL, want it cut", spec.Name, length)
			}
			if !pieces[0].From.Equal(st.From) || !pieces[len(pieces)-1].To.Equal(st.To) {
				t.Fatalf("%s over %s: pieces span [%s, %s), want [%s, %s)", spec.Name, length,
					pieces[0].From, pieces[len(pieces)-1].To, st.From, st.To)
			}
			for i, p := range pieces {
				if p.View != st.View || p.Force != st.Force || p.Bucket != st.Bucket {
					t.Fatalf("%s piece %d lost the step's view/force/bucket: %+v", spec.Name, i, p)
				}
				if p.To.Sub(p.From) < spec.MinWindow {
					t.Errorf("%s over %s: piece %d is %s, under MinWindow %s", spec.Name, length, i, p.To.Sub(p.From), spec.MinWindow)
				}
				if i == 0 {
					continue
				}
				if !p.From.Equal(pieces[i-1].To) {
					t.Fatalf("%s over %s: gap or overlap between pieces %d and %d", spec.Name, length, i-1, i)
				}
				if !bucketCeil(p.From, spec.Bucket).Equal(p.From) {
					t.Errorf("%s over %s: cut %s is not on the %s grid", spec.Name, length, p.From, spec.Bucket)
				}
			}
		}
	}
}

func TestRefreshPiecesLeavesAnUnknownBucketWhole(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	st := CAGGRefreshStep{View: "supply_1d", From: from, To: from.Add(400 * 24 * time.Hour), MinWindow: 72 * time.Hour}
	if got := RefreshPieces(st); len(got) != 1 || got[0] != st {
		t.Fatalf("RefreshPieces = %+v, want the step unchanged", got)
	}
}

type pieceCall struct {
	view     string
	from, to time.Time
	forced   bool
}

type fakePieceRefresher struct {
	calls  []pieceCall
	failAt map[int]error
	onCall func(n int)
}

func (f *fakePieceRefresher) record(view string, from, to time.Time, forced bool) error {
	n := len(f.calls)
	f.calls = append(f.calls, pieceCall{view, from, to, forced})
	if f.onCall != nil {
		f.onCall(n)
	}
	return f.failAt[n]
}

func (f *fakePieceRefresher) RefreshContinuousAggregate(_ context.Context, view string, from, to time.Time) error {
	return f.record(view, from, to, false)
}

func (f *fakePieceRefresher) RefreshContinuousAggregateForced(_ context.Context, view string, from, to time.Time) error {
	return f.record(view, from, to, true)
}

func fiveDayPrices1mStep() CAGGRefreshStep {
	from := time.Date(2024, 1, 10, 6, 0, 0, 0, time.UTC)
	return CAGGRefreshStep{View: "prices_1m", From: from, To: from.Add(5 * 24 * time.Hour), Force: true, Bucket: time.Minute, MinWindow: 2 * time.Minute}
}

func TestRunCAGGRefreshStepCallsEveryPiece(t *testing.T) {
	st := fiveDayPrices1mStep()
	f := &fakePieceRefresher{}
	if err := RunCAGGRefreshStep(context.Background(), f, st, false); err != nil {
		t.Fatal(err)
	}
	want := RefreshPieces(st)
	if len(want) != 5 || len(f.calls) != len(want) {
		t.Fatalf("%d CALLs for %d pieces, want 5 one-day CALLs", len(f.calls), len(want))
	}
	for i, c := range f.calls {
		if !c.forced || c.view != st.View || !c.from.Equal(want[i].From) || !c.to.Equal(want[i].To) {
			t.Errorf("call %d = %+v, want forced %s [%s, %s)", i, c, st.View, want[i].From, want[i].To)
		}
	}
}

// A failed window must not stop the rest, and must surface: the step
// errs, names the window, and keeps the cause matchable.
func TestRunCAGGRefreshStepAFailedPieceFailsTheStepButNotTheRest(t *testing.T) {
	st := fiveDayPrices1mStep()
	pieces := RefreshPieces(st)
	cause := &CAGGRefreshTimeoutError{View: st.View, From: pieces[1].From, To: pieces[1].To, Timeout: time.Hour}
	f := &fakePieceRefresher{failAt: map[int]error{1: cause}}
	err := RunCAGGRefreshStep(context.Background(), f, st, false)
	if err == nil {
		t.Fatal("a failed piece returned nil")
	}
	if len(f.calls) != len(pieces) {
		t.Fatalf("%d of %d pieces attempted after one failed", len(f.calls), len(pieces))
	}
	var tErr *CAGGRefreshTimeoutError
	if !errors.As(err, &tErr) {
		t.Errorf("cause lost from %v", err)
	}
	if !strings.Contains(err.Error(), "1 of 5") || !strings.Contains(err.Error(), refreshWindowString(pieces[1])) {
		t.Errorf("error does not name the unrefreshed window: %v", err)
	}
}

// Once the context is done every later window is unrefreshed too, and
// the error must say so rather than name only the one that failed.
func TestRunCAGGRefreshStepCancelledNamesEveryRemainingWindow(t *testing.T) {
	st := fiveDayPrices1mStep()
	pieces := RefreshPieces(st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakePieceRefresher{failAt: map[int]error{2: context.Canceled}, onCall: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	err := RunCAGGRefreshStep(ctx, f, st, false)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.calls) != 3 {
		t.Errorf("%d CALLs, want 3 (stop after the cancelled one)", len(f.calls))
	}
	for _, p := range pieces[2:] {
		if !strings.Contains(err.Error(), refreshWindowString(p)) {
			t.Errorf("error omits unrefreshed window %s: %v", refreshWindowString(p), err)
		}
	}
	if !strings.Contains(err.Error(), "3 of 5") {
		t.Errorf("error miscounts unrefreshed windows: %v", err)
	}
}

func TestRunCAGGRefreshStepRefusesArmedRetentionBeforeAnyCall(t *testing.T) {
	st := fiveDayPrices1mStep()
	st.View = "twap_1h"
	f := &fakePieceRefresher{}
	if err := RunCAGGRefreshStep(context.Background(), f, st, true); err == nil || len(f.calls) != 0 {
		t.Fatalf("err = %v after %d CALLs, want a refusal before any", err, len(f.calls))
	}
}

// A non-forced twap refresh applies the invalidations each retention drop
// logged against it, recomputing those buckets from emptied minute rows.
func TestRunCAGGRefreshStepRefusesNonForcedTwapWhileRetentionArmed(t *testing.T) {
	st := fiveDayPrices1mStep()
	st.View, st.Force = "twap_1d", false
	f := &fakePieceRefresher{}
	if err := RunCAGGRefreshStep(context.Background(), f, st, true); err == nil || len(f.calls) != 0 {
		t.Fatalf("err = %v after %d CALLs, want a refusal before any", err, len(f.calls))
	}
	st.View = "prices_1d"
	if err := RunCAGGRefreshStep(context.Background(), f, st, true); err != nil || len(f.calls) == 0 {
		t.Fatalf("prices_1d reads trades, not prices_1m: err = %v after %d CALLs, want it refreshed", err, len(f.calls))
	}
}

func TestCAGGRefreshWindows(t *testing.T) {
	t.Parallel()
	cols := []string{"view_name", "hypertable_name", "materialization_hypertable_name", "has_policy", "start_offset_seconds", "schedule_interval_seconds"}
	store, conn := newScriptedStore(t, scriptedResult{cols: cols, rows: [][]driver.Value{
		{"oracle_prices_1m", "oracle_updates", "_materialized_hypertable_52", true, int64(300), int64(60)},
		{"prices_1m", "trades", "_materialized_hypertable_156", true, int64(900), int64(60)},
		{"source_volume_1h", "trades", "_materialized_hypertable_91", false, nil, int64(0)},
		{"supply_1d", "asset_supply_history", "_materialized_hypertable_90", true, nil, int64(3600)},
		{"twap_1d", "_materialized_hypertable_156", "_materialized_hypertable_164", true, int64(604800), int64(3600)},
	}})
	got, err := store.CAGGRefreshWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []CAGGRefreshWindow{
		{View: "oracle_prices_1m", HasPolicy: true, StartOffset: 5 * time.Minute, ScheduleInterval: time.Minute, Hypertable: "oracle_updates"},
		{View: "prices_1m", HasPolicy: true, StartOffset: 15 * time.Minute, ScheduleInterval: time.Minute, Hypertable: "trades"},
		{View: "source_volume_1h", Hypertable: "trades"},
		{View: "supply_1d", HasPolicy: true, Unbounded: true, ScheduleInterval: time.Hour, Hypertable: "asset_supply_history"},
		{View: "twap_1d", HasPolicy: true, StartOffset: 7 * 24 * time.Hour, ScheduleInterval: time.Hour, Hypertable: "trades"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	stmt := conn.only(t)
	for _, frag := range []string{
		"timescaledb_information.continuous_aggregates",
		"c.hypertable_name",
		"c.materialization_hypertable_name,",
		"LEFT JOIN timescaledb_information.jobs",
		"proc_name = 'policy_refresh_continuous_aggregate'",
		"IN (c.view_name, c.materialization_hypertable_name)",
		"config->>'start_offset'",
		"j.schedule_interval",
	} {
		if !strings.Contains(stmt.sql, frag) {
			t.Errorf("SQL lacks %q:\n%s", frag, stmt.sql)
		}
	}
}

// TestCAGGRefreshTimeout_DerivedFromWindow pins the refresh bound: five minutes per hour of window, floored
// at ten minutes and capped at four hours. The rows are the windows the backfills actually ask for, so a
// constant change shows up as a changed expectation.
func TestCAGGRefreshTimeout_DerivedFromWindow(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{"zero window gets the floor", 0, CAGGRefreshTimeoutFloor},
		{"negative window gets the floor", -time.Hour, CAGGRefreshTimeoutFloor},
		{"prices_1m MinWindow (2m) is floored", 2 * time.Minute, CAGGRefreshTimeoutFloor},
		{"prices_15m MinWindow (30m) is floored", 30 * time.Minute, CAGGRefreshTimeoutFloor},
		{"one hour computes below the floor", time.Hour, CAGGRefreshTimeoutFloor},
		{"exactly two hours is the floor by arithmetic", 2 * time.Hour, 10 * time.Minute},
		{"prices_1h MinWindow (3h)", 3 * time.Hour, 15 * time.Minute},
		{"a 10k-ledger chunk (~4h)", 4 * time.Hour, 20 * time.Minute},
		{"prices_4h MinWindow (12h)", 12 * time.Hour, time.Hour},
		{"one day", 24 * time.Hour, 2 * time.Hour},
		{"a 40k-ledger SDEX history chunk (~55h)", 55 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"prices_1d MinWindow (3d) is capped", 3 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"a weekly slice is capped", 7 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"prices_1mo MinWindow (93d) is capped", 93 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CAGGRefreshTimeout(tc.window); got != tc.want {
				t.Fatalf("CAGGRefreshTimeout(%s) = %s, want %s", tc.window, got, tc.want)
			}
		})
	}
	// The constants themselves, so a drive-by edit to one of them is a
	// deliberate change to this file too.
	if CAGGRefreshTimeoutPerWindowHour != 5*time.Minute {
		t.Errorf("CAGGRefreshTimeoutPerWindowHour = %s, want 5m (12x real time)", CAGGRefreshTimeoutPerWindowHour)
	}
	if CAGGRefreshTimeoutFloor != 10*time.Minute {
		t.Errorf("CAGGRefreshTimeoutFloor = %s, want 10m", CAGGRefreshTimeoutFloor)
	}
	if CAGGRefreshTimeoutCeiling != 4*time.Hour {
		t.Errorf("CAGGRefreshTimeoutCeiling = %s, want 4h", CAGGRefreshTimeoutCeiling)
	}
	if CAGGRefreshTimeoutFloor >= CAGGRefreshTimeoutCeiling {
		t.Errorf("floor %s must be below ceiling %s", CAGGRefreshTimeoutFloor, CAGGRefreshTimeoutCeiling)
	}
}

// TestCAGGRefreshTimeout_EveryLiveRungIsBounded: every view the
// backfill walks, at its own MinWindow, gets a bound inside
// [floor, ceiling] — no rung computes to zero or escapes the cap.
func TestCAGGRefreshTimeout_EveryLiveRungIsBounded(t *testing.T) {
	for _, spec := range CAGGsLiveForever {
		got := CAGGRefreshTimeout(spec.MinWindow)
		if got < CAGGRefreshTimeoutFloor || got > CAGGRefreshTimeoutCeiling {
			t.Errorf("%s: CAGGRefreshTimeout(MinWindow=%s) = %s, outside [%s, %s]",
				spec.Name, spec.MinWindow, got, CAGGRefreshTimeoutFloor, CAGGRefreshTimeoutCeiling)
		}
	}
}

// TestCAGGRefreshTimeoutError_NamesViewAndWindow: the typed error names the view, window and bound in its
// message and unwraps to the driver error so SQLSTATE matching on the chain still works.
func TestCAGGRefreshTimeoutError_NamesViewAndWindow(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	cause := &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	var err error = &CAGGRefreshTimeoutError{View: "prices_1m", From: from, To: to, Timeout: 20 * time.Minute, Err: cause}

	msg := err.Error()
	for _, want := range []string{"prices_1m", "2026-09-01T00:00:00Z", "2026-09-01T04:00:00Z", "20m0s", "57014"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message lacks %q: %s", want, msg)
		}
	}
	var tErr *CAGGRefreshTimeoutError
	if !errors.As(err, &tErr) || tErr.View != "prices_1m" {
		t.Fatalf("errors.As(*CAGGRefreshTimeoutError) failed on %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("typed error must unwrap to the driver's PgError; got %v", err)
	}
	if !isStatementTimeoutErr(fmt.Errorf("wrapped: %w", cause)) {
		t.Error("isStatementTimeoutErr must see 57014 through a wrap")
	}
	// The shape a real refresh_continuous_aggregate produces: the procedure re-throws the cancellation as
	// internal_error, so the code is XX000 and only the message says what happened (seen on timescaledb
	// 2.26.4-pg15).
	if !isStatementTimeoutErr(&pgconn.PgError{Code: "XX000", Message: "canceling statement due to statement timeout"}) {
		t.Error("isStatementTimeoutErr must match the XX000 + message shape the refresh procedure emits")
	}
	if isStatementTimeoutErr(&pgconn.PgError{Code: "XX000", Message: "some other internal error"}) {
		t.Error("isStatementTimeoutErr must not match an unrelated XX000")
	}
	if isStatementTimeoutErr(&pgconn.PgError{Code: "55P03"}) {
		t.Error("isStatementTimeoutErr must not match 55P03 (concurrent refresh) — that one is retried")
	}
	if isStatementTimeoutErr(context.DeadlineExceeded) {
		t.Error("isStatementTimeoutErr is the SQL-side arm only; the ctx arm is matched separately")
	}
}

// TestRefreshContinuousAggregateWithTimeout_RejectsNonPositive: there is deliberately no "0 disables the
// bound" arm, as an unbounded refresh is the defect. The guard runs before any connection is taken, which a
// nil-pool Store proves.
func TestRefreshContinuousAggregateWithTimeout_RejectsNonPositive(t *testing.T) {
	s := &Store{}
	now := time.Now()
	for _, timeout := range []time.Duration{0, -time.Second} {
		err := s.RefreshContinuousAggregateWithTimeout(context.Background(), "prices_1m", now.Add(-time.Hour), now, timeout)
		if err == nil || !strings.Contains(err.Error(), "non-positive timeout") {
			t.Errorf("timeout %s: err = %v, want non-positive-timeout rejection", timeout, err)
		}
	}
	if err := s.RefreshContinuousAggregateWithTimeout(context.Background(), "not_a_view", now.Add(-time.Hour), now, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "unknown view") {
		t.Errorf("unknown view must still be rejected first: %v", err)
	}
}

// The guard's problem arm: a found failure admits a lower-tip
// write whether or not it names a ledger; a verdict that is false only
// because a claim was not evaluated does not.
func TestCompletenessSnapshotFoundProblem(t *testing.T) {
	cases := []struct {
		name string
		snap CompletenessSnapshot
		want bool
	}{
		{"clean", CompletenessSnapshot{Complete: true, ProjectionOK: true}, false},
		{"located problem", CompletenessSnapshot{FirstProblem: 63_100_000}, true},
		{"CH reconcile mismatch, no ledger", CompletenessSnapshot{LakeComplete: true, FoundProblem: true}, true},
		{"projection not evaluated", CompletenessSnapshot{LakeComplete: true}, false},
		{"degenerate tip below genesis", CompletenessSnapshot{Genesis: 100, Tip: 50, Watermark: 99}, false},
	}
	for _, tc := range cases {
		if got := tc.snap.foundProblem(); got != tc.want {
			t.Errorf("%s: foundProblem() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
