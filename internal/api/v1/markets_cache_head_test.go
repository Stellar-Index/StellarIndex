package v1

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pagingPoolsReader serves a fixed volume-ordered listing the way the store pages it, recording each fetched limit.
type pagingPoolsReader struct {
	fakeMarketsReader
	mu     sync.Mutex
	limits []int
	pools  []Pool
	pairs  []Market
}

func newPagingPoolsReader(n int) *pagingPoolsReader {
	r := &pagingPoolsReader{}
	for i := range n {
		vol := fmt.Sprintf("%d", 10_000-i)
		r.pools = append(r.pools, Pool{Source: "soroswap", Base: fmt.Sprintf("A%03d", i), Quote: "native", Volume24hUSD: &vol})
		r.pairs = append(r.pairs, Market{Base: fmt.Sprintf("A%03d", i), Quote: "native", Volume24hUSD: &vol})
	}
	return r
}

func page[T any](rows []T, limit int, cursorOf func(T) string) ([]T, string) {
	if len(rows) <= limit {
		return append([]T(nil), rows...), ""
	}
	return append([]T(nil), rows[:limit]...), cursorOf(rows[limit-1])
}

func (r *pagingPoolsReader) AllPools(_ context.Context, _ timescale.PoolsFilter, _ string, limit int, order timescale.MarketsOrder) ([]Pool, string, error) {
	r.mu.Lock()
	r.limits = append(r.limits, limit)
	r.mu.Unlock()
	rows, next := page(r.pools, limit, func(p Pool) string {
		return timescale.PageCursor(order, p.Source+"|"+p.Base+"|"+p.Quote, p.Volume24hUSD)
	})
	return rows, next, nil
}

func (r *pagingPoolsReader) DistinctPairsExt(_ context.Context, _ string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	r.mu.Lock()
	r.limits = append(r.limits, limit)
	r.mu.Unlock()
	rows, next := page(r.pairs, limit, func(m Market) string {
		return timescale.PageCursor(order, m.Base+"|"+m.Quote, m.Volume24hUSD)
	})
	return rows, next, nil
}

func (r *pagingPoolsReader) fetched() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.limits...)
}

// A first page sliced from the head slot must equal, row for row and cursor for cursor, what a direct fetch at that limit
// returns; otherwise page two skips or repeats rows.
func TestCachedMarketsReader_HeadSlotSlicesMatchDirectPages(t *testing.T) {
	ctx := context.Background()
	order := timescale.MarketsOrderVolume24hDesc
	up := newPagingPoolsReader(250)
	c := NewCachedMarketsReader(up, time.Minute)
	direct := newPagingPoolsReader(250)
	filter := timescale.PoolsFilter{Sources: []string{"soroswap"}}
	for _, limit := range []int{1, 3, 8, 100, 199, 200} {
		gotP, gotPNext, err := c.AllPools(ctx, filter, "", limit, order)
		if err != nil {
			t.Fatal(err)
		}
		wantP, wantPNext, _ := direct.AllPools(ctx, filter, "", limit, order)
		if !reflect.DeepEqual(gotP, wantP) || gotPNext != wantPNext {
			t.Errorf("AllPools limit=%d: got %d rows next %q, want %d rows next %q", limit, len(gotP), gotPNext, len(wantP), wantPNext)
		}
		gotM, gotMNext, err := c.DistinctPairsExt(ctx, "", limit, order)
		if err != nil {
			t.Fatal(err)
		}
		wantM, wantMNext, _ := direct.DistinctPairsExt(ctx, "", limit, order)
		if !reflect.DeepEqual(gotM, wantM) || gotMNext != wantMNext {
			t.Errorf("DistinctPairsExt limit=%d: got %d rows next %q, want %d rows next %q", limit, len(gotM), gotMNext, len(wantM), wantMNext)
		}
	}
	if got := up.fetched(); !reflect.DeepEqual(got, []int{200, 200}) {
		t.Errorf("upstream fetched limits %v, want one head-slot scan per method [200 200]", got)
	}
}

// Past the head limit, and on later pages, the caller's own limit is fetched and keyed.
func TestCachedMarketsReader_HeadSlotPassesThroughLargeAndLaterPages(t *testing.T) {
	ctx := context.Background()
	up := newPagingPoolsReader(10)
	c := NewCachedMarketsReader(up, time.Minute)
	if _, _, err := c.AllPools(ctx, timescale.PoolsFilter{}, "", 300, timescale.MarketsOrderPair); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.AllPools(ctx, timescale.PoolsFilter{}, "soroswap|A004|native", 5, timescale.MarketsOrderPair); err != nil {
		t.Fatal(err)
	}
	if got := up.fetched(); !reflect.DeepEqual(got, []int{300, 5}) {
		t.Errorf("upstream fetched limits %v, want [300 5]", got)
	}
}
