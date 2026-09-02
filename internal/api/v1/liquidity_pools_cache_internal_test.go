package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These pin the #332 F4 serving contract for the /v1/liquidity-pools ranked
// listing. Measured cause: nativeLPListing held nativeLPMu across the
// whole-`liquidity_pool`-prefix scan, so once every 60s TTL lapse the next
// caller paid that scan on its request deadline (live: 0.825 s) and every
// concurrent caller queued behind it — and with no prewarm anywhere, a cold
// process made the FIRST visitor pay it too.

// lpCacheReader is the narrow slice of ExplorerReader this cache uses: a
// countable, blockable, failable NativeLiquidityPoolsRanked.
type lpCacheReader struct {
	ExplorerReader // nil — only NativeLiquidityPoolsRanked is ever called here

	calls atomic.Int32
	fail  atomic.Bool
	// gate, when non-nil, holds the scan open until it is closed.
	gate chan struct{}
	// ledger identifies which fill produced a row.
	ledger atomic.Uint32
}

func (r *lpCacheReader) NativeLiquidityPoolsRanked(_ context.Context, _ int) ([]clickhouse.NativeLiquidityPoolState, error) {
	r.calls.Add(1)
	if r.gate != nil {
		<-r.gate
	}
	if r.fail.Load() {
		return nil, errors.New("lake down")
	}
	return []clickhouse.NativeLiquidityPoolState{{
		PoolHex: "deadbeef", PoolStrkey: "L" + "POOL",
		AssetA: "native", ReserveA: big.NewInt(10),
		AssetB: "native", ReserveB: big.NewInt(20),
		TotalShares: big.NewInt(30), Trustlines: 5, FeeBps: 30,
		Ledger: r.ledger.Load(),
	}}, nil
}

func newLPCacheServer(reader *lpCacheReader) *Server {
	return &Server{
		explorer: reader,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// backdateLPEntry ages the cached entry past its TTL.
func backdateLPEntry(s *Server, age time.Duration) {
	s.nativeLPMu.Lock()
	s.nativeLPFetched = time.Now().Add(-age)
	s.nativeLPMu.Unlock()
}

// waitFor polls cond until true or the deadline lapses.
func waitForLPCache(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// THE regression: once an entry exists, a caller arriving after the TTL has
// lapsed must be answered from that entry IMMEDIATELY while the rescan runs
// detached — it must not block on the scan.
func TestNativeLPListing_LapsedEntryIsServedWhileTheRescanRunsDetached(t *testing.T) {
	reader := &lpCacheReader{}
	reader.ledger.Store(63_000_000)
	s := newLPCacheServer(reader)

	rows, err := s.nativeLPListing(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("cold fill: rows=%d err=%v, want 1 row", len(rows), err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("cold fill ran %d scans, want 1", got)
	}

	// Age the entry out and hold the next scan open indefinitely.
	backdateLPEntry(s, 2*nativeLPListingTTL)
	reader.gate = make(chan struct{})
	reader.ledger.Store(63_500_000)

	// Off the test goroutine so an un-fixed build (which blocks on the
	// held-open scan) reports a clean failure instead of hanging.
	type lapsed struct {
		rows []LiquidityPoolReservesRow
		err  error
	}
	answered := make(chan lapsed, 1)
	go func() {
		r, e := s.nativeLPListing(context.Background())
		answered <- lapsed{r, e}
	}()
	var got lapsed
	select {
	case got = <-answered:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("lapsed read BLOCKED on the rescan — the existing entry was not served " +
			"(this is the #332 F4 defect)")
	}
	if got.err != nil {
		t.Fatalf("lapsed read: %v", got.err)
	}
	if len(got.rows) != 1 || got.rows[0].AsOfLedger != 63_000_000 {
		t.Fatalf("lapsed read served %+v, want the PREVIOUS entry's rows", got.rows)
	}

	// Further lapsed reads join the one in-flight rescan.
	for range 5 {
		if _, err := s.nativeLPListing(context.Background()); err != nil {
			t.Fatalf("repeat lapsed read: %v", err)
		}
	}
	waitForLPCache(t, "the detached rescan to start", func() bool { return reader.calls.Load() >= 2 })
	if got := reader.calls.Load(); got != 2 {
		t.Errorf("lapsed reads kicked %d rescans, want 1 (single-flight)", got-1)
	}

	// Release it (leave the channel in place, closed — reassigning the
	// field would race the in-flight refresh goroutine reading it).
	close(reader.gate)
	waitForLPCache(t, "the rescan to land", func() bool {
		s.nativeLPMu.Lock()
		defer s.nativeLPMu.Unlock()
		return len(s.nativeLPCached) == 1 && s.nativeLPCached[0].AsOfLedger == 63_500_000
	})
	rows, err = s.nativeLPListing(context.Background())
	if err != nil || len(rows) != 1 || rows[0].AsOfLedger != 63_500_000 {
		t.Fatalf("post-rescan read = %+v (err %v), want the rebuilt rows", rows, err)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Errorf("a fresh read rescanned (%d scans total)", got)
	}
}

// A failing rescan must leave the previous entry in place and keep serving
// it — old-but-real beats a 500 on a listing that changes slowly.
func TestNativeLPListing_FailedRescanKeepsTheLastGoodListing(t *testing.T) {
	reader := &lpCacheReader{}
	reader.ledger.Store(63_000_000)
	s := newLPCacheServer(reader)

	if _, err := s.nativeLPListing(context.Background()); err != nil {
		t.Fatalf("cold fill: %v", err)
	}
	backdateLPEntry(s, 2*nativeLPListingTTL)
	reader.fail.Store(true)

	rows, err := s.nativeLPListing(context.Background())
	if err != nil || len(rows) != 1 || rows[0].AsOfLedger != 63_000_000 {
		t.Fatalf("lapsed read during an outage = %+v (err %v), want the last-good rows", rows, err)
	}
	waitForLPCache(t, "the failing rescan to run", func() bool { return reader.calls.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)

	s.nativeLPMu.Lock()
	cached := s.nativeLPCached
	s.nativeLPMu.Unlock()
	if len(cached) != 1 || cached[0].AsOfLedger != 63_000_000 {
		t.Fatalf("a failed rescan destroyed the last-good entry: %+v", cached)
	}
}

// A stone-cold process still fills inline (there is nothing to serve), but
// concurrent cold callers must collapse onto ONE scan rather than each
// running their own.
func TestNativeLPListing_ConcurrentColdCallersRunOneScan(t *testing.T) {
	reader := &lpCacheReader{gate: make(chan struct{})}
	reader.ledger.Store(63_000_000)
	s := newLPCacheServer(reader)

	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := s.nativeLPListing(context.Background())
			done <- err
		}()
	}
	waitForLPCache(t, "the cold scan to start", func() bool { return reader.calls.Load() >= 1 })
	close(reader.gate)
	for range 8 {
		if err := <-done; err != nil {
			t.Fatalf("cold caller: %v", err)
		}
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("8 concurrent cold callers ran %d scans, want 1", got)
	}
}

// PrewarmNativeLiquidityPools must fill the exact entry the handler reads —
// after it runs, a request does zero scans. It goes through the handler's own
// function, so there is no prewarmed-key-vs-requested-key drift to get wrong.
func TestPrewarmNativeLiquidityPools_WarmsTheEntryTheHandlerReads(t *testing.T) {
	reader := &lpCacheReader{}
	reader.ledger.Store(63_000_000)
	s := newLPCacheServer(reader)

	s.PrewarmNativeLiquidityPools(context.Background())
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("prewarm ran %d scans, want 1", got)
	}

	rows, err := s.nativeLPListing(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("post-prewarm read: rows=%d err=%v", len(rows), err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("the request rescanned after prewarm (%d scans) — the prewarmed entry is not the one the handler reads", got)
	}

	// A second prewarm inside the TTL is a no-op.
	s.PrewarmNativeLiquidityPools(context.Background())
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("a warm prewarm rescanned (%d scans)", got)
	}

	// No explorer reader wired: safe no-op, never a nil dereference.
	(&Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).PrewarmNativeLiquidityPools(context.Background())
}
