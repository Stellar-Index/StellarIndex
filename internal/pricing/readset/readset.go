// Package readset is the request-scoped read-through memo a /v1/price/batch prefetch fills and each read seam consults.
//
// Invariant: the batch prefetch is a read-through memo beneath every gate: each seam answers a key exactly as its
// single-key read would have at an instant no more than [MaxAge] before the read; any key not read, or read longer ago,
// is read as before — it changes when reads happen, never what they return or how they are judged.
//
// A follower joining another request's shared row flight may be answered from that request's memo, so the age bound,
// not the request, is what limits how old a prefetched answer can be. The detached single-price path never carries one.
package readset

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// maxEntries bounds one request's memo; a fill past it is dropped, so that key is read as before.
const maxEntries = 8192

// MaxAge is how long after its prefetch instant a memo may answer; a read later than that falls through.
const MaxAge = 2 * time.Second

// Memo holds one request's prefetched answers. It is safe for concurrent use; the zero value is not usable, see [New].
type Memo struct {
	mu sync.RWMutex
	m  map[any]entry
	// covered is every pair a [NewPairSeam] seam holds an answer for.
	covered map[PairKey]bool
	// at is the prefetch instant: taken before any fill's read, so every entry is at least as fresh as it.
	at  time.Time
	now func() time.Time
}

// entry is a prefetched answer: found=false is a definite miss and v is then always the zero value.
type entry struct {
	v     any
	found bool
}

// New returns an empty memo whose prefetch instant is now; create it immediately before the prefetch reads.
func New() *Memo { return NewWithClock(time.Now) }

// NewWithClock is [New] reading time from now, which must be safe for concurrent use.
func NewWithClock(now func() time.Time) *Memo {
	return &Memo{m: map[any]entry{}, covered: map[PairKey]bool{}, at: now(), now: now}
}

// Covers reports whether ctx's memo is unexpired and its pair seam holds an answer, found or a definite miss, for the
// (base, quote) price read.
func Covers(ctx context.Context, base, quote canonical.Asset) bool {
	m := From(ctx)
	if m == nil || m.expired() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.covered[Pair(base, quote, 0)]
}

// expired reports whether a read now is more than [MaxAge] after the prefetch instant.
func (m *Memo) expired() bool { return m.now().Sub(m.at) > MaxAge }

type ctxKey struct{}

// With returns ctx carrying m.
func With(ctx context.Context, m *Memo) context.Context { return context.WithValue(ctx, ctxKey{}, m) }

// From returns the memo ctx carries, or nil.
func From(ctx context.Context) *Memo {
	m, _ := ctx.Value(ctxKey{}).(*Memo)
	return m
}

// PairKey is a pair read's memo key in the exact spellings it was asked for: never alias-folded, since each spelling
// is its own read.
type PairKey struct {
	Base, Quote string
	Window      time.Duration
}

// Pair returns the memo key of a (base, quote, window) read; window is zero for a read with none.
func Pair(base, quote canonical.Asset, window time.Duration) PairKey {
	return PairKey{Base: base.String(), Quote: quote.String(), Window: window}
}

// PairRead is one (base, quote, window) read a prefetch is asked to make.
type PairRead struct {
	Base, Quote canonical.Asset
	Window      time.Duration
}

// Key is r's memo key.
func (r PairRead) Key() PairKey { return Pair(r.Base, r.Quote, r.Window) }

// Seam is one read's slice of the memo, keyed by K and answering V.
type Seam[K comparable, V any] struct {
	name  string
	clone func(V) V
	pair  bool // fills mark the pair covered, see [Covers]
}

// NewSeam returns a seam named for the prefetch metric; clone deep-copies a V holding pointers so no two readers share
// one (nil when V holds none).
func NewSeam[K comparable, V any](name string, clone func(V) V) *Seam[K, V] {
	return &Seam[K, V]{name: name, clone: clone}
}

// NewPairSeam is [NewSeam] for the one seam answering a pair's price read, keyed with no window: its fills are what
// [Covers] reports.
func NewPairSeam[V any](name string, clone func(V) V) *Seam[PairKey, V] {
	return &Seam[PairKey, V]{name: name, clone: clone, pair: true}
}

type memoKey[K comparable, V any] struct {
	s *Seam[K, V]
	k K
}

func (s *Seam[K, V]) copyOf(v V) V {
	if s.clone == nil {
		return v
	}
	return s.clone(v)
}

// Fill records a successful prefetch read of k on ctx's memo: found=false is a definite miss. The first fill of a key
// wins. A no-op without a memo.
func (s *Seam[K, V]) Fill(ctx context.Context, k K, v V, found bool) {
	m := From(ctx)
	if m == nil {
		return
	}
	e := entry{found: found}
	if found {
		e.v = s.copyOf(v)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[memoKey[K, V]{s, k}]; ok {
		return
	}
	if len(m.m) >= maxEntries {
		s.count("capped")
		return
	}
	m.m[memoKey[K, V]{s, k}] = e
	if pk, ok := any(k).(PairKey); ok && s.pair {
		m.covered[pk] = true
	}
}

// Failed counts a prefetch read that errored; its key stays unfilled, so the seam reads it as before.
func (s *Seam[K, V]) Failed() { s.count("prefetch_error") }

// Read answers k from ctx's memo when it was prefetched no more than [MaxAge] ago, else calls read: with no memo on ctx
// it is exactly read(ctx).
func (s *Seam[K, V]) Read(ctx context.Context, k K, read func(context.Context) (V, bool, error)) (V, bool, error) {
	m := From(ctx)
	if m == nil {
		return read(ctx)
	}
	m.mu.RLock()
	e, ok := m.m[memoKey[K, V]{s, k}]
	m.mu.RUnlock()
	if !ok {
		s.count("fallthrough")
		return read(ctx)
	}
	if m.expired() {
		s.count("expired")
		return read(ctx)
	}
	s.count("hit")
	var zero V
	if !e.found {
		return zero, false, nil
	}
	v, _ := e.v.(V)
	return s.copyOf(v), true, nil
}

func (s *Seam[K, V]) count(result string) {
	obs.APIPriceBatchPrefetchTotal.WithLabelValues(s.name, result).Inc()
}
