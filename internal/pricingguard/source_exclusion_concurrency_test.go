package pricingguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// slowExclusions counts reads and, when gate is non-nil, blocks each read until it is closed.
type slowExclusions struct {
	mu    sync.Mutex
	err   error
	reads int
	gate  chan struct{}
}

func (s *slowExclusions) UnliftedSourceExclusions(ctx context.Context) ([]timescale.SourceExclusion, error) {
	s.mu.Lock()
	s.reads++
	gate, err := s.gate, s.err
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func (s *slowExclusions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *slowExclusions) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.gate = err, nil
}

type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syncClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func concurrentVerdicts(t *testing.T, g *SourceExclusionGate, n int) map[Withholding]int {
	t.Helper()
	base, quote := exclPair(t)
	row := timescale.Vwap1mRow{Bucket: exclBucket, Sources: []string{"sdex"}}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got = map[Withholding]int{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := g.BucketWithholding(context.Background(), base, quote, row)
			mu.Lock()
			got[w]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return got
}

func clockedGate(s *slowExclusions) (*SourceExclusionGate, *syncClock) {
	c := &syncClock{t: exclBucket}
	g := NewSourceExclusionGate(s, SourceExclusionGateOptions{})
	g.now = c.now
	return g, c
}

func TestSourceExclusionGate_ConcurrentFirstReadIsOneQuery(t *testing.T) {
	s := &slowExclusions{gate: make(chan struct{})}
	g, _ := clockedGate(s)
	go func() {
		for s.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // let the other callers queue behind the slow read
		close(s.gate)
	}()
	got := concurrentVerdicts(t, g, 32)
	if s.count() != 1 {
		t.Fatalf("32 concurrent first reads made %d store calls, want 1", s.count())
	}
	if got[NotWithheld] != 32 {
		t.Fatalf("verdicts %v, want every caller to see the one successful read", got)
	}
}

func TestSourceExclusionGate_SlowRefreshDoesNotBlockOtherCallers(t *testing.T) {
	s := &slowExclusions{}
	g, c := clockedGate(s)
	concurrentVerdicts(t, g, 1)
	block := make(chan struct{})
	s.mu.Lock()
	s.gate = block
	s.mu.Unlock()
	c.advance(exclusionCacheTTL + time.Second)

	leader := make(chan map[Withholding]int)
	go func() { leader <- concurrentVerdicts(t, g, 1) }()
	for s.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	others := make(chan map[Withholding]int)
	go func() { others <- concurrentVerdicts(t, g, 16) }()
	select {
	case got := <-others:
		if got[NotWithheld] != 16 {
			t.Fatalf("callers during a slow refresh: %v, want the last good read", got)
		}
	case <-time.After(time.Second):
		t.Fatal("callers blocked behind a slow refresh instead of serving the last good read")
	}
	close(block)
	<-leader
	if s.count() != 2 {
		t.Fatalf("%d store calls, want 2", s.count())
	}
}

func TestSourceExclusionGate_FailingStoreInsideGraceQueriedOncePerInterval(t *testing.T) {
	s := &slowExclusions{}
	g, c := clockedGate(s)
	concurrentVerdicts(t, g, 1)
	s.fail(errors.New("db down"))
	c.advance(exclusionCacheTTL + time.Second)

	if got := concurrentVerdicts(t, g, 50); got[NotWithheld] != 50 {
		t.Fatalf("inside the grace: %v, want the last good read for every caller", got)
	}
	if got := concurrentVerdicts(t, g, 50); got[NotWithheld] != 50 {
		t.Fatalf("inside the grace, second wave: %v", got)
	}
	if s.count() != 2 {
		t.Fatalf("100 calls inside one retry interval made %d store calls, want 2 (the good read and one retry)", s.count())
	}
	c.advance(exclusionRetryInterval)
	concurrentVerdicts(t, g, 50)
	if s.count() != 3 {
		t.Fatalf("after one retry interval: %d store calls, want 3", s.count())
	}
}

func TestSourceExclusionGate_PastGraceWithholdsWithoutQueryPerCall(t *testing.T) {
	s := &slowExclusions{}
	g, c := clockedGate(s)
	concurrentVerdicts(t, g, 1)
	s.fail(errors.New("db down"))
	c.advance(exclusionMaxStale + time.Second)

	if got := concurrentVerdicts(t, g, 100); got[WithheldExclusionsUnread] != 100 {
		t.Fatalf("past the grace: %v, want every call withheld", got)
	}
	if got := concurrentVerdicts(t, g, 100); got[WithheldExclusionsUnread] != 100 {
		t.Fatalf("past the grace, second wave: %v", got)
	}
	if s.count() != 2 {
		t.Fatalf("200 calls past the grace made %d store calls, want 2", s.count())
	}
}

func TestSourceExclusionGate_RefreshOutlivesTheCallerContext(t *testing.T) {
	s := &slowExclusions{gate: make(chan struct{})}
	g, _ := clockedGate(s)
	base, quote := exclPair(t)
	row := timescale.Vwap1mRow{Bucket: exclBucket, Sources: []string{"sdex"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := g.BucketWithholding(ctx, base, quote, row); got != WithheldExclusionsUnread {
		t.Fatalf("cancelled first caller: %q, want unread", got)
	}
	close(s.gate)
	if got := concurrentVerdicts(t, g, 4); got[NotWithheld] != 4 || s.count() != 1 {
		t.Fatalf("after the detached read finished: %v with %d store calls, want its result reused", got, s.count())
	}
}
