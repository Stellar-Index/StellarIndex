package v1_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// The golden batch requests through the read memo, decorated the way cmd/stellarindex-api's prefetch decorators wrap
// the Redis lookers (that package is not importable here): an empty memo and one seeded from the same fakes must both
// answer byte for byte as the undecorated server.

var (
	rsTriSeam    = readset.NewSeam[readset.PairKey, v1.CachedVWAP]("golden_triangulated", rsCloneVWAP)
	rsFreezeSeam = readset.NewSeam[readset.PairKey, struct{}]("golden_freeze", nil)
)

func rsCloneVWAP(v v1.CachedVWAP) v1.CachedVWAP {
	if v.Coverage != nil {
		c := *v.Coverage
		v.Coverage = &c
	}
	return v
}

// rsReads records every pair a decorated server read, so a later memo is seeded with exactly those keys.
type rsReads struct {
	mu       sync.Mutex
	tri, frz map[readset.PairKey]readset.PairRead
}

func (r *rsReads) add(m map[readset.PairKey]readset.PairRead, read readset.PairRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m[read.Key()] = read
}

// seed fills ctx's memo by reading every recorded key from the undecorated fakes, as a prefetch would.
func (r *rsReads) seed(ctx context.Context, tri v1.TriangulatedPriceLooker, frz v1.FrozenLooker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, read := range r.tri {
		v, found, err := tri.LookupTriangulatedVWAP(ctx, read.Base, read.Quote, read.Window)
		if err != nil {
			rsTriSeam.Failed()
			continue
		}
		rsTriSeam.Fill(ctx, k, v, found)
	}
	for k, read := range r.frz {
		frozen, err := frz.FrozenForPair(ctx, read.Base, read.Quote)
		if err != nil {
			rsFreezeSeam.Failed()
			continue
		}
		rsFreezeSeam.Fill(ctx, k, struct{}{}, frozen)
	}
}

type rsTri struct {
	next v1.TriangulatedPriceLooker
	rec  *rsReads
}

func (l rsTri) LookupTriangulatedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (v1.CachedVWAP, bool, error) {
	l.rec.add(l.rec.tri, readset.PairRead{Base: base, Quote: quote, Window: window})
	return rsTriSeam.Read(ctx, readset.Pair(base, quote, window), func(ctx context.Context) (v1.CachedVWAP, bool, error) {
		return l.next.LookupTriangulatedVWAP(ctx, base, quote, window)
	})
}

var rsMetaForwards atomic.Int64

// rsTriMeta is rsTri over a next that has composite meta, forwarded unmemoised as the production decorator's embedding does.
type rsTriMeta struct{ rsTri }

func (l rsTriMeta) LookupCompositeMeta(ctx context.Context, base, quote canonical.Asset, window time.Duration) ([]byte, bool, error) {
	rsMetaForwards.Add(1)
	return l.next.(v1.CompositeMetaLooker).LookupCompositeMeta(ctx, base, quote, window)
}

type rsFreeze struct {
	next v1.FrozenLooker
	rec  *rsReads
}

func (l rsFreeze) FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	l.rec.add(l.rec.frz, readset.PairRead{Base: asset, Quote: quote})
	_, frozen, err := rsFreezeSeam.Read(ctx, readset.Pair(asset, quote, 0), func(ctx context.Context) (struct{}, bool, error) {
		frozen, err := l.next.FrozenForPair(ctx, asset, quote)
		return struct{}{}, frozen, err
	})
	return frozen, err
}

// rsServer is spec's server with its lookers decorated and a fresh memo on every request, seeded when seed is set.
func rsServer(t *testing.T, spec eqSpec, rec *rsReads, seed bool) http.Handler {
	t.Helper()
	o := spec.build(t)
	tri, frz := o.Triangulated, o.Freeze
	if _, ok := tri.(v1.CompositeMetaLooker); ok {
		o.Triangulated = rsTriMeta{rsTri{tri, rec}}
	} else if tri != nil {
		o.Triangulated = rsTri{tri, rec}
	}
	if frz != nil {
		o.Freeze = rsFreeze{frz, rec}
	}
	h := v1.New(o).Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := readset.With(r.Context(), readset.New())
		if seed {
			rec.seed(ctx, tri, frz)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func rsServe(h http.Handler, target string) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	body := rsSingleVolatile.ReplaceAll(eqVolatile.ReplaceAll(rec.Body.Bytes(), nil), nil)
	return fmt.Sprintf("%d %s", rec.Code, body)
}

// rsSingleVolatile is the wall-clock stamps a single read's withheld problem carries beyond eqVolatile's.
var rsSingleVolatile = regexp.MustCompile(`"(measured_at|window_end)":"[^"]*"`)

func rsHits() float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("golden_triangulated", "hit")) +
		testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("golden_freeze", "hit"))
}

func TestPriceBatchGolden_ReadMemoIsByteIdentical(t *testing.T) {
	hits0 := rsHits()
	for _, spec := range eqSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			rec := &rsReads{tri: map[readset.PairKey]readset.PairRead{}, frz: map[readset.PairKey]readset.PairRead{}}
			plain := v1.New(spec.build(t)).Handler()
			empty, seeded := rsServer(t, spec, rec, false), rsServer(t, spec, rec, true)
			for _, req := range eqGoldenRequests(spec) {
				if m := batchEnvelopeMismatch(t, plain, empty, req); m != "" {
					t.Errorf("%+v: empty memo differs: %s", req, m)
				}
				if m := batchEnvelopeMismatch(t, plain, seeded, req); m != "" {
					t.Errorf("%+v: seeded memo differs: %s", req, m)
				}
			}
			// Single reads through the same decorators: the only path that reads composite meta.
			for _, id := range spec.ids {
				for _, quote := range spec.quotes {
					target := "/v1/price?" + eqQuery(id, quote, spec.thin[0], "asset")
					want := rsServe(plain, target)
					if got := rsServe(seeded, target); got != want {
						t.Errorf("%s: decorated single read differs\n a: %s\n b: %s", target, want, got)
					}
				}
			}
		})
	}
	if rsHits() == hits0 {
		t.Error("no read was answered from a seeded memo: the comparison proved nothing")
	}
	if rsMetaForwards.Load() == 0 {
		t.Error("no composite meta read went through the decorator: the forwarding is unexercised")
	}
}
