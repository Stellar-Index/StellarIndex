package v1

import (
	"context"
	"errors"
	"testing"
	"time"
)

type scriptedPoolTokens struct {
	calls int
	resp  map[string][]string
	err   error
}

func (s *scriptedPoolTokens) PoolTokens(context.Context, string) (map[string][]string, error) {
	s.calls++
	return s.resp, s.err
}

func TestPoolTokensCache_RefillsAfterTTLAndKeepsLastGoodOnError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	up := &scriptedPoolTokens{resp: map[string][]string{"CPOOL1": {"CA", "CB"}}}
	c := newPoolTokensCache(up)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		m, err := c.PoolTokens(ctx, "comet")
		if err != nil || len(m["CPOOL1"]) != 2 {
			t.Fatalf("PoolTokens = %v, %v", m, err)
		}
	}
	if up.calls != 1 {
		t.Fatalf("upstream calls inside the TTL = %d, want 1", up.calls)
	}

	now = now.Add(poolTokensTTL)
	up.resp = map[string][]string{"CPOOL1": {"CA", "CB"}, "CPOOL2": {"CC", "CD"}}
	if m, _ := c.PoolTokens(ctx, "comet"); len(m) != 2 || up.calls != 2 {
		t.Fatalf("after TTL: map len %d, upstream calls %d; want a refill to 2 pools in 2 calls", len(m), up.calls)
	}

	now = now.Add(poolTokensTTL)
	up.err = errors.New("served tier down")
	m, err := c.PoolTokens(ctx, "comet")
	if err != nil || len(m) != 2 {
		t.Fatalf("failed refill = %v, %v; want the last-good 2-pool map and no error", m, err)
	}

	if _, err := c.PoolTokens(ctx, "blend"); err == nil {
		t.Fatal("a failed first fill must surface its error, not an empty map")
	}
}

// Two concurrent readers of one source can interleave so the second misses
// the entry, then starts its flight only after the first flight stored it
// and left the group. That late flight must not re-read upstream.
func TestPoolTokensCache_LateFlightReusesFreshEntry(t *testing.T) {
	t.Parallel()
	up := &scriptedPoolTokens{resp: map[string][]string{"CPOOL1": {"CA", "CB"}}}
	c := newPoolTokensCache(up)
	if _, err := c.PoolTokens(context.Background(), "blend"); err != nil {
		t.Fatalf("first read: %v", err)
	}

	v, err := c.fill("blend")
	if m, _ := v.(map[string][]string); err != nil || len(m["CPOOL1"]) != 2 {
		t.Fatalf("late fill = %v, %v; want the stored map", v, err)
	}
	if up.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1: a flight that starts after a fresh store must reuse it", up.calls)
	}
}

type panickingPoolTokens struct{}

func (panickingPoolTokens) PoolTokens(context.Context, string) (map[string][]string, error) {
	panic("upstream bug")
}

// singleflight re-raises an unrecovered fill panic on its own goroutine,
// which would take the whole API process down instead of failing one read.
func TestPoolTokensCache_FillPanicIsAnErrorNotACrash(t *testing.T) {
	t.Parallel()
	c := newPoolTokensCache(panickingPoolTokens{})
	m, err := c.PoolTokens(context.Background(), "blend")
	if !errors.Is(err, errPoolTokensFillPanicked) || m != nil {
		t.Fatalf("PoolTokens = %v, %v; want nil, errPoolTokensFillPanicked", m, err)
	}
}
