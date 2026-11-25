package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
)

// fakeAnchors answers each pair from its spelling and the minute of the instant asked, so an answer from the wrong
// minute is visible; pairs whose base code ends in an odd digit have no bucket. Set forms answer as the singles do.
type fakeAnchors struct {
	single, before, sets, inFlight, maxInFlight atomic.Int64
	fault                                       string // "", "error" or "panic": how every set read fails
}

func (f *fakeAnchors) anchor(p canonical.Pair, t time.Time) (Vwap1mRow, error) {
	if c := p.Base.Code; (c[len(c)-1]-'0')%2 == 1 {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	b := t.Truncate(time.Minute).Add(-time.Minute)
	return Vwap1mRow{Bucket: b, VWAP: fmt.Sprintf("1.%02d", b.Minute()), Sources: []string{p.String()}}, nil
}

func (f *fakeAnchors) baseline(p canonical.Pair, before time.Time, limit int) []Vwap1mRow {
	out := make([]Vwap1mRow, 0, min(limit, 3))
	for i := 1; i <= 3 && i <= limit; i++ {
		out = append(out, Vwap1mRow{Bucket: before.Add(-time.Duration(i) * time.Minute), VWAP: "1.5", Sources: []string{p.String()}})
	}
	return out
}

func (f *fakeAnchors) ClosedVWAP1mAtOrBefore(_ context.Context, p canonical.Pair, t time.Time) (Vwap1mRow, error) {
	f.single.Add(1)
	return f.anchor(p, t)
}

func (f *fakeAnchors) ClosedVWAP1mCombinedBefore(_ context.Context, p canonical.Pair, before time.Time, limit int) ([]Vwap1mRow, error) {
	f.before.Add(1)
	return f.baseline(p, before, limit), nil
}

func (f *fakeAnchors) enter(n int) error {
	f.sets.Add(1)
	if v := f.inFlight.Add(1); v > f.maxInFlight.Load() {
		f.maxInFlight.Store(v)
	}
	time.Sleep(time.Millisecond)
	f.inFlight.Add(-1)
	if n > prefetchChunk {
		return fmt.Errorf("a set statement carried %d pairs", n)
	}
	switch f.fault {
	case "error":
		return errors.New("set read failed")
	case "panic":
		panic("set read panicked")
	}
	return nil
}

func (f *fakeAnchors) ClosedVWAP1mAtOrBeforeSet(_ context.Context, pairs []canonical.Pair, t time.Time) ([]KeyResult[Vwap1mRow], error) {
	if err := f.enter(len(pairs)); err != nil {
		return nil, err
	}
	out := make([]KeyResult[Vwap1mRow], len(pairs))
	for i, p := range pairs {
		out[i].Value, out[i].Err = f.anchor(p, t)
	}
	return out, nil
}

func (f *fakeAnchors) ClosedVWAP1mCombinedBeforeSet(_ context.Context, pairs []canonical.Pair, before time.Time, limit int) ([][]Vwap1mRow, error) {
	if err := f.enter(len(pairs)); err != nil {
		return nil, err
	}
	out := make([][]Vwap1mRow, len(pairs))
	for i, p := range pairs {
		out[i] = f.baseline(p, before, limit)
	}
	return out, nil
}

func anchorCount(seam, result string) float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues(seam, result))
}

// A prefetched anchor answers a read in the minute it was read for, as that read would; a read in another minute, or
// within a microsecond of the next, and a baseline read with another limit, are read as before.
func TestPrefetchAnchors_AnswersItsMinuteAndReadsAnyOtherAsBefore(t *testing.T) {
	f := &fakeAnchors{}
	p := PrefetchAnchors{AnchorReads: f}
	ctx := readset.With(context.Background(), readset.New())
	at := time.Date(2026, 11, 24, 12, 0, 30, 0, time.UTC)
	pairs := prefetchPairs(t, 250)
	hits, trailHits := anchorCount("anchor_vwap_1m", "hit"), anchorCount("anchor_trailing_vwap_1m", "hit")
	p.PrefetchChange24hAnchors(ctx, pairs, at, at.Add(-30*time.Minute), 40)
	// 3 anchor statements, then 2 baseline statements for the 125 found anchors (one bucket).
	if got := f.sets.Load(); got != 5 {
		t.Errorf("set statements = %d, want 5", got)
	}
	if got := f.maxInFlight.Load(); got > prefetchParallel {
		t.Errorf("%d statements in flight, want at most %d", got, prefetchParallel)
	}
	for _, pair := range pairs {
		want, wantErr := f.anchor(pair, at)
		got, err := p.ClosedVWAP1mAtOrBefore(ctx, pair, at.Add(20*time.Second))
		if !reflect.DeepEqual(got, want) || !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) {
			t.Fatalf("%v: got %+v %v, want %+v %v", pair, got, err, want, wantErr)
		}
		if err == nil {
			rows, err := p.ClosedVWAP1mCombinedBefore(ctx, pair, got.Bucket, 40)
			if err != nil || !reflect.DeepEqual(rows, f.baseline(pair, got.Bucket, 40)) {
				t.Fatalf("%v baseline: %+v %v", pair, rows, err)
			}
		}
	}
	if f.single.Load() != 0 || f.before.Load() != 0 {
		t.Fatalf("prefetched keys made %d anchor and %d baseline reads, want none", f.single.Load(), f.before.Load())
	}
	if d := anchorCount("anchor_vwap_1m", "hit") - hits; d != 250 {
		t.Errorf("anchor hits = %v, want 250 of 250", d)
	}
	if d := anchorCount("anchor_trailing_vwap_1m", "hit") - trailHits; d != 125 {
		t.Errorf("baseline hits = %v, want 125 of 125", d)
	}

	pair := pairs[0]
	for _, ts := range []time.Time{at.Add(40 * time.Second), at.Add(30*time.Second - 500*time.Nanosecond)} {
		before := f.single.Load()
		got, err := p.ClosedVWAP1mAtOrBefore(ctx, pair, ts)
		want, _ := f.anchor(pair, ts)
		if err != nil || !reflect.DeepEqual(got, want) || f.single.Load() != before+1 {
			t.Errorf("read at %v: %+v %v, want its own read %+v", ts, got, err, want)
		}
	}
	got, _ := p.ClosedVWAP1mAtOrBefore(ctx, pair, at)
	if _, err := p.ClosedVWAP1mCombinedBefore(ctx, pair, got.Bucket, 41); err != nil || f.before.Load() != 1 {
		t.Errorf("a baseline read with another limit was answered from the memo")
	}
}

// A set read that fails or panics leaves every key it carried unfilled, so each is read as before; never absent.
func TestPrefetchAnchors_FailedSetReadFallsThroughForEveryKey(t *testing.T) {
	for _, fault := range []string{"error", "panic"} {
		t.Run(fault, func(t *testing.T) {
			f := &fakeAnchors{fault: fault}
			p := PrefetchAnchors{AnchorReads: f}
			ctx := readset.With(context.Background(), readset.New())
			at := time.Date(2026, 11, 24, 12, 0, 30, 0, time.UTC)
			pairs := prefetchPairs(t, 4)
			failed := anchorCount("anchor_vwap_1m", "prefetch_error")
			p.PrefetchChange24hAnchors(ctx, pairs, at, at.Add(-30*time.Minute), 40)
			if d := anchorCount("anchor_vwap_1m", "prefetch_error") - failed; fault == "error" && d != 4 {
				t.Errorf("prefetch_error = %v, want one per key (4)", d)
			}
			for _, pair := range pairs {
				want, wantErr := f.anchor(pair, at)
				got, err := p.ClosedVWAP1mAtOrBefore(ctx, pair, at)
				if !reflect.DeepEqual(got, want) || (err == nil) != (wantErr == nil) {
					t.Errorf("%v: got %+v %v, want %+v %v", pair, got, err, want, wantErr)
				}
			}
			if f.single.Load() != 4 {
				t.Errorf("per-key reads = %d, want 4", f.single.Load())
			}
		})
	}
}

// An anchor older than notBefore is refused by the reader before its baseline is asked, so none is prefetched.
func TestPrefetchAnchors_NoBaselineForAnAnchorTooOld(t *testing.T) {
	f := &fakeAnchors{}
	ctx := readset.With(context.Background(), readset.New())
	at := time.Date(2026, 11, 24, 12, 0, 30, 0, time.UTC)
	PrefetchAnchors{AnchorReads: f}.PrefetchChange24hAnchors(ctx, prefetchPairs(t, 2), at, at, 40)
	if got := f.sets.Load(); got != 1 {
		t.Errorf("set statements = %d, want the anchor statement only", got)
	}
}
