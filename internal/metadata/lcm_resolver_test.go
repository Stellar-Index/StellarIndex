package metadata

import (
	"context"
	"errors"
	"testing"
)

type fakeLookup struct {
	rows map[string]string
	err  error
}

func (f *fakeLookup) HomeDomainAtOrBefore(_ context.Context, issuer string, _ uint32) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	d, ok := f.rows[issuer]
	return d, ok, nil
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
