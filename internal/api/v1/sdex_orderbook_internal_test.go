package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type stubOfferBookReader struct {
	offers  []clickhouse.LiveOffer
	cursor  uint32
	loadErr error

	changes    []clickhouse.OfferChange
	nextCursor uint32
	changesErr error

	lastFrom uint32
}

func (s *stubOfferBookReader) LoadLiveOffers(context.Context) ([]clickhouse.LiveOffer, uint32, error) {
	return s.offers, s.cursor, s.loadErr
}

func (s *stubOfferBookReader) OfferChangesSince(_ context.Context, from uint32) ([]clickhouse.OfferChange, uint32, error) {
	s.lastFrom = from
	return s.changes, s.nextCursor, s.changesErr
}

func bookOffer(key string, id, amount int64, selling, buying string, n, d int32, version uint64) clickhouse.LiveOffer {
	return clickhouse.LiveOffer{
		KeyXDR: key, OfferID: id, Amount: amount,
		Selling: selling, Buying: buying,
		PriceN: n, PriceD: d,
		Ledger: uint32(version >> 32), Version: version,
	}
}

func TestSDEXOrderBookCache_LoadAdvanceAndVersionDiscipline(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{
			bookOffer("k1", 1, 100, "native", usdc, 1, 2, 10<<32),
			bookOffer("k2", 2, 200, usdc, "native", 3, 1, 10<<32),
			bookOffer("k3", 3, 0, "native", usdc, 1, 1, 10<<32), // zero amount — dropped defensively
		},
		cursor: 10,
	}
	c := NewSDEXOrderBookCache(reader, nil)

	// Advance before Load is a safe no-op.
	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("pre-load Advance: %v", err)
	}
	if _, _, _, _, ready := c.snapshotPair("native", usdc); ready {
		t.Fatal("cache must not be ready before Load")
	}

	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	asks, bids, cursor, _, ready := c.snapshotPair("native", usdc)
	if !ready || cursor != 10 {
		t.Fatalf("ready=%v cursor=%d, want true/10", ready, cursor)
	}
	if len(asks) != 1 || len(bids) != 1 {
		t.Fatalf("asks/bids = %d/%d, want 1/1 (zero-amount k3 dropped)", len(asks), len(bids))
	}

	// Advance: update k1 (higher version), remove k2, and replay a STALE
	// lower-version change for k1 that must lose.
	reader.changes = []clickhouse.OfferChange{
		{
			KeyXDR: "k1", Version: 12 << 32, Ledger: 12,
			Offer: bookOffer("k1", 1, 150, "native", usdc, 1, 2, 12<<32),
		},
		{KeyXDR: "k2", Version: 12 << 32, Ledger: 12, Removed: true},
		{
			KeyXDR: "k1", Version: 11 << 32, Ledger: 11,
			Offer: bookOffer("k1", 1, 999, "native", usdc, 1, 2, 11<<32),
		},
	}
	reader.nextCursor = 12
	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if reader.lastFrom != 10 {
		t.Errorf("Advance queried from ledger %d, want 10", reader.lastFrom)
	}
	asks, bids, cursor, _, _ = c.snapshotPair("native", usdc)
	if cursor != 12 {
		t.Errorf("cursor = %d, want 12", cursor)
	}
	if len(bids) != 0 {
		t.Errorf("k2 removal not applied: %d bids remain", len(bids))
	}
	if len(asks) != 1 || asks[0].Amount != 150 {
		t.Errorf("k1 = %+v, want the version-12 amount 150 (stale version-11 replay must lose)", asks)
	}

	// A failing Advance surfaces the error and leaves the book intact.
	reader.changesErr = errors.New("boom")
	if err := c.Advance(context.Background()); err == nil {
		t.Fatal("Advance should surface the read error")
	}
	if asks, _, _, _, _ := c.snapshotPair("native", usdc); len(asks) != 1 {
		t.Error("book must be unchanged after a failed Advance")
	}
}

func TestAggregateOrderBookSide_ExactLevelsAndInversion(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

	// ASKS (sell native for USDC): two offers at the same reduced price
	// (1/2 and 2/4) must merge into one level; one at 3/4 sits above.
	asks := aggregateOrderBookSide([]clickhouse.LiveOffer{
		bookOffer("a1", 1, 10_000_000, "native", usdc, 1, 2, 1),
		bookOffer("a2", 2, 30_000_000, "native", usdc, 2, 4, 2),
		bookOffer("a3", 3, 20_000_000, "native", usdc, 3, 4, 3),
	}, false, 25)
	if len(asks) != 2 {
		t.Fatalf("ask levels = %d, want 2 (1/2 and 2/4 merge)", len(asks))
	}
	best := asks[0]
	if best.Price != "0.5000000" || best.PriceR.N != 1 || best.PriceR.D != 2 || best.Offers != 2 {
		t.Errorf("best ask = %+v, want price 0.5 (1/2) from 2 offers", best)
	}
	// 1 + 3 XLM at 0.5 → 4 base, 2 quote.
	if best.BaseAmount != "4.0000000" || best.QuoteAmount != "2.0000000" {
		t.Errorf("best ask amounts = %s/%s, want 4/2", best.BaseAmount, best.QuoteAmount)
	}
	// Cumulative through level 2: base 4+2=6; quote 2 + (2×3/4)=3.5.
	if asks[1].CumBaseAmount != "6.0000000" || asks[1].CumQuoteAmount != "3.5000000" {
		t.Errorf("cumulative = %s/%s, want 6/3.5", asks[1].CumBaseAmount, asks[1].CumQuoteAmount)
	}

	// BIDS (offers SELLING usdc FOR native): offer price N/D is
	// base-per-quote, so the level price is the exact inverse. An offer
	// of 30 USDC at 2/3 (XLM per USDC) → level price 3/2 USDC-per-XLM,
	// quote 30, base 30×2/3=20.
	bids := aggregateOrderBookSide([]clickhouse.LiveOffer{
		bookOffer("b1", 4, 300_000_000, usdc, "native", 2, 3, 4),
		bookOffer("b2", 5, 100_000_000, usdc, "native", 1, 3, 5),
	}, true, 25)
	if len(bids) != 2 {
		t.Fatalf("bid levels = %d, want 2", len(bids))
	}
	// Best bid = HIGHEST price first: 3/1 (from offer at 1/3) beats 3/2.
	if bids[0].Price != "3.0000000" || bids[0].PriceR.N != 3 || bids[0].PriceR.D != 1 {
		t.Errorf("best bid = %+v, want price 3 (3/1)", bids[0])
	}
	if bids[1].QuoteAmount != "30.0000000" || bids[1].BaseAmount != "20.0000000" {
		t.Errorf("bid level 2 = %s quote / %s base, want 30/20", bids[1].QuoteAmount, bids[1].BaseAmount)
	}

	// Depth cap: with depth=1 only the best level survives.
	if capped := aggregateOrderBookSide([]clickhouse.LiveOffer{
		bookOffer("a1", 1, 10_000_000, "native", usdc, 1, 2, 1),
		bookOffer("a3", 3, 20_000_000, "native", usdc, 3, 4, 3),
	}, false, 1); len(capped) != 1 || capped[0].Price != "0.5000000" {
		t.Errorf("depth cap wrong: %+v", capped)
	}
}

// TestSDEXOrderBookCache_MaintainObservesMetrics pins the op-qualified
// outcome labels: Load records load_ok/load_error, Advance records
// advance_ok/advance_error, and the pre-Load Advance no-op records
// NOTHING (counting it as ok would mask a stuck load behind a healthy
// advance rate).
func TestSDEXOrderBookCache_MaintainObservesMetrics(t *testing.T) {
	count := func(outcome string) uint64 {
		return obstest.HistogramSampleCount(t, obs.SDEXOrderBookMaintainDurationSeconds, "outcome", outcome)
	}
	loadOK, loadErr := count("load_ok"), count("load_error")
	advOK, advErr := count("advance_ok"), count("advance_error")

	reader := &stubOfferBookReader{loadErr: errors.New("lake down")}
	c := NewSDEXOrderBookCache(reader, nil)

	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("pre-load Advance: %v", err)
	}
	if got := count("advance_ok"); got != advOK {
		t.Errorf("pre-load Advance must not observe advance_ok (got %d, want %d)", got, advOK)
	}

	if err := c.Load(context.Background()); err == nil {
		t.Fatal("Load should surface the reader error")
	}
	if got := count("load_error"); got != loadErr+1 {
		t.Errorf("load_error observations = %d, want %d", got, loadErr+1)
	}

	reader.loadErr = nil
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := count("load_ok"); got != loadOK+1 {
		t.Errorf("load_ok observations = %d, want %d", got, loadOK+1)
	}

	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := count("advance_ok"); got != advOK+1 {
		t.Errorf("advance_ok observations = %d, want %d", got, advOK+1)
	}

	reader.changesErr = errors.New("lake down")
	if err := c.Advance(context.Background()); err == nil {
		t.Fatal("Advance should surface the reader error")
	}
	if got := count("advance_error"); got != advErr+1 {
		t.Errorf("advance_error observations = %d, want %d", got, advErr+1)
	}
}
