package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// swrTestClock is a settable clock shared by a cache and its detached fills.
type swrTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *swrTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *swrTestClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// gatedMarketsUpstream serves one market, one pool and an empty sparkline batch; hold parks every read until release, which
// decides whether the parked reads fail.
type gatedMarketsUpstream struct {
	mu         sync.Mutex
	gate       chan struct{}
	fail       atomic.Bool
	sparkCalls atomic.Int64
}

func (u *gatedMarketsUpstream) hold() {
	u.mu.Lock()
	u.gate = make(chan struct{})
	u.mu.Unlock()
}

func (u *gatedMarketsUpstream) release(fail bool) {
	u.fail.Store(fail)
	u.mu.Lock()
	close(u.gate)
	u.gate = nil
	u.mu.Unlock()
}

func (u *gatedMarketsUpstream) wait(ctx context.Context) error {
	u.mu.Lock()
	g := u.gate
	u.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if u.fail.Load() {
		return errors.New("upstream down")
	}
	return nil
}

func (u *gatedMarketsUpstream) DistinctPairsExt(ctx context.Context, _ string, _ int, _ timescale.MarketsOrder) ([]Market, string, error) {
	if err := u.wait(ctx); err != nil {
		return nil, "", err
	}
	return []Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 7}}, "", nil
}

func (u *gatedMarketsUpstream) SourceMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (u *gatedMarketsUpstream) AssetMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (u *gatedMarketsUpstream) AllPools(ctx context.Context, _ timescale.PoolsFilter, _ string, _ int, _ timescale.MarketsOrder) ([]Pool, string, error) {
	if err := u.wait(ctx); err != nil {
		return nil, "", err
	}
	return []Pool{{Source: "aquarius", Base: "native", Quote: "fiat:USD"}}, "", nil
}

func (u *gatedMarketsUpstream) PairMarket(context.Context, canonical.Asset, canonical.Asset) (Market, bool, error) {
	return Market{}, false, nil
}

func (u *gatedMarketsUpstream) GetPairsVolumeHistory24hBatch(ctx context.Context, _ [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	u.sparkCalls.Add(1)
	if err := u.wait(ctx); err != nil {
		return nil, err
	}
	return map[string][]timescale.PairVolumePoint{}, nil
}

func (u *gatedMarketsUpstream) FirstTradeBatch(context.Context, [][2]string) (map[string]time.Time, error) {
	return nil, nil
}

// waitMarketsIdle blocks until no page or sparkline fill is in flight, so the next request sees the settled outcome.
func waitMarketsIdle(t *testing.T, c *CachedMarketsReader) {
	t.Helper()
	busy := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, e := range c.entries {
			if e.flight != nil {
				return true
			}
		}
		c.sparks.mu.Lock()
		defer c.sparks.mu.Unlock()
		for _, e := range c.sparks.entries {
			if e.flight != nil {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); busy(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("cache fills still in flight after 5s")
		}
	}
}

type swrFlagsServe struct {
	stale, noStore bool
	asOf           time.Time
}

func getSWRFlags(t *testing.T, h http.Handler, path string) swrFlagsServe {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		AsOf  time.Time `json:"as_of"`
		Data  []any     `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("GET %s served %d rows, want the cached 1", path, len(body.Data))
	}
	// Degraded never reaches the wire; it is the no-store header.
	return swrFlagsServe{
		stale: body.Flags.Stale, asOf: body.AsOf,
		noStore: strings.Contains(rec.Header().Get("Cache-Control"), "no-store"),
	}
}

// A serve past the TTL that starts a healthy refresh is clean and cacheable; one whose last refresh failed is stale and
// no-store; one older than ttl+min(ttl, budget) whose refresh it starts is stale but cacheable; after a successful refresh
// the next serve is clean again.
func TestMarketsSWR_FlagsOnlyLateOrFailedRefresh(t *testing.T) {
	const ttl = 30 * time.Second
	for _, path := range []string{"/v1/markets?include=sparkline", "/v1/pools"} {
		t.Run(path, func(t *testing.T) {
			clk := &swrTestClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
			up := &gatedMarketsUpstream{}
			cache := NewCachedMarketsReader(up, ttl)
			cache.now, cache.sparks.now = clk.now, clk.now
			h := New(Options{Markets: cache}).Handler()
			want := func(step string, got swrFlagsServe, stale bool) {
				t.Helper()
				if got.stale != stale || got.noStore != stale {
					t.Errorf("%s: stale=%v no-store=%v, want both %v", step, got.stale, got.noStore, stale)
				}
			}

			filled := getSWRFlags(t, h, path)
			want("cold fill", filled, false)

			clk.add(ttl + time.Second)
			up.hold()
			healthy := getSWRFlags(t, h, path)
			want("past TTL, healthy refresh in flight", healthy, false)
			if !healthy.asOf.Equal(filled.asOf) {
				t.Errorf("past-TTL serve as_of = %v, want the fill time %v", healthy.asOf, filled.asOf)
			}
			up.release(true)
			waitMarketsIdle(t, cache)

			up.hold()
			want("previous refresh failed, retry in flight", getSWRFlags(t, h, path), true)
			up.release(false)
			waitMarketsIdle(t, cache)

			refreshed := getSWRFlags(t, h, path)
			want("fresh after a successful refresh", refreshed, false)
			if !refreshed.asOf.Equal(clk.now()) {
				t.Errorf("refreshed as_of = %v, want the refresh time %v", refreshed.asOf, clk.now())
			}

			clk.add(2*ttl + time.Second)
			up.hold()
			if bound := getSWRFlags(t, h, path); !bound.stale || bound.noStore {
				t.Errorf("past the hard bound, refresh just started: stale=%v no-store=%v, want stale only", bound.stale, bound.noStore)
			}
			up.release(false)
			waitMarketsIdle(t, cache)

			if strings.Contains(path, "sparkline") && up.sparkCalls.Load() == 0 {
				t.Fatal("the sparkline batch was never read, so its cache went untested")
			}
		})
	}
}
