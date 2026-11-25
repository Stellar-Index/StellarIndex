package v1_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	pfPriceSeam = readset.NewSeam[readset.PairKey, pfAnswer]("pf_prices", func(a pfAnswer) pfAnswer {
		a.srcs = slices.Clone(a.srcs)
		return a
	})
	pfTriSeam = readset.NewSeam[readset.PairKey, v1.CachedVWAP]("pf_triangulated", rsCloneVWAP)
	pfFrzSeam = readset.NewSeam[readset.PairKey, struct{}]("pf_freeze", nil)
)

// pfFault is how every prefetch of a server misbehaves.
type pfFault int

const (
	pfOK      pfFault = iota
	pfFail            // every prefetch read fails
	pfPanic           // the prefetch panics
	pfOverrun         // the prefetch outlives the planner's budget, ignoring its context
	pfCancel          // the client goes away during the prefetch
)

// pfStats records what a decorated server's reads did: the reads rows made with a memo on ctx (fallthrough or expired)
// and the keys a prefetch left unfilled.
type pfStats struct {
	mu       sync.Mutex
	fault    pfFault
	release  chan struct{}
	cancel   context.CancelFunc
	fell     []pfRead
	unfilled map[pfRead]bool
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

// enter applies the fault; false means the prefetch reads nothing.
func (st *pfStats) enter() bool {
	switch st.fault {
	case pfPanic:
		panic("prefetch exploded")
	case pfOverrun:
		<-st.release
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
	ans, _, _ := pfPriceSeam.Read(ctx, k, func(ctx context.Context) (pfAnswer, bool, error) {
		p.st.rowRead(ctx, "prices", k)
		snap, srcs, stale, err := p.PriceReader.LatestPrice(ctx, a, q)
		return pfAnswer{snap, srcs, stale, err}, true, nil
	})
	return ans.snap, ans.srcs, ans.stale, ans.err
}

// PrefetchLatestPrices memoises only a definite answer, as the store decorator below the gates does: a withheld verdict
// depends on the row's own include_thin admission, and a failed read is no answer.
func (p pfPrices) PrefetchLatestPrices(ctx context.Context, reads []readset.PairRead) {
	if !p.st.enter() {
		return
	}
	for _, r := range reads {
		snap, srcs, stale, err := p.PriceReader.LatestPrice(ctx, r.Base, r.Quote)
		if p.st.fault == pfFail || (err != nil && !errors.Is(err, v1.ErrPriceNotFound)) {
			pfPriceSeam.Failed()
			p.st.left("prices", r.Key())
			continue
		}
		pfPriceSeam.Fill(ctx, r.Key(), pfAnswer{snap, srcs, stale, err}, true)
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
	if !l.st.enter() {
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

type pfTriMeta struct{ pfTri }

func (l pfTriMeta) LookupCompositeMeta(ctx context.Context, base, quote canonical.Asset, window time.Duration) ([]byte, bool, error) {
	return l.next.(v1.CompositeMetaLooker).LookupCompositeMeta(ctx, base, quote, window)
}

type pfFrz struct {
	next v1.FrozenLooker
	st   *pfStats
}

func (l pfFrz) FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	k := readset.Pair(asset, quote, 0)
	_, frozen, err := pfFrzSeam.Read(ctx, k, func(ctx context.Context) (struct{}, bool, error) {
		l.st.rowRead(ctx, "freeze", k)
		frozen, err := l.next.FrozenForPair(ctx, asset, quote)
		return struct{}{}, frozen, err
	})
	return frozen, err
}

func (l pfFrz) PrefetchFrozen(ctx context.Context, reads []readset.PairRead) {
	if !l.st.enter() {
		return
	}
	for _, r := range reads {
		frozen, err := l.next.FrozenForPair(ctx, r.Base, r.Quote)
		if l.st.fault == pfFail || err != nil {
			pfFrzSeam.Failed()
			l.st.left("freeze", r.Key())
			continue
		}
		pfFrzSeam.Fill(ctx, r.Key(), struct{}{}, frozen)
	}
}

// pfOptions is spec's wiring with every seam it has decorated with a prefetch.
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
	if o.Freeze != nil {
		o.Freeze = pfFrz{o.Freeze, st}
	}
	return o
}

func newPfStats(fault pfFault) *pfStats {
	return &pfStats{fault: fault, release: make(chan struct{}), unfilled: map[pfRead]bool{}}
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
	seams := []string{"pf_prices", "pf_triangulated", "pf_freeze"}
	before := map[string]float64{}
	for _, s := range seams {
		before[s] = pfHits(s)
	}
	for _, spec := range eqSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			st := newPfStats(pfOK)
			plain, on := v1.New(spec.build(t)).Handler(), v1.New(pfOptions(t, spec, st)).Handler()
			for _, req := range eqGoldenRequests(spec) {
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
	})
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
		{"overruns its budget", pfOverrun, reqs[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPfStats(tc.fault)
			defer close(st.release)
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
