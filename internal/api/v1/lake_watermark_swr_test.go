package v1

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// slowWatermark models the PRODUCTION lake reader's shape rather than a
// convenient one: clickhouse.ExplorerReader.LakeWatermark is a plain
// QueryRow with no internal deadline (its only bound is the connection's
// max_execution_time=30 / ReadTimeout=30s), so a slow lake keeps it in the
// read long after the caller that started it has given up. delay therefore
// elapses regardless of the context handed in.
type slowWatermark struct {
	mu       sync.Mutex
	calls    int
	finished int

	delay    time.Duration
	ledger   uint32
	closedAt time.Time
	err      error
}

func (s *slowWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	s.mu.Lock()
	s.calls++
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return s.ledger, s.closedAt, s.err
}

func (s *slowWatermark) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *slowWatermark) finishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// TestLakeWatermark_SlowLakeDoesNotSerialiseCallers (RLT-095). The watermark
// refresh must never run under the process-global lakeWMMu: every lake-backed
// route calls lakeWatermark (pools_reserves, asset_supply ×3,
// liquidity_pools ×2, lending, plus the three explorer account-state sites),
// so a single slow ClickHouse round-trip inside the critical section makes
// every concurrent request queue behind it on a non-context-aware mutex and
// burn its own deadline. Each caller must come back within ITS OWN budget,
// and the burst must coalesce onto one read.
func TestLakeWatermark_SlowLakeDoesNotSerialiseCallers(t *testing.T) {
	const (
		callers   = 8
		lakeDelay = 1200 * time.Millisecond
		budget    = 300 * time.Millisecond
		slack     = 400 * time.Millisecond
	)
	wm := &slowWatermark{delay: lakeDelay, ledger: 100, closedAt: time.Now()}
	s := &Server{lakeWatermarkReader: wm, logger: slog.Default()}

	returned := make(chan time.Duration, callers)
	start := time.Now()
	for i := 0; i < callers; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			s.lakeWatermark(ctx)
			returned <- time.Since(start)
		}()
	}

	overall := time.After(budget + slack)
	for i := 0; i < callers; i++ {
		select {
		case d := <-returned:
			if d > budget+slack {
				t.Errorf("caller returned after %s, past its %s deadline — the watermark refresh is running on the request path", d, budget)
			}
		case <-overall:
			t.Fatalf("only %d of %d concurrent callers returned within %s while one lake read was in flight — requests are serialised behind the watermark lock (RLT-095)",
				i, callers, budget+slack)
		}
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads for %d concurrent cold callers = %d, want 1 (the refresh must be coalesced, not repeated per caller)", callers, got)
	}
}

// TestLakeWatermark_ColdFailureIsRateLimited (RLT-095). A failed read must
// stamp a retry gap. Without one, every subsequent request re-enters the
// read — against a wedged lake (whose read is bounded only by ClickHouse's
// 30s ReadTimeout) that is a back-to-back retry train, one per request.
func TestLakeWatermark_ColdFailureIsRateLimited(t *testing.T) {
	wm := &slowWatermark{err: errors.New("lake down")}
	s := &Server{lakeWatermarkReader: wm, logger: slog.Default()}

	for i := 0; i < 4; i++ {
		if _, _, ok := s.lakeWatermark(context.Background()); ok {
			t.Fatalf("call %d reported a watermark though every read failed", i)
		}
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads across 4 requests after a failed read = %d, want 1 (a failure must back off for the retry gap, not retry per request)", got)
	}
}

// TestLakeWatermark_LapsedEntryServedWithoutWaitingOnTheLake (RLT-095). Once
// the TTL lapses the cached watermark is still perfectly serviceable — its
// close time only gets older, which is exactly what flags.stale reads — so
// the caller that happens to notice the lapse must be served from cache
// immediately while the refresh runs detached, and a failed refresh must
// neither drop the last-good value nor re-arm an immediate retry.
func TestLakeWatermark_LapsedEntryServedWithoutWaitingOnTheLake(t *testing.T) {
	wm := &slowWatermark{delay: 400 * time.Millisecond, err: errors.New("lake down")}
	s := &Server{lakeWatermarkReader: wm, logger: slog.Default()}
	// The state every request lands in once the TTL window lapses.
	s.lakeWMLedger, s.lakeWMClosedAt = 100, time.Now()
	s.lakeWMFetched = time.Now().Add(-2 * lakeWatermarkTTL)

	start := time.Now()
	ledger, stale, ok := s.lakeWatermark(context.Background())
	elapsed := time.Since(start)
	if !ok || ledger != 100 || stale {
		t.Fatalf("lapsed watermark = (%d, stale=%v, ok=%v), want (100, false, true)", ledger, stale, ok)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("serving a lapsed watermark took %s while the lake read was slow — the refresh must not run on the caller's path (RLT-095)", elapsed)
	}

	// Let the detached refresh finish (and fail).
	deadline := time.Now().Add(3 * time.Second)
	for wm.finishedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if wm.finishedCount() == 0 {
		t.Fatal("the detached refresh never ran")
	}
	if _, _, ok := s.lakeWatermark(context.Background()); !ok {
		t.Fatal("last-good watermark dropped after a failed refresh")
	}
	if got := wm.callCount(); got != 1 {
		t.Errorf("lake reads = %d, want 1 — a failed refresh must back off for the retry gap", got)
	}
}

// TestLakeWatermark_UnmeasuredFailsClosed: every lake-backed route discards ok
// and serves flags.stale straight from lakeWatermark, so a wired reader with
// no measurement must report stale=true — "unknown" served as fresh is the
// defect. Only an unwired reader (no lake to judge) stays stale=false.
func TestLakeWatermark_UnmeasuredFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		wm        LakeWatermarkReader
		wantStale bool
	}{
		{"cold read failed", &slowWatermark{err: errors.New("lake down")}, true},
		{"lake empty", &slowWatermark{}, true},
		{"no reader wired", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{lakeWatermarkReader: tc.wm, logger: slog.Default()}
			ledger, stale, ok := s.lakeWatermark(context.Background())
			if ok || ledger != 0 {
				t.Fatalf("watermark = (%d, ok=%v), want (0, false) with no measurement", ledger, ok)
			}
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}

// TestAssetSupply_UnmeasuredWatermarkServesStale drives the same property
// through a route: the lake is wired but its watermark read fails, so the
// supply response must carry flags.stale=true and no as_of_ledger.
func TestAssetSupply_UnmeasuredWatermarkServesStale(t *testing.T) {
	f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
		ContractID: supplyContractID,
		Total:      big.NewInt(9), Mint: big.NewInt(9), Burn: big.NewInt(0), Clawback: big.NewInt(0),
		FlowCount: 1,
	}}
	rec := serveSupplyWM(t, f, nil, &stubWatermark{err: errors.New("lake down")}, supplyContractID)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
	if got.AsOfLedger != 0 || !flags.Stale {
		t.Errorf("as_of_ledger = %d stale = %v, want 0/true when the wired lake watermark is unmeasured", got.AsOfLedger, flags.Stale)
	}
}
