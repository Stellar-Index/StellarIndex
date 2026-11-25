package v1_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
)

// The golden batch requests with the batch planner on: each seam is decorated the way cmd/stellarindex-api's are, with
// a prefetch capability, so the planner fills a memo before the rows run.

type pfAnswer struct {
	snap  v1.PriceSnapshot
	srcs  []string
	stale bool
	err   error
}

var (
	pfPriceSeam = readset.NewPairSeam[pfAnswer]("pf_prices", func(a pfAnswer) pfAnswer {
		a.srcs = slices.Clone(a.srcs)
		return a
	})
	pfTriSeam = readset.NewSeam[readset.PairKey, v1.CachedVWAP]("pf_triangulated", rsCloneVWAP)
)

// pfFault is how every prefetch of a server misbehaves.
type pfFault int

const (
	pfOK      pfFault = iota
	pfFail            // every prefetch read fails
	pfPanic           // the prefetch panics
	pfOverrun         // the prefetch outlives the planner's budget, waiting on its context
	pfCancel          // the client goes away during the prefetch
)

// pfStats records what a decorated server's reads did: the reads rows made with a memo on ctx (fallthrough or expired),
// the keys a prefetch left unfilled and the reads each prefetch was asked for.
type pfStats struct {
	mu       sync.Mutex
	fault    pfFault
	cancel   context.CancelFunc
	fell     []pfRead
	unfilled map[pfRead]bool
	asked    [][]readset.PairRead
	// overrun receives an overrunning fill's context once the planner has stopped waiting, and is closed when that
	// context is done.
	overrun     chan context.Context
	overrunDone chan struct{}
	// afterPrices runs once a price prefetch has filled.
	afterPrices func()
}

type pfRead struct {
	seam string
	key  readset.PairKey
}

func (st *pfStats) rowRead(ctx context.Context, seam string, k readset.PairKey) {
	if readset.From(ctx) == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fell = append(st.fell, pfRead{seam, k})
}

func (st *pfStats) left(seam string, k readset.PairKey) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.unfilled[pfRead{seam, k}] = true
}

// enter records the ask and applies the fault; false means the prefetch reads nothing.
func (st *pfStats) enter(ctx context.Context, reads []readset.PairRead) bool {
	st.mu.Lock()
	st.asked = append(st.asked, reads)
	st.mu.Unlock()
	switch st.fault {
	case pfPanic:
		panic("prefetch exploded")
	case pfOverrun:
		st.overrun <- ctx
		<-ctx.Done()
		close(st.overrunDone)
		return false
	case pfCancel:
		st.cancel()
		return false
	case pfOK, pfFail:
	}
	return true
}

type pfPrices struct {
	v1.PriceReader
	st *pfStats
}

func (p pfPrices) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	k := readset.Pair(a, q, 0)
	ans, found, _ := pfPriceSeam.Read(ctx, k, func(ctx context.Context) (pfAnswer, bool, error) {
		p.st.rowRead(ctx, "prices", k)
		snap, srcs, stale, err := p.PriceReader.LatestPrice(ctx, a, q)
		return pfAnswer{snap, srcs, stale, err}, true, nil
	})
	if !found {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return ans.snap, ans.srcs, ans.stale, ans.err
}

// PrefetchLatestPrices memoises only a definite answer, as the store decorator below the gates does: a withheld verdict
// depends on the row's own include_thin admission, and a failed read is no answer.
func (p pfPrices) PrefetchLatestPrices(ctx context.Context, reads []readset.PairRead) {
	if !p.st.enter(ctx, reads) {
		return
	}
	if p.st.afterPrices != nil {
		defer p.st.afterPrices()
	}
	for _, r := range reads {
		snap, srcs, stale, err := p.PriceReader.LatestPrice(ctx, r.Base, r.Quote)
		if p.st.fault == pfFail || (err != nil && !errors.Is(err, v1.ErrPriceNotFound)) {
			pfPriceSeam.Failed()
			p.st.left("prices", r.Key())
			continue
		}
		pfPriceSeam.Fill(ctx, r.Key(), pfAnswer{snap, srcs, stale, err}, err == nil)
	}
}

type pfTri struct {
	next v1.TriangulatedPriceLooker
	st   *pfStats
}

func (l pfTri) LookupTriangulatedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (v1.CachedVWAP, bool, error) {
	k := readset.Pair(base, quote, window)
	return pfTriSeam.Read(ctx, k, func(ctx context.Context) (v1.CachedVWAP, bool, error) {
		l.st.rowRead(ctx, "triangulated", k)
		return l.next.LookupTriangulatedVWAP(ctx, base, quote, window)
	})
}

func (l pfTri) PrefetchTriangulatedVWAP(ctx context.Context, reads []readset.PairRead) {
	if l.st.fault == pfOverrun || !l.st.enter(ctx, reads) {
		return
	}
	for _, r := range reads {
		v, found, err := l.next.LookupTriangulatedVWAP(ctx, r.Base, r.Quote, r.Window)
		if l.st.fault == pfFail || err != nil {
			pfTriSeam.Failed()
			l.st.left("triangulated", r.Key())
			continue
		}
		pfTriSeam.Fill(ctx, r.Key(), v, found)
	}
}

type pfChangeAnswer struct {
	v   string
	err error
}

var pfChangeSeam = readset.NewSeam[readset.PairKey, pfChangeAnswer]("pf_change24h", nil)

// pfChange24h keys an anchor read as the USD pair of the spelling asked, as the production reader reads it.
type pfChange24h struct {
	next v1.Change24hReader
	st   *pfStats
}

func (c pfChange24h) USDPrice24hAgo(ctx context.Context, a canonical.Asset) (string, error) {
	k := readset.Pair(a, canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}, 0)
	ans, _, _ := pfChangeSeam.Read(ctx, k, func(ctx context.Context) (pfChangeAnswer, bool, error) {
		c.st.rowRead(ctx, "change24h", k)
		v, err := c.next.USDPrice24hAgo(ctx, a)
		return pfChangeAnswer{v, err}, true, nil
	})
	return ans.v, ans.err
}

func (c pfChange24h) PrefetchUSDPrice24hAgo(ctx context.Context, assets []canonical.Asset) {
	switch c.st.fault {
	case pfPanic:
		panic("change_24h prefetch exploded")
	case pfOverrun, pfCancel:
		return
	case pfOK, pfFail:
	}
	for _, a := range assets {
		k := readset.Pair(a, canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}, 0)
		v, err := c.next.USDPrice24hAgo(ctx, a)
		if c.st.fault == pfFail || (err != nil && !errors.Is(err, v1.ErrChange24hUnavailable)) {
			pfChangeSeam.Failed()
			c.st.left("change24h", k)
			continue
		}
		pfChangeSeam.Fill(ctx, k, pfChangeAnswer{v, err}, true)
	}
}

type pfTriMeta struct{ pfTri }

func (l pfTriMeta) LookupCompositeMeta(ctx context.Context, base, quote canonical.Asset, window time.Duration) ([]byte, bool, error) {
	return l.next.(v1.CompositeMetaLooker).LookupCompositeMeta(ctx, base, quote, window)
}

// pfOptions is spec's wiring with its price and triangulated seams decorated with a prefetch.
func pfOptions(t *testing.T, spec eqSpec, st *pfStats) v1.Options {
	t.Helper()
	o := spec.build(t)
	if o.Prices != nil {
		o.Prices = pfPrices{o.Prices, st}
	}
	if _, ok := o.Triangulated.(v1.CompositeMetaLooker); ok {
		o.Triangulated = pfTriMeta{pfTri{o.Triangulated, st}}
	} else if o.Triangulated != nil {
		o.Triangulated = pfTri{o.Triangulated, st}
	}
	if o.Change24h != nil {
		o.Change24h = pfChange24h{o.Change24h, st}
	}
	return o
}

func newPfStats(fault pfFault) *pfStats {
	return &pfStats{fault: fault, unfilled: map[pfRead]bool{}, overrun: make(chan context.Context, 1), overrunDone: make(chan struct{})}
}

// pfPadded is req with unpriced ids appended up to the planner's 8-id floor, so the planner runs on every fixture.
func pfPadded(req eqBatchRequest) eqBatchRequest {
	distinct := map[string]bool{}
	for _, id := range req.ids {
		distinct[id] = true
	}
	ids := append([]string{}, req.ids...)
	for i := 0; len(distinct) < 8; i++ {
		id := eqID(fmt.Sprintf("PAD%d", i))
		distinct[id] = true
		ids = append(ids, id)
	}
	req.ids = ids
	return req
}

func pfHits(seam string) float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues(seam, "hit"))
}

// pfInScope reports whether a row's read is one the planner covers for req: an alias spelling of a requested id against
// the request's quote, with the Redis VWAP fallback's window on the triangulated seam.
func pfInScope(t *testing.T, req eqBatchRequest, r pfRead) bool {
	t.Helper()
	q, err := canonical.ParseAsset(req.quote)
	if err != nil || r.key.Quote != q.String() {
		return false
	}
	if (r.seam == "triangulated") != (r.key.Window == 5*time.Minute) || (r.seam != "triangulated" && r.key.Window != 0) {
		return false
	}
	for _, id := range req.ids {
		a, err := canonical.ParseAsset(id)
		if err != nil {
			continue
		}
		for _, alias := range canonical.AssetAliases(a) {
			if alias.String() == r.key.Base {
				return true
			}
		}
	}
	return false
}

func TestPriceBatchPrefetch_GoldenIsByteIdenticalAndAnsweredFromTheMemo(t *testing.T) {
	seams := []string{"pf_prices", "pf_triangulated", "pf_change24h"}
	before := map[string]float64{}
	for _, s := range seams {
		before[s] = pfHits(s)
	}
	for _, spec := range eqSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			st := newPfStats(pfOK)
			plain, on := v1.New(spec.build(t)).Handler(), v1.New(pfOptions(t, spec, st)).Handler()
			for _, req := range eqGoldenRequests(spec) {
				req = pfPadded(req)
				st.fell = nil
				if m := batchEnvelopeMismatch(t, plain, on, req); m != "" {
					t.Errorf("%+v: planner on differs: %s", req, m)
				}
				// Every covered read a row made went to its store only where the prefetch could not answer it.
				for _, r := range st.fell {
					if pfInScope(t, req, r) && !st.unfilled[r] {
						t.Errorf("%+v: row read %s %+v from the store, not the prefetch", req, r.seam, r.key)
					}
				}
			}
		})
	}
	for _, s := range seams {
		if pfHits(s) == before[s] {
			t.Errorf("seam %s: no row read was answered from the prefetch", s)
		}
	}
}

func TestPriceBatchPrefetch_RowEqualsSinglePrice(t *testing.T) {
	eqRowsEqualSingles(t, func(t *testing.T, spec eqSpec) http.Handler {
		return v1.New(pfOptions(t, spec, newPfStats(pfOK))).Handler()
	}, func(ids []string) []string { return pfPadded(eqBatchRequest{ids: ids}).ids })
}

func pfUniverse(t *testing.T) eqSpec {
	t.Helper()
	for _, spec := range eqSpecs() {
		if spec.name == "universe" {
			return spec
		}
	}
	t.Fatal("no universe fixture")
	return eqSpec{}
}

func TestPriceBatchPrefetch_FailingPrefetchNeverFailsTheRequest(t *testing.T) {
	spec := pfUniverse(t)
	plain := v1.New(spec.build(t)).Handler()
	reqs := eqGoldenRequests(spec)
	for _, tc := range []struct {
		name  string
		fault pfFault
		reqs  []eqBatchRequest
	}{
		{"every read fails", pfFail, reqs},
		{"panic", pfPanic, reqs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPfStats(tc.fault)
			on := v1.New(pfOptions(t, spec, st)).Handler()
			hits := pfHits("pf_prices")
			for _, req := range tc.reqs {
				if m := batchEnvelopeMismatch(t, plain, on, req); m != "" {
					t.Errorf("%+v: differs from no planner: %s", req, m)
				}
			}
			if pfHits("pf_prices") != hits {
				t.Error("a failed prefetch answered a row")
			}
		})
	}
}

func TestPriceBatchPrefetch_ClientGoneDuringPrefetchWritesNothing(t *testing.T) {
	spec := pfUniverse(t)
	st := newPfStats(pfCancel)
	on := v1.New(pfOptions(t, spec, st)).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	rec := httptest.NewRecorder()
	target := "/v1/price/batch?" + eqQuery(strings.Join(spec.ids, ","), "fiat:USD", false, "asset_ids")
	on.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx))
	if rec.Body.Len() != 0 {
		t.Errorf("an aborted batch wrote %d %s", rec.Code, rec.Body.String())
	}
}

// The budget stops the planner's wait, never the fill: a statement cancelled in flight costs pgx its connection. The
// fill's context ends at readset.MaxAge.
func TestPriceBatchPrefetch_BudgetStopsTheWaitNotTheFill(t *testing.T) {
	spec := pfUniverse(t)
	st := newPfStats(pfOverrun)
	plain, on := v1.New(spec.build(t)).Handler(), v1.New(pfOptions(t, spec, st)).Handler()
	start := time.Now()
	if m := batchEnvelopeMismatch(t, plain, on, eqGoldenRequests(spec)[0]); m != "" {
		t.Errorf("differs from no planner: %s", m)
	}
	if took := time.Since(start); took >= readset.MaxAge {
		t.Fatalf("the request waited %v for a fill", took)
	}
	fctx := <-st.overrun
	if err := fctx.Err(); err != nil {
		t.Fatalf("the fill's context ended with the budget: %v", err)
	}
	select {
	case <-st.overrunDone:
	case <-time.After(2 * readset.MaxAge):
		t.Fatal("the fill's context outlived readset.MaxAge")
	}
	if took := time.Since(start); !errors.Is(fctx.Err(), context.DeadlineExceeded) || took < readset.MaxAge {
		t.Errorf("fill context ended after %v with %v, want readset.MaxAge's deadline", took, fctx.Err())
	}
}

var pfFrzSeam = readset.NewSeam[readset.PairKey, struct{}]("pf_freeze", nil)

// pfLateFreeze is a freeze looker that could prefetch, whose FRZH/fiat:USD marker is set only once armed.
type pfLateFreeze struct {
	next  v1.FrozenLooker
	armed *atomic.Bool
	asked *atomic.Int32
}

func (l pfLateFreeze) frozen(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	if asset.String() == eqID("FRZH") && !l.armed.Load() {
		return false, nil
	}
	return l.next.FrozenForPair(ctx, asset, quote)
}

func (l pfLateFreeze) FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	_, frozen, err := pfFrzSeam.Read(ctx, readset.Pair(asset, quote, 0), func(ctx context.Context) (struct{}, bool, error) {
		f, err := l.frozen(ctx, asset, quote)
		return struct{}{}, f, err
	})
	return frozen, err
}

func (l pfLateFreeze) PrefetchFrozen(ctx context.Context, reads []readset.PairRead) {
	defer l.asked.Add(1) // counted once filled, so arming waits for a prefetch that ran
	for _, r := range reads {
		if f, err := l.frozen(ctx, r.Base, r.Quote); err == nil {
			pfFrzSeam.Fill(ctx, r.Key(), struct{}{}, f)
		}
	}
}

// A row's freeze verdict is read after the price it judges: a marker set after the planner prefetched the price, but
// before the row's freeze read, holds the row, while the price still comes from the memo.
func TestPriceBatchPrefetch_FreezeIsReadAfterThePrice(t *testing.T) {
	spec := pfUniverse(t)
	st := newPfStats(pfOK)
	o := pfOptions(t, spec, st)
	var armed atomic.Bool
	var asked atomic.Int32
	o.Freeze = pfLateFreeze{next: o.Freeze, armed: &armed, asked: &asked}
	st.afterPrices = func() {
		for deadline := time.Now().Add(200 * time.Millisecond); asked.Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		armed.Store(true)
	}
	plain, on := v1.New(spec.build(t)).Handler(), v1.New(o).Handler()
	hits := pfHits("pf_prices")
	req := eqBatchRequest{ids: spec.ids, quote: "fiat:USD"}
	if m := batchEnvelopeMismatch(t, plain, on, req); m != "" {
		t.Errorf("a marker set after the prefetch was not seen: %s", m)
	}
	if asked.Load() != 0 {
		t.Error("the planner prefetched the freeze marker")
	}
	if pfHits("pf_prices") == hits {
		t.Error("no price came from the memo: the ordering was not exercised")
	}
}

func pfCapped() float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("pf_prices", "capped"))
}

// The planner runs from 8 ids, and prefetches only the first 200 of a 1000-id POST.
func TestPriceBatchPrefetch_IDBounds(t *testing.T) {
	spec := pfUniverse(t)
	for _, n := range []int{7, 8} {
		st := newPfStats(pfOK)
		on := v1.New(pfOptions(t, spec, st)).Handler()
		rsServe(on, "/v1/price/batch?"+eqQuery(strings.Join(spec.ids[:n], ","), "fiat:USD", false, "asset_ids"))
		if asked := len(st.asked) > 0; asked != (n >= 8) {
			t.Errorf("%d ids: prefetched=%v", n, asked)
		}
	}

	ids := append([]string{}, spec.ids...)
	for i := len(ids); i < 1000; i++ {
		ids = append(ids, eqID(fmt.Sprintf("B%d", i)))
	}
	st := newPfStats(pfOK)
	plain, on := v1.New(spec.build(t)).Handler(), v1.New(pfOptions(t, spec, st)).Handler()
	capped := pfCapped()
	if m := batchEnvelopeMismatch(t, plain, on, eqBatchRequest{post: true, ids: ids, quote: "fiat:USD"}); m != "" {
		t.Errorf("1000-id POST differs: %s", m)
	}
	if pfCapped() != capped {
		t.Error("the memo capped")
	}
	head := map[string]bool{}
	for _, id := range ids[:200] {
		a, _ := canonical.ParseAsset(id)
		for _, alias := range canonical.AssetAliases(a) {
			head[alias.String()] = true
		}
	}
	for _, reads := range st.asked {
		for _, r := range reads {
			if !head[r.Base.String()] {
				t.Fatalf("prefetched %v, beyond the first 200 ids", r.Base)
			}
		}
		if len(reads) < 200 {
			t.Errorf("prefetched %d reads, want every spelling of the first 200 ids", len(reads))
		}
	}

	// An invalid id past the first 200 still fails the batch, so nothing is prefetched for it.
	bad := append([]string{}, ids...)
	bad[500] = "@@@"
	st = newPfStats(pfOK)
	on = v1.New(pfOptions(t, spec, st)).Handler()
	if m := batchEnvelopeMismatch(t, plain, on, eqBatchRequest{post: true, ids: bad, quote: "fiat:USD"}); m != "" {
		t.Errorf("invalid id at 500 differs: %s", m)
	}
	if len(st.asked) != 0 {
		t.Errorf("a batch that 400s prefetched %d times", len(st.asked))
	}
}
