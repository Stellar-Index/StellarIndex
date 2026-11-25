package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
)

var pfiSeam = readset.NewSeam[readset.PairKey, string]("pf_internal", nil)

// pfiReader prices every pair at price through pfiSeam, holding each read until release closes.
type pfiReader struct {
	PriceReader
	entered chan struct{}
	release chan struct{}
	price   string
	calls   atomic.Int32
	mu      sync.Mutex
	memo    []bool // per store read: did ctx carry a memo
}

func (r *pfiReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (PriceSnapshot, []string, bool, error) {
	r.entered <- struct{}{}
	<-r.release
	v, _, _ := pfiSeam.Read(ctx, readset.Pair(a, q, 0), func(ctx context.Context) (string, bool, error) {
		r.calls.Add(1)
		r.mu.Lock()
		r.memo = append(r.memo, readset.From(ctx) != nil)
		r.mu.Unlock()
		return r.price, true, nil
	})
	return PriceSnapshot{AssetID: a.String(), Quote: q.String(), Price: v}, []string{"test"}, false, nil
}

func newPfiReader(price string) *pfiReader {
	return &pfiReader{entered: make(chan struct{}, 4), release: make(chan struct{}), price: price}
}

// A follower that joins another request's row flight is answered from that request's prefetch only while it is at most
// readset.MaxAge old; past that, the read falls through to the store.
func TestBatchRowFlight_FollowerSeesAtMostMaxAgeOldPrefetch(t *testing.T) {
	btc := canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"within max age", readset.MaxAge - time.Millisecond, "1.0"},
		{"past max age", readset.MaxAge + time.Millisecond, "2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newPfiReader("2.0")
			s := New(Options{Prices: reader})
			t0 := time.Now()
			var now atomic.Int64
			now.Store(t0.UnixNano())
			m := readset.NewWithClock(func() time.Time { return time.Unix(0, now.Load()) })
			leaderCtx := readset.With(context.Background(), m)
			pfiSeam.Fill(leaderCtx, readset.Pair(btc, defaultPriceQuote, 0), "1.0", true)

			rows := make([]batchRowResult, 2)
			var wg sync.WaitGroup
			run := func(i int, ctx context.Context) {
				defer wg.Done()
				r := httptest.NewRequest(http.MethodGet, "/v1/price/batch", nil).WithContext(ctx)
				rows[i] = s.resolveBatchRowShared(ctx, r, "crypto:BTC", defaultPriceQuote)
			}
			wg.Add(2)
			go run(0, leaderCtx)
			<-reader.entered
			go run(1, context.Background()) // the follower carries no memo of its own
			time.Sleep(50 * time.Millisecond)
			now.Store(t0.Add(tc.age).UnixNano())
			close(reader.release)
			wg.Wait()

			for i, row := range rows {
				if !row.ok || row.snap.Price != tc.want {
					t.Errorf("row %d = %+v, want price %s", i, row.snap, tc.want)
				}
			}
			if got := len(reader.entered); got != 0 {
				t.Errorf("%d more store reads: the follower did not join the leader's flight", got)
			}
		})
	}
}

// The coalesced single-price read never carries a memo, and a prefetched batch row never joins it.
func TestReadPriceWithAliases_SinglePathNeverSeesAMemo(t *testing.T) {
	btc := canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}
	reader := newPfiReader("2.0")
	s := New(Options{Prices: reader})
	memoCtx := readset.With(context.Background(), readset.New())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _, _, _ = s.readPriceWithAliasesServed(context.Background(), reader, btc, defaultPriceQuote)
	}()
	<-reader.entered
	go func() {
		defer wg.Done()
		_, _, _, _, _ = s.readPriceWithAliasesServed(memoCtx, reader, btc, defaultPriceQuote)
	}()
	<-reader.entered // the memo read did not join the single read's flight
	close(reader.release)
	wg.Wait()
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if got := reader.calls.Load(); got != 2 {
		t.Fatalf("store reads = %d, want 2", got)
	}
	seen := 0
	for _, withMemo := range reader.memo {
		if withMemo {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("reads carrying a memo = %d of %v, want only the batch row's", seen, reader.memo)
	}
}
