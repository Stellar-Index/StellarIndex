package readset

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type ratAnswer struct{ price *big.Rat }

func cloneRat(v ratAnswer) ratAnswer {
	if v.price != nil {
		v.price = new(big.Rat).Set(v.price)
	}
	return v
}

func asset(t *testing.T, id string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// counted is a single-key read that answers want and counts its calls.
type counted struct {
	mu    sync.Mutex
	calls int
	want  ratAnswer
	found bool
	err   error
}

func (c *counted) read(context.Context) (ratAnswer, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.want, c.found, c.err
}

func TestReadset_NoMemoIsTheSingleRead(t *testing.T) {
	seam := NewSeam[PairKey, ratAnswer]("test_nomemo", cloneRat)
	boom := errors.New("redis down")
	c := &counted{want: ratAnswer{big.NewRat(7, 3)}, found: true, err: boom}
	ctx := context.Background()
	seam.Fill(ctx, PairKey{Base: "native"}, ratAnswer{big.NewRat(1, 1)}, true) // no memo: dropped
	v, found, err := seam.Read(ctx, PairKey{Base: "native"}, c.read)
	if c.calls != 1 || !found || !errors.Is(err, boom) || v.price.Cmp(big.NewRat(7, 3)) != 0 {
		t.Fatalf("no memo must be exactly the single read: calls=%d v=%v found=%v err=%v", c.calls, v.price, found, err)
	}
}

func TestReadset_ThreeStates(t *testing.T) {
	seam := NewSeam[PairKey, ratAnswer]("test_states", cloneRat)
	ctx := With(context.Background(), New())
	present, absent, unread := PairKey{Base: "a"}, PairKey{Base: "b"}, PairKey{Base: "c"}
	seam.Fill(ctx, present, ratAnswer{big.NewRat(42, 100)}, true)
	seam.Fill(ctx, absent, ratAnswer{big.NewRat(9, 1)}, false) // a miss never carries a value

	c := &counted{want: ratAnswer{big.NewRat(5, 1)}, found: true}
	v, found, err := seam.Read(ctx, present, c.read)
	if c.calls != 0 || !found || err != nil || v.price.Cmp(big.NewRat(42, 100)) != 0 {
		t.Errorf("present: calls=%d v=%v found=%v err=%v", c.calls, v.price, found, err)
	}
	v, found, err = seam.Read(ctx, absent, c.read)
	if c.calls != 0 || found || err != nil || v.price != nil {
		t.Errorf("absent must be the miss (zero, false, nil): calls=%d v=%v found=%v err=%v", c.calls, v.price, found, err)
	}
	v, found, _ = seam.Read(ctx, unread, c.read)
	if c.calls != 1 || !found || v.price.Cmp(big.NewRat(5, 1)) != 0 {
		t.Errorf("unprefetched key must fall through: calls=%d v=%v", c.calls, v.price)
	}
	if _, _, _ = seam.Read(ctx, unread, c.read); c.calls != 2 {
		t.Errorf("a fall-through read is not memoised: calls=%d", c.calls)
	}
}

func TestReadset_FailedPrefetchFallsThrough(t *testing.T) {
	seam := NewSeam[PairKey, ratAnswer]("test_failed", cloneRat)
	ctx := With(context.Background(), New())
	before := testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("test_failed", "prefetch_error"))
	seam.Failed()
	boom := errors.New("timeout")
	c := &counted{err: boom}
	if _, _, err := seam.Read(ctx, PairKey{Base: "a"}, c.read); !errors.Is(err, boom) || c.calls != 1 {
		t.Errorf("a key whose prefetch failed must be read as before: calls=%d err=%v", c.calls, err)
	}
	if got := testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("test_failed", "prefetch_error")); got != before+1 {
		t.Errorf("prefetch_error count %v, want %v", got, before+1)
	}
}

func TestReadset_KeysAreExactSpellings(t *testing.T) {
	usd, eur := asset(t, "fiat:USD"), asset(t, "fiat:EUR")
	native, cxlm := asset(t, "native"), asset(t, "crypto:XLM")
	keys := []PairKey{
		Pair(native, usd, time.Minute), Pair(cxlm, usd, time.Minute), Pair(native, eur, time.Minute),
		Pair(native, usd, time.Hour), Pair(usd, native, time.Minute), Pair(native, usd, 0),
	}
	seam := NewSeam[PairKey, ratAnswer]("test_keys", cloneRat)
	ctx := With(context.Background(), New())
	for i, k := range keys {
		seam.Fill(ctx, k, ratAnswer{big.NewRat(int64(i+1), 1)}, true)
	}
	for i, k := range keys {
		v, found, _ := seam.Read(ctx, k, (&counted{}).read)
		if !found || v.price.Cmp(big.NewRat(int64(i+1), 1)) != 0 {
			t.Errorf("%+v answered %v, want %d: keys must not fold spellings, quotes or windows", k, v.price, i+1)
		}
	}
	other := NewSeam[PairKey, ratAnswer]("test_keys_other", cloneRat)
	if c := (&counted{}); func() bool { _, _, _ = other.Read(ctx, keys[0], c.read); return c.calls != 1 }() {
		t.Error("a seam must not answer from another seam's fills")
	}
}

func TestReadset_ValuesAreNotShared(t *testing.T) {
	seam := NewSeam[PairKey, ratAnswer]("test_alias", cloneRat)
	ctx := With(context.Background(), New())
	k := PairKey{Base: "a"}
	filled := ratAnswer{big.NewRat(1, 3)}
	seam.Fill(ctx, k, filled, true)
	filled.price.SetInt64(99)
	first, _, _ := seam.Read(ctx, k, (&counted{}).read)
	first.price.SetInt64(77)
	second, _, _ := seam.Read(ctx, k, (&counted{}).read)
	if second.price.Cmp(big.NewRat(1, 3)) != 0 {
		t.Errorf("memoised value changed to %v by a caller's mutation", second.price)
	}
}

func TestReadset_FirstFillWinsAndBounded(t *testing.T) {
	seam := NewSeam[int, int]("test_bound", nil)
	ctx := With(context.Background(), New())
	seam.Fill(ctx, 0, 1, true)
	seam.Fill(ctx, 0, 2, true)
	if v, _, _ := seam.Read(ctx, 0, func(context.Context) (int, bool, error) { return -1, true, nil }); v != 1 {
		t.Errorf("refill changed the answer to %d", v)
	}
	for i := 1; i <= maxEntries; i++ {
		seam.Fill(ctx, i, i, true)
	}
	if v, _, _ := seam.Read(ctx, maxEntries, func(context.Context) (int, bool, error) { return -1, true, nil }); v != -1 {
		t.Errorf("a fill past the bound was kept (%d); it must fall through", v)
	}
	if got := len(From(ctx).m); got != maxEntries {
		t.Errorf("memo holds %d entries, bound %d", got, maxEntries)
	}
}

func TestReadset_ConcurrentRows(t *testing.T) {
	seam := NewSeam[int, ratAnswer]("test_race", cloneRat)
	ctx := With(context.Background(), New())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				seam.Fill(ctx, i, ratAnswer{big.NewRat(int64(i), 1)}, i%3 != 0)
				v, found, _ := seam.Read(ctx, (i+g)%200, func(context.Context) (ratAnswer, bool, error) { return ratAnswer{}, false, nil })
				if found {
					v.price.SetInt64(-1)
				}
			}
		}(g)
	}
	wg.Wait()
	for i := 0; i < 200; i++ {
		v, found, _ := seam.Read(ctx, i, (&counted{}).read)
		if want := i%3 != 0; found != want || (found && v.price.Cmp(big.NewRat(int64(i), 1)) != 0) {
			t.Errorf("key %d: found=%v v=%v", i, found, v.price)
		}
	}
}

// fakeClock is a concurrency-safe clock tests advance by hand.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func TestReadset_ExpiredEntriesFallThrough(t *testing.T) {
	seam := NewSeam[PairKey, ratAnswer]("test_expired", cloneRat)
	clk := &fakeClock{}
	clk.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	ctx := With(context.Background(), NewWithClock(clk.now))
	present, absent := PairKey{Base: "a"}, PairKey{Base: "b"}
	seam.Fill(ctx, present, ratAnswer{big.NewRat(1, 2)}, true)
	seam.Fill(ctx, absent, ratAnswer{}, false)
	expired := func() float64 {
		return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("test_expired", "expired"))
	}
	e0 := expired()
	c := &counted{want: ratAnswer{big.NewRat(3, 1)}, found: true}

	clk.advance(MaxAge) // the boundary: exactly MaxAge old still answers
	if v, found, _ := seam.Read(ctx, present, c.read); c.calls != 0 || !found || v.price.Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("at MaxAge: calls=%d v=%v found=%v, want the memoised 1/2", c.calls, v.price, found)
	}
	if _, found, _ := seam.Read(ctx, absent, c.read); c.calls != 0 || found {
		t.Fatalf("at MaxAge: a memoised miss must still answer: calls=%d found=%v", c.calls, found)
	}

	clk.advance(time.Nanosecond)
	if v, found, _ := seam.Read(ctx, present, c.read); c.calls != 1 || !found || v.price.Cmp(big.NewRat(3, 1)) != 0 {
		t.Errorf("past MaxAge: calls=%d v=%v, want the real read's 3", c.calls, v.price)
	}
	if _, found, _ := seam.Read(ctx, absent, c.read); c.calls != 2 || !found {
		t.Errorf("past MaxAge: a memoised miss must fall through too: calls=%d found=%v", c.calls, found)
	}
	if got := expired(); got != e0+2 {
		t.Errorf("expired count %v, want %v", got, e0+2)
	}
}

func TestReadset_ExpiryRace(t *testing.T) {
	seam := NewSeam[int, int]("test_expiry_race", nil)
	clk := &fakeClock{}
	ctx := With(context.Background(), NewWithClock(clk.now))
	for i := 0; i < 100; i++ {
		seam.Fill(ctx, i, i, true)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			clk.advance(10 * time.Millisecond)
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				// A fresh read answers the memo (i%100); an expired one answers the real read (-1); nothing else is right.
				at := clk.now()
				v, _, _ := seam.Read(ctx, i%100, func(context.Context) (int, bool, error) { return -1, true, nil })
				if v != i%100 && v != -1 {
					t.Errorf("key %d answered %d", i%100, v)
				}
				if v == i%100 && at.Sub(time.Unix(0, 0)) > MaxAge {
					t.Errorf("key %d answered from a memo already %v old", i%100, at.Sub(time.Unix(0, 0)))
				}
			}
		}()
	}
	wg.Wait()
	if v, _, _ := seam.Read(ctx, 1, func(context.Context) (int, bool, error) { return -1, true, nil }); v != -1 {
		t.Errorf("after 4s the memo answered %d; it must have expired", v)
	}
}

func TestReadset_CountsHitAndFallthrough(t *testing.T) {
	seam := NewSeam[int, int]("test_metric", nil)
	hit := func() float64 {
		return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("test_metric", "hit"))
	}
	fall := func() float64 {
		return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("test_metric", "fallthrough"))
	}
	h0, f0 := hit(), fall()
	read := func(context.Context) (int, bool, error) { return 0, false, nil }
	_, _, _ = seam.Read(context.Background(), 1, read) // no memo: not counted
	ctx := With(context.Background(), New())
	seam.Fill(ctx, 1, 1, true)
	_, _, _ = seam.Read(ctx, 1, read)
	_, _, _ = seam.Read(ctx, 2, read)
	if hit() != h0+1 || fall() != f0+1 {
		t.Errorf("hit %v fallthrough %v, want %v %v", hit(), fall(), h0+1, f0+1)
	}
}
