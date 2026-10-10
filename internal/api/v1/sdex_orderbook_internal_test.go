package v1

import (
	"context"
	"errors"
	"testing"
	"time"

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
	if _, ready := c.snapshotMarket("native", usdc); ready {
		t.Fatal("cache must not be ready before Load")
	}

	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap, ready := c.snapshotMarket("native", usdc)
	asks, bids, cursor := snap.asks, snap.bids, snap.cursor
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
	snap, _ = c.snapshotMarket("native", usdc)
	asks, bids, cursor = snap.asks, snap.bids, snap.cursor
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
	if snap, _ := c.snapshotMarket("native", usdc); len(snap.asks) != 1 {
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
// advance_ok/advance_error/advance_held, and the pre-Load Advance no-op
// records NOTHING (counting it as ok would mask a stuck load behind a
// healthy advance rate).
func TestSDEXOrderBookCache_MaintainObservesMetrics(t *testing.T) {
	count := func(outcome string) uint64 {
		return obstest.HistogramSampleCount(t, obs.SDEXOrderBookMaintainDurationSeconds, "outcome", outcome)
	}
	loadOK, loadErr := count("load_ok"), count("load_error")
	advOK, advErr, advHeld := count("advance_ok"), count("advance_error"), count("advance_held")

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

	// A held tick (next == cursor, no error — the reader's early return
	// below an unhealed lake hole) must NOT count as advance_ok: that
	// would mask a stuck cursor behind a healthy-looking advance rate,
	// exactly the gap that check closes. cursor is 0 post-Load here
	// (reader.cursor is unset); nextCursor defaults to the same 0.
	if err := c.Advance(context.Background()); err != nil {
		t.Fatalf("held Advance: %v", err)
	}
	if got := count("advance_held"); got != advHeld+1 {
		t.Errorf("advance_held observations = %d, want %d", got, advHeld+1)
	}
	if got := count("advance_ok"); got != advOK {
		t.Errorf("held Advance must not observe advance_ok (got %d, want %d)", got, advOK)
	}

	// A real advance — the cursor moves — records advance_ok.
	reader.nextCursor = 1
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

// TestSDEXOrderBookCache_ZombieQuarantineAndVerify pins the
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
	snap, _ := c.snapshotMarket("native", usdc)
	asks, bids := snap.asks, snap.bids
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
	snap, _ = c.snapshotMarket("native", usdc)
	asks, bids = snap.asks, snap.bids
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

// TestCrossedPairCount_TouchingIsNotCrossed pins the tripwire to the
// on-chain invariant: a PASSIVE offer rests against an opposite offer at
// exactly the inverse price (a 1:1 stablecoin maker is the common case),
// so a touching book is legal and must read 0 or the alert on this gauge
// fires forever. Only best bid > best ask is impossible on a resting book.
func TestCrossedPairCount_TouchingIsNotCrossed(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	touching := map[string]clickhouse.LiveOffer{
		"ask": bookOffer("ask", 1, 100, "native", usdc, 1, 2, 5<<32|1),
		"bid": bookOffer("bid", 2, 100, usdc, "native", 2, 1, 5<<32|2), // bid 0.5 == ask 0.5
	}
	if got := crossedPairs(touching); len(got) != 0 {
		t.Errorf("touching book crossed = %v, want none", got)
	}
	crossed := map[string]clickhouse.LiveOffer{
		"ask": touching["ask"],
		"bid": bookOffer("bid", 2, 100, usdc, "native", 19, 10, 5<<32|2), // bid 10/19 ≈ 0.526 > 0.5
	}
	if got := crossedPairs(crossed); len(got) != 1 || got[0] != usdc+"/native" {
		t.Errorf("strictly crossed book = %v, want [%s/native]", got, usdc)
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
	if snap, _ := c.snapshotMarket("native", usdc); snap.withheldBids != 1 {
		t.Errorf("post-Load withheld bids = %d, want 1", snap.withheldBids)
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
	if snap, _ := c.snapshotMarket("native", usdc); snap.withheldBids != 0 || len(c.withheld) != 0 {
		t.Errorf("post-Advance withheld bids = %d (markets %v), want 0", snap.withheldBids, c.withheld)
	}
	if got := testutil.ToFloat64(obs.SDEXOrderBookCrossedPairs); got != 1 {
		t.Errorf("crossed gauge = %v, want 1 (served book bid 0.8 vs ask 0.5)", got)
	}

	// Verification must not resurrect the superseded pre-load state.
	if err := c.VerifyPending(context.Background(), 10); err != nil {
		t.Fatalf("VerifyPending: %v", err)
	}
	snap, _ := c.snapshotMarket("native", usdc)
	bids := snap.bids
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

// TestSDEXOrderBookCache_MaintainTickReloadsPeriodically pins the
// maintainer policy the API process runs: Load's "self-heal" re-load
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
