package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

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

	removed   map[string]struct{} // keys OfferRemovedAt reports dead
	verifyErr error

	lastFrom uint32
	lastRefs []clickhouse.OfferRemovalRef
}

func (s *stubOfferBookReader) LoadLiveOffers(context.Context) ([]clickhouse.LiveOffer, uint32, error) {
	return s.offers, s.cursor, s.loadErr
}

func (s *stubOfferBookReader) OfferChangesSince(_ context.Context, from uint32) ([]clickhouse.OfferChange, uint32, error) {
	s.lastFrom = from
	return s.changes, s.nextCursor, s.changesErr
}

func (s *stubOfferBookReader) OfferRemovedAt(_ context.Context, refs []clickhouse.OfferRemovalRef) (map[string]struct{}, error) {
	s.lastRefs = refs
	if s.verifyErr != nil {
		return nil, s.verifyErr
	}
	dead := map[string]struct{}{}
	for _, ref := range refs {
		if _, ok := s.removed[ref.KeyXDR]; ok {
			dead[ref.KeyXDR] = struct{}{}
		}
	}
	return dead, nil
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
	// Versions carry a nonzero intra_ledger_seq — intra 0 marks the
	// version-tie quarantine class, covered by its own test below.
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{
			bookOffer("k1", 1, 100, "native", usdc, 1, 2, 10<<32|7),
			bookOffer("k2", 2, 200, usdc, "native", 3, 1, 10<<32|8),
			bookOffer("k3", 3, 0, "native", usdc, 1, 1, 10<<32|9), // zero amount — dropped defensively
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

// TestSDEXOrderBookCache_ZombieQuarantineAndVerify pins the 2026-07-31
// crossed-book fix: a loaded offer whose version carries
// intra_ledger_seq == 0 (the version-tie-ambiguous class — its
// same-ledger `removed` sibling may have lost the ReplacingMergeTree
// merge arbitrarily) is NOT served until the change-stream probe
// proves no removal exists at its ledger. Proven-dead zombies are
// dropped for good; proven-live offers graduate into the served book.
func TestSDEXOrderBookCache_ZombieQuarantineAndVerify(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{
			// Trusted modern ask at 0.5 USDC/XLM (nonzero intra).
			bookOffer("ask", 1, 100, "native", usdc, 1, 2, 63_000_000<<32|41),
			// Zombie bid from 2021: sells USDC at 2.31 XLM/USDC → implied
			// bid 0.4327 USDC/XLM, CROSSING the 0.5 ask... except it was
			// consumed on-chain; only a version tie kept it "live".
			bookOffer("zombie", 2, 500, usdc, "native", 5777499, 2500000, 38_224_736<<32),
			// Genuine old resting bid at 4 XLM/USDC → 0.25 USDC/XLM (not
			// crossing) — also intra 0, must come back after verification.
			bookOffer("resting", 3, 700, usdc, "native", 4, 1, 40_000_000<<32),
		},
		cursor:  63_000_001,
		removed: map[string]struct{}{"zombie": {}},
	}
	c := NewSDEXOrderBookCache(reader, nil)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Quarantine: only the trusted ask is served; the intra-0 offers are
	// pending; the served book is NOT crossed (the zombie is unserved).
	asks, bids, _, _, _ := c.snapshotPair("native", usdc)
	if len(asks) != 1 || len(bids) != 0 {
		t.Fatalf("post-Load asks/bids = %d/%d, want 1/0 (intra-0 offers quarantined)", len(asks), len(bids))
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookPendingOffers); got != 2 {
		t.Errorf("pending gauge = %v, want 2", got)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookCrossedPairs); got != 0 {
		t.Errorf("crossed gauge = %v, want 0 (zombie must not be served)", got)
	}

	// Verify: the zombie is proven dead and discarded; the genuine
	// resting offer graduates into the served book.
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("VerifyPending: %v", err)
	}
	if len(reader.lastRefs) != 2 {
		t.Fatalf("verify probed %d refs, want 2", len(reader.lastRefs))
	}
	asks, bids, _, _, _ = c.snapshotPair("native", usdc)
	if len(asks) != 1 || len(bids) != 1 {
		t.Fatalf("post-verify asks/bids = %d/%d, want 1/1 (zombie dead, resting restored)", len(asks), len(bids))
	}
	if bids[0].KeyXDR != "resting" {
		t.Errorf("served bid = %q, want the genuine resting offer", bids[0].KeyXDR)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookPendingOffers); got != 0 {
		t.Errorf("pending gauge = %v, want 0 after the drain", got)
	}

	// Empty-quarantine verify is a no-op that touches nothing.
	reader.lastRefs = nil
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("empty VerifyPending: %v", err)
	}
	if reader.lastRefs != nil {
		t.Error("empty quarantine must not probe the lake")
	}
}

// TestSDEXOrderBookCache_AdvanceSupersedesPendingAndCrossedGauge pins
// two behaviours: (1) a tip-forward change resolves a quarantined
// key's fate without waiting for verification, and (2) the
// crossed-pairs gauge fires when the SERVED book itself is crossed
// (the residual class verification cannot disprove).
func TestSDEXOrderBookCache_AdvanceSupersedesPendingAndCrossedGauge(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{
			bookOffer("ask", 1, 100, "native", usdc, 1, 2, 63_000_000<<32|3), // 0.5 USDC/XLM
			bookOffer("suspect", 2, 500, usdc, "native", 4, 1, 40_000_000<<32),
		},
		cursor: 63_000_001,
	}
	c := NewSDEXOrderBookCache(reader, nil)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A live change for the suspect key supersedes its quarantined
	// pre-load state: here a fresh, CROSSING bid (sells USDC at 1.25
	// XLM/USDC → implied bid 0.8 USDC/XLM >= 0.5 ask).
	reader.changes = []clickhouse.OfferChange{{
		KeyXDR: "suspect", Version: 63_000_002 << 32, Ledger: 63_000_002,
		Offer: bookOffer("suspect", 2, 500, usdc, "native", 5, 4, 63_000_002<<32),
	}}
	reader.nextCursor = 63_000_002
	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookPendingOffers); got != 0 {
		t.Errorf("pending gauge = %v, want 0 (Advance resolves the suspect)", got)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookCrossedPairs); got != 1 {
		t.Errorf("crossed gauge = %v, want 1 (served book bid 0.8 vs ask 0.5)", got)
	}

	// Verification must not resurrect the superseded pre-load state.
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("VerifyPending: %v", err)
	}
	_, bids, _, _, _ := c.snapshotPair("native", usdc)
	if len(bids) != 1 || bids[0].Version != 63_000_002<<32 {
		t.Fatalf("bids = %+v, want only the Advance-applied version", bids)
	}

	// The crossing clears when the stale bid is removed.
	reader.changes = []clickhouse.OfferChange{{KeyXDR: "suspect", Version: 63_000_003 << 32, Ledger: 63_000_003, Removed: true}}
	reader.nextCursor = 63_000_003
	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookCrossedPairs); got != 0 {
		t.Errorf("crossed gauge = %v, want 0 after the crossing bid is removed", got)
	}
}

// TestSDEXOrderBookCache_VerifyObservesMetrics pins the verify_ok /
// verify_error maintain outcomes; an empty-quarantine tick observes
// nothing (steady state must not drown the load/advance signal).
func TestSDEXOrderBookCache_VerifyObservesMetrics(t *testing.T) {
	count := func(outcome string) uint64 {
		return obstest.HistogramSampleCount(t, obs.SDEXOrderBookMaintainDurationSeconds, "outcome", outcome)
	}
	vOK, vErr := count("verify_ok"), count("verify_error")

	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{bookOffer("s1", 1, 100, usdc, "native", 4, 1, 40_000_000<<32)},
		cursor: 63_000_001,
	}
	c := NewSDEXOrderBookCache(reader, nil)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	reader.verifyErr = errors.New("lake down")
	if err := c.VerifyPending(context.Background(), 10); err == nil {
		t.Fatal("VerifyPending should surface the probe error")
	}
	if got := count("verify_error"); got != vErr+1 {
		t.Errorf("verify_error observations = %d, want %d", got, vErr+1)
	}

	reader.verifyErr = nil
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("VerifyPending: %v", err)
	}
	if got := count("verify_ok"); got != vOK+1 {
		t.Errorf("verify_ok observations = %d, want %d", got, vOK+1)
	}

	// Quarantine now empty — a further verify observes nothing.
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("empty VerifyPending: %v", err)
	}
	if got := count("verify_ok"); got != vOK+1 {
		t.Errorf("empty verify must not observe (got %d, want %d)", got, vOK+1)
	}
}
