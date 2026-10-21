package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type countingChange24h struct {
	calls atomic.Int32
	err   error
	gate  chan struct{}
}

func (c *countingChange24h) USDPrice24hAgo(_ context.Context, a canonical.Asset) (string, error) {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	if c.err != nil {
		return "", c.err
	}
	return "anchor:" + a.String(), nil
}

func TestMinuteChange24hReader_OneReadPerAssetPerMinute(t *testing.T) {
	next := &countingChange24h{}
	r := newMinuteChange24hReader(next)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if v, err := r.USDPrice24hAgo(ctx, canonical.NativeAsset()); err != nil || v != "anchor:native" {
			t.Fatalf("got %q, %v", v, err)
		}
	}
	if n := next.calls.Load(); n != 1 {
		t.Fatalf("same asset, same minute: %d reads, want 1", n)
	}
	now = now.Add(50 * time.Second) // 03:04:55, still minute 03:04
	_, _ = r.USDPrice24hAgo(ctx, canonical.NativeAsset())
	if n := next.calls.Load(); n != 1 {
		t.Fatalf("within the minute: %d reads, want 1", n)
	}
	now = now.Add(10 * time.Second) // 03:05:05
	_, _ = r.USDPrice24hAgo(ctx, canonical.NativeAsset())
	if n := next.calls.Load(); n != 2 {
		t.Fatalf("next minute: %d reads, want 2", n)
	}
}

func TestMinuteChange24hReader_ErrorsAreNotCached(t *testing.T) {
	next := &countingChange24h{err: errors.New("boom")}
	r := newMinuteChange24hReader(next)
	r.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	for range 2 {
		if _, err := r.USDPrice24hAgo(context.Background(), canonical.NativeAsset()); err == nil {
			t.Fatal("want the inner error")
		}
	}
	if n := next.calls.Load(); n != 2 {
		t.Fatalf("%d reads, want 2 (errors re-read)", n)
	}
}

func TestMinuteChange24hReader_ConcurrentMissesShareOneRead(t *testing.T) {
	next := &countingChange24h{gate: make(chan struct{})}
	r := newMinuteChange24hReader(next)
	r.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := r.USDPrice24hAgo(context.Background(), canonical.NativeAsset()); err != nil || v != "anchor:native" {
				t.Errorf("got %q, %v", v, err)
			}
		}()
	}
	for next.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(next.gate)
	wg.Wait()
	if n := next.calls.Load(); n != 1 {
		t.Fatalf("%d reads for 10 concurrent misses, want 1", n)
	}
}

func TestMinuteChange24hReader_WaiterHonoursItsOwnContext(t *testing.T) {
	next := &countingChange24h{gate: make(chan struct{})}
	defer close(next.gate)
	r := newMinuteChange24hReader(next)
	r.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.USDPrice24hAgo(ctx, canonical.NativeAsset()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
