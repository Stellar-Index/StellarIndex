package v1

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// TestLakeWatermark_ThresholdPin pins the lakeStaleThreshold semantics at
// the seam: stale flips exactly when the watermark's close time trails now
// by more than the threshold (±30s margins keep the pin robust on slow CI).
func TestLakeWatermark_ThresholdPin(t *testing.T) {
	cases := []struct {
		name      string
		lag       time.Duration
		wantStale bool
	}{
		{"fresh capture", 10 * time.Second, false},
		{"just inside threshold", lakeStaleThreshold - 30*time.Second, false},
		{"just beyond threshold", lakeStaleThreshold + 30*time.Second, true},
		{"long-wedged sink", time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{LakeWatermark: &stubWatermark{ledger: 100, closedAt: time.Now().Add(-tc.lag)}}}
			ledger, stale, ok := s.lakeWatermark(context.Background())
			if !ok || ledger != 100 {
				t.Fatalf("watermark = (%d, ok=%v), want (100, true)", ledger, ok)
			}
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v at lag %s", stale, tc.wantStale, tc.lag)
			}
		})
	}
}

func TestLakeWatermark_NilReader(t *testing.T) {
	s := &Server{}
	if _, _, ok := s.lakeWatermark(context.Background()); ok {
		t.Fatal("nil reader should report no watermark")
	}
}

func TestLakeWatermark_EmptyLake(t *testing.T) {
	s := &Server{Options: Options{LakeWatermark: &stubWatermark{ledger: 0, closedAt: time.Time{}}}}
	if _, _, ok := s.lakeWatermark(context.Background()); ok {
		t.Fatal("ledger 0 (empty lake) should report no watermark")
	}
}

// TestLakeWatermark_CachedWithinTTL: the reader runs once per TTL window,
// not per request — the whole point of the cached getter (do NOT
// ContiguousWatermark/max() the lake per request).
func TestLakeWatermark_CachedWithinTTL(t *testing.T) {
	wm := &stubWatermark{ledger: 100, closedAt: time.Now()}
	s := &Server{Options: Options{LakeWatermark: wm}}
	for i := 0; i < 5; i++ {
		if _, _, ok := s.lakeWatermark(context.Background()); !ok {
			t.Fatalf("call %d: watermark unexpectedly missing", i)
		}
	}
	if wm.calls != 1 {
		t.Fatalf("reader calls = %d, want 1 (cached within TTL)", wm.calls)
	}
}

// TestLakeWatermark_ServesPreviousOnRefreshError: a failed refresh keeps
// serving the last-good watermark (whose growing age still yields correct
// stale semantics) instead of dropping the field.
func TestLakeWatermark_ServesPreviousOnRefreshError(t *testing.T) {
	wm := &stubWatermark{ledger: 100, closedAt: time.Now()}
	s := &Server{Options: Options{LakeWatermark: wm}, logger: slog.Default()}
	if _, _, ok := s.lakeWatermark(context.Background()); !ok {
		t.Fatal("first read should succeed")
	}
	// Force an expired cache + an erroring reader.
	s.lakeWMFetched = time.Now().Add(-2 * lakeWatermarkTTL)
	wm.err = errors.New("lake down")
	ledger, _, ok := s.lakeWatermark(context.Background())
	if !ok || ledger != 100 {
		t.Fatalf("watermark after failed refresh = (%d, ok=%v), want previous (100, true)", ledger, ok)
	}
}

// TestLakeWatermark_SlowLakeDoesNotSerialiseCallers. The watermark
// refresh must never run under the process-global lakeWMMu: every lake-backed
// route calls lakeWatermark (pools_reserves, asset_supply ×3,
// liquidity_pools ×2, lending, plus the three explorer account-state sites),
// so a single slow ClickHouse round-trip inside the critical section makes
// every concurrent request queue behind it on a non-context-aware mutex and
// burn its own deadline. Each caller must come back within ITS OWN budget,
// and the burst must coalesce onto one read.
func TestLakeWatermark_SlowLakeDoesNotSerialiseCallers(t *testing.T) {
	const (
		callers   = 8
		lakeDelay = 1200 * time.Millisecond
		budget    = 300 * time.Millisecond
		slack     = 400 * time.Millisecond
	)
	wm := &slowWatermark{delay: lakeDelay, ledger: 100, closedAt: time.Now()}
	s := &Server{Options: Options{LakeWatermark: wm}, logger: slog.Default()}

	returned := make(chan time.Duration, callers)
	start := time.Now()
	for i := 0; i < callers; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			s.lakeWatermark(ctx)
			returned <- time.Since(start)
		}()
	}

	overall := time.After(budget + slack)
	for i := 0; i < callers; i++ {
		select {
		case d := <-returned:
			if d > budget+slack {
				t.Errorf("caller returned after %s, past its %s deadline — the watermark refresh is running on the request path", d, budget)
			}
		case <-overall:
			t.Fatalf("only %d of %d concurrent callers returned within %s while one lake read was in flight — requests are serialised behind the watermark lock (RLT-095)",
				i, callers, budget+slack)
		}
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads for %d concurrent cold callers = %d, want 1 (the refresh must be coalesced, not repeated per caller)", callers, got)
	}
}

// TestLakeWatermark_ColdFailureIsRateLimited. A failed read must
// stamp a retry gap. Without one, every subsequent request re-enters the
// read — against a wedged lake (whose read is bounded only by ClickHouse's
// 30s ReadTimeout) that is a back-to-back retry train, one per request.
func TestLakeWatermark_ColdFailureIsRateLimited(t *testing.T) {
	wm := &slowWatermark{err: errors.New("lake down")}
	s := &Server{Options: Options{LakeWatermark: wm}, logger: slog.Default()}

	for i := 0; i < 4; i++ {
		if _, _, ok := s.lakeWatermark(context.Background()); ok {
			t.Fatalf("call %d reported a watermark though every read failed", i)
		}
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads across 4 requests after a failed read = %d, want 1 (a failure must back off for the retry gap, not retry per request)", got)
	}
}

// TestLakeWatermark_LapsedEntryServedWithoutWaitingOnTheLake. Once
// the TTL lapses the cached watermark is still perfectly serviceable — its
// close time only gets older, which is exactly what flags.stale reads — so
// the caller that happens to notice the lapse must be served from cache
// immediately while the refresh runs detached, and a failed refresh must
// neither drop the last-good value nor re-arm an immediate retry.
func TestLakeWatermark_LapsedEntryServedWithoutWaitingOnTheLake(t *testing.T) {
	wm := &slowWatermark{delay: 400 * time.Millisecond, err: errors.New("lake down")}
	s := &Server{Options: Options{LakeWatermark: wm}, logger: slog.Default()}
	// The state every request lands in once the TTL window lapses.
	s.lakeWMLedger, s.lakeWMClosedAt = 100, time.Now()
	s.lakeWMFetched = time.Now().Add(-2 * lakeWatermarkTTL)

	start := time.Now()
	ledger, stale, ok := s.lakeWatermark(context.Background())
	elapsed := time.Since(start)
	if !ok || ledger != 100 || stale {
		t.Fatalf("lapsed watermark = (%d, stale=%v, ok=%v), want (100, false, true)", ledger, stale, ok)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("serving a lapsed watermark took %s while the lake read was slow — the refresh must not run on the caller's path (RLT-095)", elapsed)
	}

	// Let the detached refresh finish (and fail).
	deadline := time.Now().Add(3 * time.Second)
	for wm.finishedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if wm.finishedCount() == 0 {
		t.Fatal("the detached refresh never ran")
	}
	if _, _, ok := s.lakeWatermark(context.Background()); !ok {
		t.Fatal("last-good watermark dropped after a failed refresh")
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads = %d, want 1 — a failed refresh must back off for the retry gap", got)
	}
}

// TestLakeWatermark_UnmeasuredFailsClosed: every lake-backed route discards ok
// and serves flags.stale straight from lakeWatermark, so a wired reader with
// no measurement must report stale=true — "unknown" served as fresh is the
// defect. Only an unwired reader (no lake to judge) stays stale=false.
func TestLakeWatermark_UnmeasuredFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		wm        LakeWatermarkReader
		wantStale bool
	}{
		{"cold read failed", &slowWatermark{err: errors.New("lake down")}, true},
		{"lake empty", &slowWatermark{}, true},
		{"no reader wired", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{LakeWatermark: tc.wm}, logger: slog.Default()}
			ledger, stale, ok := s.lakeWatermark(context.Background())
			if ok || ledger != 0 {
				t.Fatalf("watermark = (%d, ok=%v), want (0, false) with no measurement", ledger, ok)
			}
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}
