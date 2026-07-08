package decimalsguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// fakeReader returns a fixed set of (source, asset) refs and records how many
// times it was called.
type fakeReader struct {
	refs  []timescale.SorobanDEXTradeRef
	err   error
	calls int
}

func (f *fakeReader) RecentSorobanDEXTrades(_ context.Context, _ time.Time) ([]timescale.SorobanDEXTradeRef, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.refs, nil
}

// fakeResolver resolves decimals from a static map and counts lookups so we
// can prove the resolved-cache suppresses repeat lake queries.
type fakeResolver struct {
	decimals map[string]uint32 // present => found
	err      error
	calls    int
}

func (f *fakeResolver) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	f.calls++
	if f.err != nil {
		return 0, false, f.err
	}
	d, ok := f.decimals[contractID]
	return d, ok, nil
}

// counterVal reads the current counter value for a (source, asset) child.
func counterVal(source, asset string) float64 {
	return testutil.ToFloat64(obs.DEXTradeNonstandardDecimalsTotal.WithLabelValues(source, asset))
}

func TestSweep_FiresOnNonStandardDecimals(t *testing.T) {
	const (
		src = "soroswap"
		// Opaque label content only — the fake resolver looks it up in a
		// map, so these are deliberately NOT real strkeys (keeps the fixture
		// clear of Stellar-address secret-scanners).
		asset = "fake-contract-18dp-bridged"
	)
	before := counterVal(src, asset)

	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: src, Asset: asset}}}
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 18}}
	g := New(reader, resolver, Options{Window: time.Minute})

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := counterVal(src, asset) - before; got != 1 {
		t.Fatalf("counter delta = %v, want 1", got)
	}
}

func TestSweep_SilentOnStandardDecimals(t *testing.T) {
	// A 7-dp/7-dp pair — the fast path today — must leave the metric
	// untouched. The ratio is byte-identical to before, and there is no
	// alarm.
	const src = "phoenix"
	base := "fake-contract-7dp-base"
	quote := "fake-contract-7dp-quote"

	beforeBase := counterVal(src, base)
	beforeQuote := counterVal(src, quote)

	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{
		{Source: src, Asset: base},
		{Source: src, Asset: quote},
	}}
	resolver := &fakeResolver{decimals: map[string]uint32{base: 7, quote: 7}}
	g := New(reader, resolver, Options{Window: time.Minute})

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := counterVal(src, base) - beforeBase; got != 0 {
		t.Fatalf("base counter delta = %v, want 0", got)
	}
	if got := counterVal(src, quote) - beforeQuote; got != 0 {
		t.Fatalf("quote counter delta = %v, want 0", got)
	}
}

func TestSweep_DedupsAndCaches(t *testing.T) {
	const (
		src   = "aquarius"
		asset = "fake-contract-6dp-dedup"
	)
	before := counterVal(src, asset)

	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: src, Asset: asset}}}
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 6}}
	g := New(reader, resolver, Options{Window: time.Minute})

	// Three sweeps of the same standing offender.
	for i := 0; i < 3; i++ {
		if err := g.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	if got := counterVal(src, asset) - before; got != 1 {
		t.Fatalf("counter delta = %v, want 1 (dedup per source+asset)", got)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver.calls = %d, want 1 (resolved cache should suppress repeat lake queries)", resolver.calls)
	}
}

func TestSweep_ConservativeOnUnresolvable(t *testing.T) {
	// A resolution error and a not-found declaration must BOTH be silent:
	// we alarm only on a confirmed non-7 value, and neither is cached (so a
	// later-captured instance is re-checked).
	const src = "comet"
	errAsset := "fake-contract-resolve-error"
	missAsset := "fake-contract-no-metadata"

	beforeErr := counterVal(src, errAsset)
	beforeMiss := counterVal(src, missAsset)

	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{
		{Source: src, Asset: errAsset},
		{Source: src, Asset: missAsset},
	}}
	// errAsset -> resolver error; missAsset -> found=false (absent from map).
	resolver := &fakeResolver{decimals: map[string]uint32{}, err: nil}
	// Wrap so errAsset specifically errors.
	res := &selectiveResolver{inner: resolver, errFor: map[string]error{errAsset: errors.New("clickhouse down")}}
	g := New(reader, res, Options{Window: time.Minute})

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := counterVal(src, errAsset) - beforeErr; got != 0 {
		t.Fatalf("error-asset counter delta = %v, want 0", got)
	}
	if got := counterVal(src, missAsset) - beforeMiss; got != 0 {
		t.Fatalf("not-found-asset counter delta = %v, want 0", got)
	}

	// Not cached: a second sweep re-queries both.
	firstCalls := res.calls
	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if res.calls-firstCalls != 2 {
		t.Fatalf("resolver re-query delta = %d, want 2 (unresolvable assets must NOT be cached)", res.calls-firstCalls)
	}
}

// selectiveResolver returns a per-asset error, else delegates.
type selectiveResolver struct {
	inner  *fakeResolver
	errFor map[string]error
	calls  int
}

func (s *selectiveResolver) TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error) {
	s.calls++
	if e, ok := s.errFor[contractID]; ok {
		return 0, false, e
	}
	return s.inner.TokenDecimals(ctx, contractID)
}

func TestSweep_PropagatesEnumerationError(t *testing.T) {
	reader := &fakeReader{err: errors.New("db unreachable")}
	resolver := &fakeResolver{decimals: map[string]uint32{}}
	g := New(reader, resolver, Options{Window: time.Minute})

	if err := g.Sweep(context.Background()); err == nil {
		t.Fatal("expected enumeration error to propagate")
	}
}

// fakeWriter records every UpsertNonstandardDecimalsAsset call so tests can
// assert the confirmed-offender persistence side of report().
type fakeWriter struct {
	calls int
	last  struct {
		asset    string
		decimals uint32
		source   string
	}
	err error
}

func (f *fakeWriter) UpsertNonstandardDecimalsAsset(_ context.Context, asset string, decimals uint32, source string) error {
	f.calls++
	f.last.asset, f.last.decimals, f.last.source = asset, decimals, source
	if f.err != nil {
		return f.err
	}
	return nil
}

// TestSweep_PersistsConfirmedOffenderToWriter proves report() upserts a
// confirmed non-7-decimals asset through the wired Writer exactly once —
// the same dedup latch that suppresses repeat metric increments also
// suppresses repeat upserts (the row doesn't need re-writing every sweep).
func TestSweep_PersistsConfirmedOffenderToWriter(t *testing.T) {
	const (
		src   = "aquarius"
		asset = "fake-contract-9dp-writer"
	)
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: src, Asset: asset}}}
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 9}}
	writer := &fakeWriter{}
	g := New(reader, resolver, Options{Window: time.Minute, Writer: writer})

	for i := 0; i < 3; i++ {
		if err := g.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	if writer.calls != 1 {
		t.Fatalf("writer.calls = %d, want 1 (dedup per source+asset, same latch as the metric)", writer.calls)
	}
	if writer.last.asset != asset || writer.last.decimals != 9 || writer.last.source != src {
		t.Fatalf("writer recorded (%s, %d, %s), want (%s, 9, %s)", writer.last.asset, writer.last.decimals, writer.last.source, asset, src)
	}
}

// TestSweep_WriterErrorDoesNotBlockDetection proves a persistence failure
// is swallowed (warn-logged) — the metric still fires and Sweep still
// succeeds. The serving guard staying unfed on a DB hiccup must never take
// down the detection signal.
func TestSweep_WriterErrorDoesNotBlockDetection(t *testing.T) {
	const (
		src   = "comet"
		asset = "fake-contract-18dp-writer-err"
	)
	before := counterVal(src, asset)

	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: src, Asset: asset}}}
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 18}}
	writer := &fakeWriter{err: errors.New("connection refused")}
	g := New(reader, resolver, Options{Window: time.Minute, Writer: writer})

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := counterVal(src, asset) - before; got != 1 {
		t.Fatalf("counter delta = %v, want 1 (detection unaffected by writer error)", got)
	}
	if writer.calls != 1 {
		t.Fatalf("writer.calls = %d, want 1 (attempted once despite the error)", writer.calls)
	}
}
