package decimalsguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSweep_AllReadsFailedIsNotASuccess: a pass that could read nothing from the lake must not look like a clean one.
func TestSweep_AllReadsFailedIsNotASuccess(t *testing.T) {
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "comet", Asset: "blind-a"}, {Source: "comet", Asset: "blind-b"}}}
	g := New(reader, &fakeResolver{err: errors.New("clickhouse down")}, Options{Window: time.Minute})

	obs.DecimalsGuardSweepLastSuccessUnix.Set(1)
	if err := g.Sweep(context.Background()); err == nil {
		t.Fatal("a sweep whose every lake read failed returned nil")
	}
	if got := testutil.ToFloat64(obs.DecimalsGuardSweepLastSuccessUnix); got != 1 {
		t.Errorf("heartbeat = %v, want it left at 1", got)
	}
}

// TestBackfill_ReportsFailedReads: the wide pass says so when a token it enumerated went unread.
func TestBackfill_ReportsFailedReads(t *testing.T) {
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "comet", Asset: "bf-ok"}, {Source: "comet", Asset: "bf-unread"}}}
	res := &selectiveResolver{
		inner:  &fakeResolver{decimals: map[string]uint32{"bf-ok": 7}},
		errFor: map[string]error{"bf-unread": errors.New("timeout")},
	}
	g := New(reader, res, Options{BackfillThrottle: time.Nanosecond})
	if err := g.Backfill(context.Background()); err == nil {
		t.Fatal("a backfill with a failed lake read returned nil")
	}
}

// TestTick_RetriesStrandedWriteWithoutEnumeration: a dormant offender is enumerated once; its failed write must not wait for
// a second enumeration that never comes.
func TestTick_RetriesStrandedWriteWithoutEnumeration(t *testing.T) {
	ref := timescale.SorobanDEXTradeRef{Source: "soroswap", Asset: "dormant-18dp-stranded"}
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{ref}}
	writer := &fakeWriter{err: errors.New("connection refused")}
	g := New(reader, &fakeResolver{decimals: map[string]uint32{ref.Asset: 18}}, Options{Writer: writer, BackfillThrottle: time.Nanosecond})

	if err := g.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DecimalsGuardPendingWrites); got != 1 {
		t.Fatalf("pending gauge = %v, want 1", got)
	}

	reader.refs = nil // the token is outside every later sweep window
	writer.err = nil
	g.tick(context.Background(), "periodic")
	if writer.calls != 2 || writer.last.asset != ref.Asset || writer.last.decimals != 18 {
		t.Fatalf("writer calls=%d last=%+v, want the stranded write retried", writer.calls, writer.last)
	}
	if got := testutil.ToFloat64(obs.DecimalsGuardPendingWrites); got != 0 {
		t.Errorf("pending gauge = %v, want 0 after the write landed", got)
	}
	g.tick(context.Background(), "periodic")
	if writer.calls != 2 {
		t.Errorf("writer calls = %d, want no further write once it landed", writer.calls)
	}
}

// TestUnresolvedAssets_CountedUntilConfirmed: "could not verify" is exported, and stops being counted once the lake answers.
func TestUnresolvedAssets_CountedUntilConfirmed(t *testing.T) {
	const asset = "no-metadata-yet"
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "phoenix", Asset: asset}}}
	resolver := &fakeResolver{decimals: map[string]uint32{}}
	g := New(reader, resolver, Options{Window: time.Minute})

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DecimalsGuardUnresolvedAssets); got != 1 {
		t.Fatalf("unresolved gauge = %v, want 1", got)
	}
	resolver.decimals[asset] = 7
	if err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DecimalsGuardUnresolvedAssets); got != 0 {
		t.Errorf("unresolved gauge = %v, want 0 once confirmed", got)
	}
}

// lateIngestReader returns its ref only to a window wider than a sweep's, as a trade ingested hours after its ledger time would.
type lateIngestReader struct {
	mu   sync.Mutex
	ref  timescale.SorobanDEXTradeRef
	wide int
}

func (l *lateIngestReader) RecentSorobanDEXTrades(_ context.Context, since time.Time) ([]timescale.SorobanDEXTradeRef, error) {
	if time.Since(since) < time.Hour {
		return nil, nil
	}
	l.mu.Lock()
	l.wide++
	l.mu.Unlock()
	return []timescale.SorobanDEXTradeRef{l.ref}, nil
}

// TestRun_ReenumeratesTheWideWindow: Run itself repeats the Backfill enumeration; nothing else examines a late-ingested trade.
func TestRun_ReenumeratesTheWideWindow(t *testing.T) {
	ref := timescale.SorobanDEXTradeRef{Source: "aquarius", Asset: "late-ingested-9dp"}
	reader := &lateIngestReader{ref: ref}
	writer := &syncWriter{landed: make(chan string, 4)}
	g := New(reader, &fakeResolver{decimals: map[string]uint32{ref.Asset: 9}}, Options{Writer: writer, BackfillThrottle: time.Nanosecond})
	g.reenumerateEvery = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = g.Run(ctx, time.Hour) }()
	select {
	case got := <-writer.landed:
		if got != ref.Asset {
			t.Errorf("persisted %q, want %q", got, ref.Asset)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run never re-enumerated the wide window")
	}
	cancel()
	<-done
}

type syncWriter struct{ landed chan string }

func (s *syncWriter) UpsertNonstandardDecimalsAsset(_ context.Context, asset string, _ uint32, _ string) error {
	s.landed <- asset
	return nil
}

// TestSeededAsset_LeavesTheUnresolvedCount: hand-seeding is the runbook's fix for an asset the lake cannot read, so it must clear the count.
func TestSeededAsset_LeavesTheUnresolvedCount(t *testing.T) {
	const asset = "fixed-in-code-18dp"
	reader := &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "phoenix", Asset: asset}}}
	store := newFakeStore()
	g := New(reader, &fakeResolver{decimals: map[string]uint32{}}, Options{Window: time.Minute, Writer: store})

	g.tick(context.Background(), "periodic")
	if got := testutil.ToFloat64(obs.DecimalsGuardUnresolvedAssets); got != 1 {
		t.Fatalf("unresolved gauge = %v, want 1 before the seed", got)
	}
	store.rows[asset] = timescale.NonstandardDecimalsAsset{Asset: asset, Decimals: 18}
	g.tick(context.Background(), "periodic")
	g.tick(context.Background(), "periodic")
	if got := testutil.ToFloat64(obs.DecimalsGuardUnresolvedAssets); got != 0 {
		t.Errorf("unresolved gauge = %v, want 0 once the asset is seeded", got)
	}
}
