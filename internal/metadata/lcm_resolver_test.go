package metadata

import (
	"context"
	"errors"
	"testing"
)

type fakeLookup struct {
	rows          map[string]string
	err           error
	gotAsOfLedger uint32 // captures the asOf the resolver passed
}

func (f *fakeLookup) HomeDomainAtOrBefore(_ context.Context, issuer string, asOfLedger uint32) (string, bool, error) {
	f.gotAsOfLedger = asOfLedger
	if f.err != nil {
		return "", false, f.err
	}
	d, ok := f.rows[issuer]
	return d, ok, nil
}

// TestLCMHomeDomainResolver_AsOfFitsInPostgresInt32 pins the
// "no upper bound" sentinel below MaxInt32 — the previous
// `^uint32(0)` overflowed the postgres int4 column on every call,
// resurfacing as a flood of `pq: value "4294967295" is out of range
// for type integer (22003)` errors that defeated the LCM path
// entirely on r1 and silently routed every issuer through the
// static-map fallback.
func TestLCMHomeDomainResolver_AsOfFitsInPostgresInt32(t *testing.T) {
	const maxInt32 = uint32(1<<31 - 1) // 2,147,483,647
	lookup := &fakeLookup{rows: map[string]string{}}
	r := NewLCMHomeDomainResolver(lookup)
	_, _, _ = r.HomeDomainFor(context.Background(), "GA1")
	if lookup.gotAsOfLedger > maxInt32 {
		t.Errorf("resolver passed asOfLedger=%d which overflows postgres int4 (max %d). Use math.MaxInt32 not ^uint32(0).",
			lookup.gotAsOfLedger, maxInt32)
	}
	if lookup.gotAsOfLedger == 0 {
		t.Errorf("resolver passed asOfLedger=0 — that means 'only observations up to genesis', not 'latest'")
	}
}

func TestLCMHomeDomainResolver_HappyPath(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{
		"GA1": "ratesengine.net",
	}})
	domain, ok, err := r.HomeDomainFor(context.Background(), "GA1")
	if err != nil {
		t.Fatalf("HomeDomainFor: %v", err)
	}
	if !ok || domain != "ratesengine.net" {
		t.Errorf("got (%q, %v), want (ratesengine.net, true)", domain, ok)
	}
}

// TestLCMHomeDomainResolver_NotObserved — issuer has no
// observation. The resolver returns ("", false, nil) — same
// shape as "observed but empty" because the closure caller has
// to fall through to the static map either way.
func TestLCMHomeDomainResolver_NotObserved(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{}})
	domain, ok, err := r.HomeDomainFor(context.Background(), "GA_UNKNOWN")
	if err != nil {
		t.Fatalf("HomeDomainFor: %v", err)
	}
	if ok || domain != "" {
		t.Errorf("got (%q, %v), want (empty, false)", domain, ok)
	}
}

// TestLCMHomeDomainResolver_StoreError — wrap with
// ErrLCMUnavailable so the chained-fallback caller can drop to
// static.
func TestLCMHomeDomainResolver_StoreError(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{err: errors.New("network")})
	_, _, err := r.HomeDomainFor(context.Background(), "GA1")
	if !errors.Is(err, ErrLCMUnavailable) {
		t.Errorf("err=%v want wrapping ErrLCMUnavailable", err)
	}
}

// TestChainedHomeDomainLookup_LiveWins — when the LCM resolver
// returns a non-empty domain, the static map is not consulted.
func TestChainedHomeDomainLookup_LiveWins(t *testing.T) {
	live := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{
		"GA1": "live.example.com",
	}})
	staticCalled := 0
	staticFn := func(string) (string, bool) {
		staticCalled++
		return "static.example.com", true
	}
	lookup := ChainedHomeDomainLookup(live, staticFn, nil)
	domain, ok := lookup("GA1")
	if !ok || domain != "live.example.com" {
		t.Errorf("got (%q, %v), want (live.example.com, true)", domain, ok)
	}
	if staticCalled != 0 {
		t.Errorf("static called %d times when LCM hit, want 0", staticCalled)
	}
}

// TestChainedHomeDomainLookup_FallsBackOnNoObservation — issuer
// not observed → fall through to static map.
func TestChainedHomeDomainLookup_FallsBackOnNoObservation(t *testing.T) {
	live := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{}})
	staticFn := func(issuer string) (string, bool) {
		if issuer == "GA1" {
			return "static.example.com", true
		}
		return "", false
	}
	lookup := ChainedHomeDomainLookup(live, staticFn, nil)
	domain, ok := lookup("GA1")
	if !ok || domain != "static.example.com" {
		t.Errorf("got (%q, %v), want (static.example.com, true)", domain, ok)
	}
}

// TestChainedHomeDomainLookup_FallsBackOnStorageError — storage
// error logs via warnFn and falls through to static.
func TestChainedHomeDomainLookup_FallsBackOnStorageError(t *testing.T) {
	live := NewLCMHomeDomainResolver(&fakeLookup{err: errors.New("network down")})
	warnCalls := 0
	warnFn := func(string, ...any) { warnCalls++ }
	staticFn := func(string) (string, bool) {
		return "static.example.com", true
	}
	lookup := ChainedHomeDomainLookup(live, staticFn, warnFn)
	domain, ok := lookup("GA1")
	if !ok || domain != "static.example.com" {
		t.Errorf("got (%q, %v), want (static.example.com, true)", domain, ok)
	}
	if warnCalls == 0 {
		t.Errorf("warnFn not called on storage error")
	}
}
