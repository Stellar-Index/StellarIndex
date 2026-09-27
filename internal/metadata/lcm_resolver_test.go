package metadata

import (
	"context"
	"errors"
	"testing"
)

// fakeLookup: an issuer present in rows is observed with that domain
// ("" = observed with no home_domain); absent = never observed.
type fakeLookup struct {
	rows          map[string]string
	err           error
	gotAsOfLedger uint32 // captures the asOf the resolver passed
	gotCtxErr     error  // ctx.Err() at read time
}

func (f *fakeLookup) HomeDomainAtOrBefore(ctx context.Context, issuer string, asOfLedger uint32) (IssuerHomeDomain, error) {
	f.gotAsOfLedger = asOfLedger
	f.gotCtxErr = ctx.Err()
	if f.err != nil {
		return IssuerHomeDomain{}, f.err
	}
	d, ok := f.rows[issuer]
	return IssuerHomeDomain{Observed: ok, Domain: d}, nil
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
	_, _ = r.HomeDomainFor(context.Background(), "GA1")
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
		"GA1": "stellarindex.io",
	}})
	got, err := r.HomeDomainFor(context.Background(), "GA1")
	if err != nil {
		t.Fatalf("HomeDomainFor: %v", err)
	}
	if want := (IssuerHomeDomain{Observed: true, Domain: "stellarindex.io"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestLCMHomeDomainResolver_ObservedWithoutDomain — an observation whose
// AccountEntry carries no home_domain is reported as observed, distinct
// from "never observed".
func TestLCMHomeDomainResolver_ObservedWithoutDomain(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{"GA1": ""}})
	got, err := r.HomeDomainFor(context.Background(), "GA1")
	if err != nil {
		t.Fatalf("HomeDomainFor: %v", err)
	}
	if want := (IssuerHomeDomain{Observed: true}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLCMHomeDomainResolver_NotObserved(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{}})
	got, err := r.HomeDomainFor(context.Background(), "GA_UNKNOWN")
	if err != nil {
		t.Fatalf("HomeDomainFor: %v", err)
	}
	if got != (IssuerHomeDomain{}) {
		t.Errorf("got %+v, want not observed", got)
	}
}

// TestLCMHomeDomainResolver_StoreError — wrap with
// ErrLCMUnavailable so the chained-fallback caller can drop to
// static.
func TestLCMHomeDomainResolver_StoreError(t *testing.T) {
	r := NewLCMHomeDomainResolver(&fakeLookup{err: errors.New("network")})
	_, err := r.HomeDomainFor(context.Background(), "GA1")
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
	domain, ok := lookup(context.Background(), "GA1")
	if !ok || domain != "live.example.com" {
		t.Errorf("got (%q, %v), want (live.example.com, true)", domain, ok)
	}
	if staticCalled != 0 {
		t.Errorf("static called %d times when LCM hit, want 0", staticCalled)
	}
}

// TestChainedHomeDomainLookup_ObservedClearSuppressesStatic — an issuer
// whose latest observation carries no home_domain (cleared on chain, or
// merged) must resolve to no domain; the operator's static entry must not
// re-assert an identity the issuer withdrew.
func TestChainedHomeDomainLookup_ObservedClearSuppressesStatic(t *testing.T) {
	live := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{"GA1": ""}})
	staticCalled := 0
	staticFn := func(string) (string, bool) {
		staticCalled++
		return "static.example.com", true
	}
	lookup := ChainedHomeDomainLookup(live, staticFn, nil)
	domain, ok := lookup(context.Background(), "GA1")
	if ok || domain != "" {
		t.Errorf("got (%q, %v), want (\"\", false): on-chain clear must win over static", domain, ok)
	}
	if staticCalled != 0 {
		t.Errorf("static called %d times for an observed issuer, want 0", staticCalled)
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
	domain, ok := lookup(context.Background(), "GA1")
	if !ok || domain != "static.example.com" {
		t.Errorf("got (%q, %v), want (static.example.com, true)", domain, ok)
	}
}

// TestChainedHomeDomainLookup_UsesCallerContext — the store read runs
// under the request context, so a disconnected client cancels it.
func TestChainedHomeDomainLookup_UsesCallerContext(t *testing.T) {
	fake := &fakeLookup{rows: map[string]string{}}
	lookup := ChainedHomeDomainLookup(NewLCMHomeDomainResolver(fake),
		func(string) (string, bool) { return "", false }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookup(ctx, "GA1")
	if !errors.Is(fake.gotCtxErr, context.Canceled) {
		t.Fatal("store read did not inherit the caller's cancelled context")
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
	domain, ok := lookup(context.Background(), "GA1")
	if !ok || domain != "static.example.com" {
		t.Errorf("got (%q, %v), want (static.example.com, true)", domain, ok)
	}
	if warnCalls == 0 {
		t.Errorf("warnFn not called on storage error")
	}
}

// TestSplitHomeDomainLayers — ObservedHomeDomainLookup never answers from
// the static map, and StaticHomeDomainFallback answers only for an
// unobserved issuer (or a failed read), so an observed clear still
// suppresses the static entry when a caller runs its own read between them.
func TestSplitHomeDomainLayers(t *testing.T) {
	live := NewLCMHomeDomainResolver(&fakeLookup{rows: map[string]string{
		"GLIVE": "live.example.com", "GCLEAR": "",
	}})
	broken := NewLCMHomeDomainResolver(&fakeLookup{err: errors.New("network down")})
	staticFn := func(string) (string, bool) { return "static.example.com", true }
	cases := []struct {
		name   string
		lookup func(context.Context, string) (string, bool)
		issuer string
		want   string
	}{
		{"observed: hit", ObservedHomeDomainLookup(live, nil), "GLIVE", "live.example.com"},
		{"observed: unobserved skips static", ObservedHomeDomainLookup(live, nil), "GNONE", ""},
		{"observed: storage error skips static", ObservedHomeDomainLookup(broken, nil), "GLIVE", ""},
		{"static: observed domain suppresses", StaticHomeDomainFallback(live, staticFn, nil), "GLIVE", ""},
		{"static: observed clear suppresses", StaticHomeDomainFallback(live, staticFn, nil), "GCLEAR", ""},
		{"static: unobserved answers", StaticHomeDomainFallback(live, staticFn, nil), "GNONE", "static.example.com"},
		{"static: storage error answers", StaticHomeDomainFallback(broken, staticFn, nil), "GLIVE", "static.example.com"},
	}
	for _, tc := range cases {
		domain, ok := tc.lookup(context.Background(), tc.issuer)
		if domain != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got (%q, %v), want %q", tc.name, domain, ok, tc.want)
		}
	}
}
