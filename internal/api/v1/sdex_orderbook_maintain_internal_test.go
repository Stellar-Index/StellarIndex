package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// countingOfferBookReader counts the full loads and incremental reads the
// maintainer issues, on top of the shared stub's canned answers.
type countingOfferBookReader struct {
	stubOfferBookReader
	loads    int
	advances int
}

func (s *countingOfferBookReader) LoadLiveOffers(ctx context.Context) ([]clickhouse.LiveOffer, uint32, error) {
	s.loads++
	return s.stubOfferBookReader.LoadLiveOffers(ctx)
}

func (s *countingOfferBookReader) OfferChangesSince(ctx context.Context, from uint32) ([]clickhouse.OfferChange, uint32, error) {
	s.advances++
	return s.stubOfferBookReader.OfferChangesSince(ctx, from)
}

// TestSDEXOrderBookCache_MaintainTickReloadsPeriodically pins the
// maintainer policy the API process runs (F162): Load's "self-heal" re-load
// was documented but nothing ever called it, so a book that had gone wrong
// below its cursor stayed wrong until the process restarted.
func TestSDEXOrderBookCache_MaintainTickReloadsPeriodically(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &countingOfferBookReader{stubOfferBookReader: stubOfferBookReader{
		offers:     []clickhouse.LiveOffer{bookOffer("k1", 1, 100, "native", usdc, 1, 2, 10<<32|7)},
		cursor:     10,
		nextCursor: 10,
		loadErr:    errors.New("lake not up yet"),
	}}
	c := NewSDEXOrderBookCache(reader, nil)
	clock := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	// A failed INITIAL load is retried on the very next tick — until it
	// lands the endpoint is a 503 — and never advances an unloaded book.
	c.MaintainTick(ctx)
	clock = clock.Add(SDEXOrderBookAdvanceInterval)
	reader.loadErr = nil
	c.MaintainTick(ctx)
	if reader.loads != 2 || reader.advances != 0 {
		t.Fatalf("loads/advances = %d/%d, want 2/0: a failed initial load retries next tick, with no advance before it lands",
			reader.loads, reader.advances)
	}
	if _, ready := c.snapshotMarket("native", usdc); !ready {
		t.Fatal("book must be ready once the retried load lands")
	}

	// Steady state: every tick advances, none re-loads.
	for range 5 {
		clock = clock.Add(SDEXOrderBookAdvanceInterval)
		c.MaintainTick(ctx)
	}
	if reader.loads != 2 || reader.advances != 5 {
		t.Fatalf("loads/advances = %d/%d, want 2/5: steady-state ticks advance and do not re-load", reader.loads, reader.advances)
	}

	// The self-heal: once the last good load is a full interval old, the
	// tick advances AND rebuilds the book from the lake.
	reader.offers = []clickhouse.LiveOffer{bookOffer("k2", 2, 300, "native", usdc, 1, 2, 900<<32|4)}
	reader.cursor, reader.nextCursor = 900, 900
	clock = clock.Add(SDEXOrderBookReloadInterval)
	c.MaintainTick(ctx)
	if reader.loads != 3 {
		t.Fatalf("loads = %d, want 3: a book older than SDEXOrderBookReloadInterval must be re-loaded", reader.loads)
	}
	snap, _ := c.snapshotMarket("native", usdc)
	asks, cursor := snap.asks, snap.cursor
	if len(asks) != 1 || asks[0].KeyXDR != "k2" || cursor != 900 {
		t.Fatalf("after re-load asks=%+v cursor=%d, want exactly k2 at cursor 900 — the re-load replaces the book wholesale", asks, cursor)
	}

	// A FAILED re-load keeps the old book serving and backs off: it is not
	// retried every tick, only after SDEXOrderBookReloadRetry.
	reader.loadErr = errors.New("clickhouse struggling")
	clock = clock.Add(SDEXOrderBookReloadInterval)
	c.MaintainTick(ctx)
	if reader.loads != 4 {
		t.Fatalf("loads = %d, want 4: the due re-load is attempted", reader.loads)
	}
	if snap, ready := c.snapshotMarket("native", usdc); !ready || len(snap.asks) != 1 || snap.asks[0].KeyXDR != "k2" {
		t.Fatalf("after a failed re-load ready=%v asks=%+v, want the previous book (k2) still served", ready, snap.asks)
	}
	clock = clock.Add(SDEXOrderBookAdvanceInterval)
	c.MaintainTick(ctx)
	if reader.loads != 4 {
		t.Fatalf("loads = %d, want 4: a failed re-load must not be retried on the next tick", reader.loads)
	}
	clock = clock.Add(SDEXOrderBookReloadRetry)
	c.MaintainTick(ctx)
	if reader.loads != 5 {
		t.Fatalf("loads = %d, want 5: a failed re-load is retried after SDEXOrderBookReloadRetry", reader.loads)
	}
}

// TestSDEXOrderBookCache_ReloadKeepsVerificationVerdicts guards the
// periodic re-load against its own side effect: Load quarantines every
// intra_ledger_seq == 0 winner, so a naive daily re-load would pull every
// long-resting pre-intra-era offer OUT of the served book until the probe
// backlog drained again. An offer already served at the identical version
// keeps its verdict; anything else is still quarantined.
func TestSDEXOrderBookCache_ReloadKeepsVerificationVerdicts(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	live := bookOffer("live", 1, 100, "native", usdc, 1, 2, 40<<32) // intra 0 — suspect
	dead := bookOffer("dead", 2, 200, "native", usdc, 1, 3, 41<<32) // intra 0 — suspect, removal exists
	reader := &stubOfferBookReader{
		offers:  []clickhouse.LiveOffer{live, dead},
		cursor:  50,
		removed: map[string]struct{}{"dead": {}},
	}
	c := NewSDEXOrderBookCache(reader, nil)
	ctx := context.Background()
	if err := c.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap, _ := c.snapshotMarket("native", usdc); len(snap.asks) != 0 {
		t.Fatalf("first load serves %d asks, want 0: both suspects start quarantined", len(snap.asks))
	}
	if err := c.VerifyPending(ctx, SDEXOrderBookVerifyBatch); err != nil {
		t.Fatalf("VerifyPending: %v", err)
	}
	if snap, _ := c.snapshotMarket("native", usdc); len(snap.asks) != 1 || snap.asks[0].KeyXDR != "live" {
		t.Fatalf("after verify asks = %+v, want exactly the proven-live offer", snap.asks)
	}

	// Re-load reads the same rows again, plus a suspect the book has never
	// seen and one whose winning version MOVED since it was verified.
	fresh := bookOffer("fresh", 3, 300, "native", usdc, 1, 4, 45<<32)
	reader.offers = []clickhouse.LiveOffer{live, dead, fresh}
	if err := c.Load(ctx); err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	snap, _ := c.snapshotMarket("native", usdc)
	asks := snap.asks
	if len(asks) != 1 || asks[0].KeyXDR != "live" {
		t.Fatalf("after re-load asks = %+v, want exactly [live]: a verified offer at the same version stays "+
			"served (not re-quarantined), while the proven-dead and the never-verified suspects stay out", asks)
	}
	c.mu.RLock()
	_, deadQuarantined := c.pending["dead"]
	_, freshQuarantined := c.pending["fresh"]
	c.mu.RUnlock()
	if !deadQuarantined || !freshQuarantined {
		t.Fatalf("pending dead=%v fresh=%v, want both quarantined again", deadQuarantined, freshQuarantined)
	}

	moved := bookOffer("live", 1, 90, "native", usdc, 1, 2, 60<<32) // same key, NEW intra-0 version
	reader.offers = []clickhouse.LiveOffer{moved}
	if err := c.Load(ctx); err != nil {
		t.Fatalf("second re-Load: %v", err)
	}
	if snap, _ := c.snapshotMarket("native", usdc); len(snap.asks) != 0 {
		t.Fatalf("asks = %+v, want none: a verdict earned at version 40<<32 does not cover the key's new "+
			"version 60<<32 — that row is a fresh version-tie suspect", snap.asks)
	}
}
