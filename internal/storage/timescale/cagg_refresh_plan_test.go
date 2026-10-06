// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

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
