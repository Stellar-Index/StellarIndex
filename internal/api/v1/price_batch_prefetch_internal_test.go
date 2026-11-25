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

var pfiSeam = readset.NewPairSeam[string]("pf_internal", nil)

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

func (r *pfiReader) memoReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.memo {
		if m {
			n++
		}
	}
	return n
}

func newPfiReader(price string) *pfiReader {
	return &pfiReader{entered: make(chan struct{}, 64), release: make(chan struct{}), price: price}
}

var pfiBTC = canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}

type pfiFill int

const (
	pfiNone  pfiFill = iota
	pfiFound         // pfiBTC/USD memoised at "1.0"
	pfiMiss          // pfiBTC/USD memoised as a definite miss
)

// pfiMemo is a memo whose clock is now, filled as fill says.
func pfiMemo(fill pfiFill, now *atomic.Int64) context.Context {
	m := readset.NewWithClock(func() time.Time { return time.Unix(0, now.Load()) })
	ctx := readset.With(context.Background(), m)
	if fill != pfiNone {
		pfiSeam.Fill(ctx, readset.Pair(pfiBTC, defaultPriceQuote, 0), "1.0", fill == pfiFound)
	}
	return ctx
}

// A follower that joins another request's row flight is answered from that request's prefetch only while it is at most
// readset.MaxAge old; past that, the read falls through to the store.
func TestBatchRowFlight_FollowerSeesAtMostMaxAgeOldPrefetch(t *testing.T) {
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
			leaderCtx := pfiMemo(pfiFound, &now)

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

// Concurrent alias reads of one key coalesce on the shared flight, which carries no memo, unless the memo still holds
// a found bucket for it: then each is answered from the memo alone, and a single /v1/price caller beside them still
// gets the flight. A memoised miss coalesces too, since the miss path is the slow one.
func TestReadPriceWithAliases_CoalescesUnlessTheMemoHoldsTheRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fill      pfiFill
		age       time.Duration
		wantStore int32  // store reads made
		wantWalks int    // LatestPrice calls: one per flight, one per memo-answered row
		wantPrice string // the memo-carrying callers' answer
	}{
		{"not prefetched", pfiNone, 0, 1, 1, "2.0"},
		{"prefetched", pfiFound, 0, 1, 17, "1.0"}, // the store read is the single caller's own flight
		{"prefetched miss", pfiMiss, 0, 1, 1, "2.0"},
		{"prefetch expired", pfiFound, readset.MaxAge + time.Millisecond, 1, 1, "2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newPfiReader("2.0")
			s := New(Options{Prices: reader})
			var now atomic.Int64
			t0 := time.Now()
			now.Store(t0.Add(-tc.age).UnixNano())
			ctx := pfiMemo(tc.fill, &now)
			now.Store(t0.UnixNano())

			const rows = 16
			got := make([]string, rows)
			var wg sync.WaitGroup
			wg.Add(rows + 1)
			go func() {
				defer wg.Done()
				_, _, _, _, _ = s.readPriceWithAliasesServed(context.Background(), reader, pfiBTC, defaultPriceQuote)
			}()
			for i := range rows {
				go func() {
					defer wg.Done()
					snap, _, _, _, _ := s.readPriceWithAliasesServed(ctx, reader, pfiBTC, defaultPriceQuote)
					got[i] = snap.Price
				}()
			}
			time.Sleep(100 * time.Millisecond)
			close(reader.release)
			wg.Wait()

			if n := reader.calls.Load(); n != tc.wantStore {
				t.Errorf("store reads = %d, want %d", n, tc.wantStore)
			}
			if n := len(reader.entered); n != tc.wantWalks {
				t.Errorf("alias walks = %d, want %d", n, tc.wantWalks)
			}
			if n := reader.memoReads(); n != 0 {
				t.Errorf("%d store reads carried a memo: a shared flight saw it", n)
			}
			for i, p := range got {
				if p != tc.wantPrice {
					t.Errorf("row %d = %q, want %q", i, p, tc.wantPrice)
				}
			}
		})
	}
}
