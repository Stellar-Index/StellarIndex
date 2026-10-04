package v1

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// gatedFill is an upstream that blocks until released and, like a real
// DB driver, fails with ctx.Err() if its ctx is cancelled first.
type gatedFill struct {
	calls     atomic.Int32
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
}

func newGatedFill() *gatedFill {
	return &gatedFill{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedFill) wait(ctx context.Context) error {
	g.calls.Add(1)
	g.startOnce.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// assertLeaderCancelSparesWaiter drives one cold key: the leader starts the
// fill on a cancellable ctx, a healthy waiter joins it, then the leader
// aborts. The leader must get its own ctx error; the waiter must get the
// upstream value from the SAME single upstream call; and the key must stay
// cached for the next caller.
func assertLeaderCancelSparesWaiter[T any](
	t *testing.T,
	g *gatedFill,
	get func(context.Context) (T, error),
	isWant func(T) bool,
) {
	t.Helper()
	const timeout = 5 * time.Second

	leaderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := get(leaderCtx)
		leaderErr <- err
	}()
	select {
	case <-g.started:
	case <-time.After(timeout):
		t.Fatal("leader never reached upstream")
	}

	type result struct {
		v   T
		err error
	}
	waiter := make(chan result, 1)
	go func() {
		v, err := get(context.Background())
		waiter <- result{v, err}
	}()
	// Let the waiter park on the flight. If it has not, the calls==1
	// assertion below still catches a second upstream call.
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader err = %v, want context.Canceled", err)
		}
	case <-time.After(timeout):
		t.Fatal("cancelled leader did not return")
	}

	close(g.release)
	select {
	case r := <-waiter:
		if r.err != nil {
			t.Fatalf("waiter err = %v; the leader's abort failed a healthy waiter", r.err)
		}
		if !isWant(r.v) {
			t.Fatalf("waiter got %+v, want the upstream value", r.v)
		}
	case <-time.After(timeout):
		t.Fatal("waiter never returned")
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (waiter must share the leader's fill)", n)
	}

	v, err := get(context.Background())
	if err != nil || !isWant(v) {
		t.Fatalf("follow-up read = %+v, %v; want the cached upstream value", v, err)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("upstream calls after follow-up = %d, want 1 (value must stay cached)", n)
	}
}

type gatedNetStats struct{ g *gatedFill }

func (u gatedNetStats) GetNetworkStats(ctx context.Context) (timescale.NetworkStats, error) {
	if err := u.g.wait(ctx); err != nil {
		return timescale.NetworkStats{}, err
	}
	return timescale.NetworkStats{MarketsCount24h: 42}, nil
}

func TestColdFill_NetworkStats_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedNetworkStatsReader(gatedNetStats{g}, time.Minute)
	assertLeaderCancelSparesWaiter(t, g, c.GetNetworkStats,
		func(v timescale.NetworkStats) bool { return v.MarketsCount24h == 42 })
}

func TestColdFill_MarketsPairs_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedMarketsReader(nil, time.Minute)
	upstream := func(ctx context.Context) ([]Market, string, error) {
		if err := g.wait(ctx); err != nil {
			return nil, "", err
		}
		return []Market{{Base: "native", Quote: "USDC"}}, "next", nil
	}
	get := func(ctx context.Context) ([]Market, error) {
		rows, cursor, _, _, err := c.fetchPairs(ctx, "distinct_pairs", "k", upstream)
		if err == nil && cursor != "next" {
			return nil, errors.New("cursor lost")
		}
		return rows, err
	}
	assertLeaderCancelSparesWaiter(t, g, get,
		func(v []Market) bool { return len(v) == 1 && v[0].Base == "native" })
}

func TestColdFill_MarketsPools_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedMarketsReader(nil, time.Minute)
	upstream := func(ctx context.Context) ([]Pool, string, error) {
		if err := g.wait(ctx); err != nil {
			return nil, "", err
		}
		return []Pool{{Source: "soroswap"}}, "", nil
	}
	get := func(ctx context.Context) ([]Pool, error) {
		rows, _, _, err := c.fetchPools(ctx, "all_pools", "k", upstream)
		return rows, err
	}
	assertLeaderCancelSparesWaiter(t, g, get,
		func(v []Pool) bool { return len(v) == 1 && v[0].Source == "soroswap" })
}

func TestColdFill_Issuers_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedIssuersReader(nil, time.Minute)
	upstream := func(ctx context.Context) ([]timescale.IssuerSummary, error) {
		if err := g.wait(ctx); err != nil {
			return nil, err
		}
		return []timescale.IssuerSummary{{GStrkey: "GISSUER"}}, nil
	}
	get := func(ctx context.Context) ([]timescale.IssuerSummary, error) {
		return c.fetchList(ctx, "k", upstream)
	}
	assertLeaderCancelSparesWaiter(t, g, get,
		func(v []timescale.IssuerSummary) bool { return len(v) == 1 && v[0].GStrkey == "GISSUER" })
}

func TestColdFill_AssetsRows_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedAssetsReader(nil, time.Minute)
	upstream := func(ctx context.Context) ([]timescale.AssetRow, error) {
		if err := g.wait(ctx); err != nil {
			return nil, err
		}
		return []timescale.AssetRow{{Slug: "xlm"}}, nil
	}
	get := func(ctx context.Context) ([]timescale.AssetRow, error) {
		return c.fetchRows(ctx, "list", "k", upstream)
	}
	assertLeaderCancelSparesWaiter(t, g, get,
		func(v []timescale.AssetRow) bool { return len(v) == 1 && v[0].Slug == "xlm" })
}

func TestColdFill_AssetsHistoryMap_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedAssetsReader(nil, time.Minute)
	upstream := func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		if err := g.wait(ctx); err != nil {
			return nil, err
		}
		return map[string][]timescale.AssetPricePoint{"xlm": {{T: "t0"}}}, nil
	}
	get := func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		return c.fetchHistoryMap(ctx, "history", "k", upstream)
	}
	assertLeaderCancelSparesWaiter(t, g, get,
		func(v map[string][]timescale.AssetPricePoint) bool {
			return len(v["xlm"]) == 1 && v["xlm"][0].T == "t0"
		})
}

func TestColdFill_AssetsSWR_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedAssetsReader(nil, time.Minute)
	upstream := func(ctx context.Context) (string, error) {
		if err := g.wait(ctx); err != nil {
			return "", err
		}
		return "detail", nil
	}
	get := func(ctx context.Context) (string, error) {
		return swr(ctx, c, "detail", "k", upstream)
	}
	assertLeaderCancelSparesWaiter(t, g, get, func(v string) bool { return v == "detail" })
}

type gatedSourcesStats struct{ g *gatedFill }

func (u gatedSourcesStats) GetSourceStats(ctx context.Context) ([]timescale.SourceStats, error) {
	if err := u.g.wait(ctx); err != nil {
		return nil, err
	}
	return []timescale.SourceStats{{Source: "sdex", TradeCount24h: 7}}, nil
}

func (u gatedSourcesStats) GetSourceVolumeHistory24h(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, errors.New("unused")
}

func (u gatedSourcesStats) GetSourceVolumeHistory7d(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, errors.New("unused")
}

func TestColdFill_SourcesStats_LeaderCancelSparesWaiter(t *testing.T) {
	g := newGatedFill()
	c := NewCachedSourcesStatsReader(gatedSourcesStats{g}, time.Minute)
	assertLeaderCancelSparesWaiter(t, g, c.GetSourceStats,
		func(v []timescale.SourceStats) bool { return len(v) == 1 && v[0].TradeCount24h == 7 })
}

// A panicking fill must reach waiters as an error, not an empty success,
// and must not wedge the key.
func TestColdFill_PanicIsAnErrorAndDoesNotWedge(t *testing.T) {
	c := NewCachedIssuersReader(nil, time.Minute)
	boom := func(context.Context) ([]timescale.IssuerSummary, error) { panic("boom") }
	if _, err := c.fetchList(context.Background(), "k", boom); !errors.Is(err, errCacheFillPanicked) {
		t.Fatalf("err = %v, want errCacheFillPanicked", err)
	}
	ok := func(context.Context) ([]timescale.IssuerSummary, error) {
		return []timescale.IssuerSummary{{GStrkey: "G"}}, nil
	}
	got, err := c.fetchList(context.Background(), "k", ok)
	if err != nil || len(got) != 1 {
		t.Fatalf("retry after panic = %+v, %v; want a fresh fill", got, err)
	}
}
