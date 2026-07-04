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
			s := &Server{lakeWatermarkReader: &stubWatermark{ledger: 100, closedAt: time.Now().Add(-tc.lag)}}
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
	s := &Server{lakeWatermarkReader: &stubWatermark{ledger: 0, closedAt: time.Time{}}}
	if _, _, ok := s.lakeWatermark(context.Background()); ok {
		t.Fatal("ledger 0 (empty lake) should report no watermark")
	}
}

// TestLakeWatermark_CachedWithinTTL: the reader runs once per TTL window,
// not per request — the whole point of the cached getter (do NOT
// ContiguousWatermark/max() the lake per request).
func TestLakeWatermark_CachedWithinTTL(t *testing.T) {
	wm := &stubWatermark{ledger: 100, closedAt: time.Now()}
	s := &Server{lakeWatermarkReader: wm}
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
	s := &Server{lakeWatermarkReader: wm, logger: slog.Default()}
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
