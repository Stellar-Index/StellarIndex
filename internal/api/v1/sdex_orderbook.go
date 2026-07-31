package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// GET /v1/sdex/orderbook — live classic order-book depth for one
// (selling, buying) pair, served from an in-process book maintained
// off the certified lake.
//
// Why in-process: offer entries in the lake carry their assets only
// inside the entry_xdr blob (no selling/buying columns), so a per-pair
// read would decode the whole ~1.02B-row offer slice per request. The
// book is instead loaded once at process start and advanced with
// partition-pruned incremental change reads — see
// clickhouse/sdex_offer_book_reader.go for the full trade-off note.

// SDEXOrderBookAdvanceInterval is the cadence the background goroutine
// in cmd/stellarindex-api/main.go advances the live book at. 60s keeps
// the incremental reads to a handful of recent partitions while
// bounding staleness at about the freshness /v1/price's closed-bucket
// contract already serves.
const SDEXOrderBookAdvanceInterval = 60 * time.Second

// sdexOrderBookDefaultDepth / MaxDepth bound the served price levels
// per side.
const (
	sdexOrderBookDefaultDepth = 25
	sdexOrderBookMaxDepth     = 200
)

// SDEXOrderBookVerifyBatch bounds one [SDEXOrderBookCache.VerifyPending]
// sweep (called once per advance tick by the maintainer goroutine in
// cmd/stellarindex-api/main.go). Each pending offer costs one
// partition-pruned (ledger, key) probe against ledger_entry_changes —
// ~1.5 ms/key measured on r1 (2026-07-31) — so a 2,500 batch is ~4 s
// of the 60 s tick, and a fully zombie-laden book (~10^6 suspects)
// converges in hours, not days. The quarantined suspects are NOT
// served meanwhile, so the book is honest from the first snapshot;
// verification only widens it back out with proven-live offers.
const SDEXOrderBookVerifyBatch = 2500

// SDEXOfferBookReader is the lake seam the order-book cache maintains
// itself from. Production wiring is *clickhouse.ExplorerReader.
type SDEXOfferBookReader interface {
	LoadLiveOffers(ctx context.Context) ([]clickhouse.LiveOffer, uint32, error)
	OfferChangesSince(ctx context.Context, fromLedger uint32) ([]clickhouse.OfferChange, uint32, error)
	// OfferRemovedAt reports which refs have a `removed` change row at
	// their own winning ledger — the version-tie zombie probe (see the
	// clickhouse implementation for the full defect narrative).
	OfferRemovedAt(ctx context.Context, refs []clickhouse.OfferRemovalRef) (map[string]struct{}, error)
}

// SDEXOrderBookCache is the in-process live offer book: a map of every
// live classic offer, loaded once ([Load]) and advanced incrementally
// ([Advance]) by a background goroutine. Handlers aggregate per-pair
// depth from the map under an RLock (tens of thousands of offers —
// microseconds). Before the first Load completes the handler serves an
// honest 503 "snapshot loading" problem, never fabricated emptiness.
//
// Trust discipline (the 2026-07-31 crossed-book fix): a loaded offer
// whose winning version carries intra_ledger_seq == 0 is AMBIGUOUS —
// either a pre-intra-era backfill row whose same-ledger siblings
// (including a possible removal) lost the ReplacingMergeTree version
// tie arbitrarily, or a modern row that happens to be its ledger's
// first change. Such offers are quarantined in `pending` — excluded
// from every served snapshot — until [VerifyPending] proves against
// the ledger_entry_changes stream that no removal exists at their
// ledger. Proven-dead offers are dropped for good (offer LedgerKeys
// are never reused); proven-live ones graduate into the served book.
// Without this, ~46k phantom XLM/USDC bids from 2021 served a crossed
// book (best bid 0.4327 vs best ask 0.1722).
type SDEXOrderBookCache struct {
	reader SDEXOfferBookReader
	logger Logger

	mu       sync.RWMutex
	offers   map[string]clickhouse.LiveOffer // served book; key = LedgerKey XDR
	pending  map[string]clickhouse.LiveOffer // quarantined suspects awaiting verification
	crossed  int                             // last computed crossed-pair count (for change-only logging)
	cursor   uint32                          // ledger_entry_changes high-water applied
	updated  time.Time
	loadedOK bool
}

// NewSDEXOrderBookCache constructs an empty (not-ready) cache.
func NewSDEXOrderBookCache(reader SDEXOfferBookReader, logger Logger) *SDEXOrderBookCache {
	return &SDEXOrderBookCache{reader: reader, logger: logger}
}

// observeMaintain records one Load/Advance/VerifyPending attempt
// against the paired order-book maintenance metrics. op is "load",
// "advance" or "verify"; the outcome label is op-qualified (load_ok,
// advance_error, verify_ok, …) so the cost classes chart apart.
func observeMaintain(op string, start time.Time, err error) {
	outcome := op + "_ok"
	if err != nil {
		outcome = op + "_error"
	}
	obs.SDEXOrderBookMaintainTotal.WithLabelValues(outcome).Inc()
	obs.SDEXOrderBookMaintainDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// Load performs the initial full-book load. Idempotent — a re-Load
// replaces the book wholesale (used as a self-heal if Advance ever
// falls persistently behind).
func (c *SDEXOrderBookCache) Load(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { observeMaintain("load", start, err) }()
	offers, cursor, err := c.reader.LoadLiveOffers(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("sdex order book load", "err", err)
		}
		return err
	}
	book := make(map[string]clickhouse.LiveOffer, len(offers))
	for _, o := range offers {
		if o.Amount <= 0 || o.PriceN <= 0 || o.PriceD <= 0 {
			continue // defensive: a live offer is always positive
		}
		if prev, dup := book[o.KeyXDR]; dup && prev.Version >= o.Version {
			continue
		}
		book[o.KeyXDR] = o
	}
	// Partition winners into the served book and the version-tie
	// quarantine (intra_ledger_seq == 0 — see the type comment).
	// Seed-snapshot rows carry intra 0xFFFFFFFF and stay trusted.
	served := make(map[string]clickhouse.LiveOffer, len(book))
	pending := make(map[string]clickhouse.LiveOffer)
	for k, o := range book {
		if o.Version&0xFFFFFFFF == 0 {
			pending[k] = o
			continue
		}
		served[k] = o
	}
	c.mu.Lock()
	c.offers = served
	c.pending = pending
	c.cursor = cursor
	c.updated = time.Now().UTC()
	c.loadedOK = true
	c.updateHealthGaugesLocked()
	c.mu.Unlock()
	return nil
}

// VerifyPending drains up to limit quarantined offers by probing the
// lake's change stream for a removal at each offer's own ledger.
// Proven-dead offers are discarded permanently; the rest graduate into
// the served book. No-op (and unobserved — steady state has nothing to
// verify) when the quarantine is empty.
func (c *SDEXOrderBookCache) VerifyPending(ctx context.Context, limit int) (err error) {
	c.mu.RLock()
	refs := make([]clickhouse.OfferRemovalRef, 0, min(limit, len(c.pending)))
	for k, o := range c.pending {
		if len(refs) >= limit {
			break
		}
		refs = append(refs, clickhouse.OfferRemovalRef{KeyXDR: k, Ledger: o.Ledger})
	}
	c.mu.RUnlock()
	if len(refs) == 0 {
		return nil
	}
	start := time.Now()
	defer func() { observeMaintain("verify", start, err) }()
	dead, err := c.reader.OfferRemovedAt(ctx, refs)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("sdex order book verify", "err", err)
		}
		return err
	}
	c.mu.Lock()
	for _, ref := range refs {
		o, ok := c.pending[ref.KeyXDR]
		if !ok || o.Ledger != ref.Ledger {
			continue // superseded by an Advance since the refs were drawn
		}
		delete(c.pending, ref.KeyXDR)
		if _, gone := dead[ref.KeyXDR]; gone {
			continue // proven dead — never serve, never re-check
		}
		if prev, dup := c.offers[ref.KeyXDR]; dup && prev.Version >= o.Version {
			continue
		}
		c.offers[ref.KeyXDR] = o
	}
	c.updateHealthGaugesLocked()
	c.mu.Unlock()
	return nil
}

// Advance applies offer changes since the cursor. Changes are applied
// by version (>= wins), so overlapping or duplicated change rows are
// idempotent. No-op (nil) before Load has succeeded.
func (c *SDEXOrderBookCache) Advance(ctx context.Context) (err error) {
	c.mu.RLock()
	ready, cursor := c.loadedOK, c.cursor
	c.mu.RUnlock()
	if !ready {
		// Deliberately unobserved: pre-Load ticks do no work, and
		// counting them as advance_ok would mask a stuck load behind a
		// healthy-looking advance rate.
		return nil
	}
	start := time.Now()
	defer func() { observeMaintain("advance", start, err) }()
	changes, next, err := c.reader.OfferChangesSince(ctx, cursor)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("sdex order book advance", "err", err)
		}
		return err
	}
	c.mu.Lock()
	for _, ch := range changes {
		// Any tip-forward change supersedes a quarantined pre-load
		// state: the live stream's version ordering is trustworthy, so
		// the key's fate is decided below, not by verification.
		delete(c.pending, ch.KeyXDR)
		prev, exists := c.offers[ch.KeyXDR]
		if exists && prev.Version > ch.Version {
			continue // stale duplicate
		}
		if ch.Removed || ch.Offer.Amount <= 0 || ch.Offer.PriceN <= 0 || ch.Offer.PriceD <= 0 {
			delete(c.offers, ch.KeyXDR)
			continue
		}
		c.offers[ch.KeyXDR] = ch.Offer
	}
	c.cursor = next
	c.updated = time.Now().UTC()
	c.updateHealthGaugesLocked()
	c.mu.Unlock()
	return nil
}

// updateHealthGaugesLocked recomputes the book's data-quality gauges.
// Caller holds mu.
//
// The crossed-pairs count is the invariant tripwire this cache exists
// to keep at zero: Stellar's DEX executes crossing offers at
// submission, so a RESTING book can never have best bid >= best ask.
// Any sustained non-zero value means phantom offers are being served —
// e.g. the residual zombie class whose removal was never ingested at
// all (so [VerifyPending] cannot disprove them).
func (c *SDEXOrderBookCache) updateHealthGaugesLocked() {
	crossed := crossedPairCount(c.offers)
	if crossed != c.crossed && c.logger != nil {
		c.logger.Warn("sdex order book crossed-pair count changed",
			"crossed_pairs", crossed, "was", c.crossed, "pending_offers", len(c.pending))
	}
	c.crossed = crossed
	obs.SDEXOrderBookCrossedPairs.Set(float64(crossed))
	obs.SDEXOrderBookPendingOffers.Set(float64(len(c.pending)))
}

// minPriceRat returns the smaller of the rationals cur = curN/curD and
// n/d by exact cross-multiplication (curD == 0 means "unset"). Price
// components are int32, so the products fit comfortably in int64.
func minPriceRat(curN, curD, n, d int64) (int64, int64) {
	if curD == 0 || n*curD < curN*d {
		return n, d
	}
	return curN, curD
}

// crossedPairCount counts (unordered) asset pairs whose book is
// crossed: best bid >= best ask.
func crossedPairCount(offers map[string]clickhouse.LiveOffer) int {
	// Per unordered pair, track the best (minimum) offer price of each
	// direction. For direction A→B an offer's N/D is B-per-A.
	type bestPrices struct {
		fwdN, fwdD int64 // min price selling key[0] for key[1]
		revN, revD int64 // min price selling key[1] for key[0]
	}
	pairs := map[[2]string]*bestPrices{}
	for _, o := range offers {
		key, fwd := [2]string{o.Selling, o.Buying}, true
		if o.Buying < o.Selling {
			key, fwd = [2]string{o.Buying, o.Selling}, false
		}
		b := pairs[key]
		if b == nil {
			b = &bestPrices{}
			pairs[key] = b
		}
		n, d := int64(o.PriceN), int64(o.PriceD)
		if fwd {
			b.fwdN, b.fwdD = minPriceRat(b.fwdN, b.fwdD, n, d)
		} else {
			b.revN, b.revD = minPriceRat(b.revN, b.revD, n, d)
		}
	}
	crossed := 0
	for _, b := range pairs {
		if b.fwdD == 0 || b.revD == 0 {
			continue // one-sided book — nothing to cross
		}
		// Best ask (key[1] per key[0]) = fwdN/fwdD. Best bid in the same
		// unit is the inverse of the reverse side's best price: revD/revN.
		// Crossed iff revD/revN >= fwdN/fwdD ⟺ revD·fwdD >= revN·fwdN.
		if b.revD*b.fwdD >= b.revN*b.fwdN {
			crossed++
		}
	}
	return crossed
}

// snapshotPair collects both sides of the (selling, buying) market
// under an RLock. ok=false before the first Load.
func (c *SDEXOrderBookCache) snapshotPair(selling, buying string) (asks, bids []clickhouse.LiveOffer, cursor uint32, at time.Time, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.loadedOK {
		return nil, nil, 0, time.Time{}, false
	}
	for _, o := range c.offers {
		switch {
		case o.Selling == selling && o.Buying == buying:
			asks = append(asks, o)
		case o.Selling == buying && o.Buying == selling:
			bids = append(bids, o)
		}
	}
	return asks, bids, c.cursor, c.updated, true
}

// ─── Wire shapes ─────────────────────────────────────────────────────

// SDEXOrderBookLevelView is one aggregated price level. All amounts
// are decimal STRINGS in whole (7-decimal) asset units; cumulative
// sums use big math (many int64 offers can exceed int64).
type SDEXOrderBookLevelView struct {
	// Price is buying-per-selling of the requested market (quote per
	// base), rendered from the exact rational at 7 decimal places.
	Price string `json:"price"`
	// PriceR is the exact reduced price rational (n/d = price).
	PriceR SDEXPriceRatView `json:"price_r"`
	// BaseAmount / QuoteAmount are this level's summed depth in the
	// requested market's base (selling) / quote (buying) asset.
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
	// CumBaseAmount / CumQuoteAmount are running totals from the top
	// of the book through this level.
	CumBaseAmount  string `json:"cum_base_amount"`
	CumQuoteAmount string `json:"cum_quote_amount"`
	// Offers is how many live offers were aggregated into this level.
	Offers int `json:"offers"`
}

// SDEXPriceRatView is an exact price fraction.
type SDEXPriceRatView struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

// SDEXOrderBookView is the envelope data field of GET /v1/sdex/orderbook.
type SDEXOrderBookView struct {
	// Selling / Buying echo the requested market: Selling is the BASE
	// asset (what asks sell), Buying the QUOTE.
	Selling string `json:"selling"`
	Buying  string `json:"buying"`
	// AsOfLedger is the ledger_entry_changes high-water the book has
	// applied; SnapshotAt is when it last advanced.
	AsOfLedger uint32 `json:"as_of_ledger"`
	SnapshotAt string `json:"snapshot_at"`
	// Asks (selling base for quote) ascending by price; Bids (buying
	// base with quote) descending by price.
	Asks []SDEXOrderBookLevelView `json:"asks"`
	Bids []SDEXOrderBookLevelView `json:"bids"`
	// AskOffers / BidOffers are the total live offer counts per side
	// BEFORE the depth cap, so a truncated book is visible as such.
	AskOffers int `json:"ask_offers"`
	BidOffers int `json:"bid_offers"`
	// Depth is the applied per-side level cap.
	Depth int `json:"depth"`
}

// ─── Handler ─────────────────────────────────────────────────────────

// handleSDEXOrderbook serves GET /v1/sdex/orderbook?selling=&buying=&depth=.
func (s *Server) handleSDEXOrderbook(w http.ResponseWriter, r *http.Request) {
	if s.sdexOrderBook == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/orderbook-unavailable",
			"Order book unavailable", http.StatusServiceUnavailable,
			"the SDEX order book requires the lake reader, which is not wired on this deployment")
		return
	}
	selling, buying, depth, problem := parseOrderBookParams(r)
	if problem != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-orderbook-request",
			"Invalid order book request", http.StatusBadRequest, problem)
		return
	}

	asks, bids, cursor, at, ready := s.sdexOrderBook.snapshotPair(selling.String(), buying.String())
	if !ready {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/orderbook-loading",
			"Order book snapshot is loading", http.StatusServiceUnavailable,
			"the live offer book is still loading from the lake after process start; retry shortly")
		return
	}

	view := SDEXOrderBookView{
		Selling:    selling.String(),
		Buying:     buying.String(),
		AsOfLedger: cursor,
		SnapshotAt: at.Format(time.RFC3339),
		Asks:       aggregateOrderBookSide(asks, false, depth),
		Bids:       aggregateOrderBookSide(bids, true, depth),
		AskOffers:  len(asks),
		BidOffers:  len(bids),
		Depth:      depth,
	}
	writeJSON(w, view, Flags{})
}

// parseOrderBookParams validates selling/buying (canonical classic
// forms only — the SDEX trades classic assets) and the depth cap.
// problem is non-empty on rejection.
func parseOrderBookParams(r *http.Request) (selling, buying canonical.Asset, depth int, problem string) {
	depth = sdexOrderBookDefaultDepth
	var err error
	if selling, err = parseClassicBookAsset(r.URL.Query().Get("selling")); err != nil {
		return selling, buying, 0, "selling: " + err.Error()
	}
	if buying, err = parseClassicBookAsset(r.URL.Query().Get("buying")); err != nil {
		return selling, buying, 0, "buying: " + err.Error()
	}
	if selling.String() == buying.String() {
		return selling, buying, 0, "selling and buying must differ"
	}
	if raw := r.URL.Query().Get("depth"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > sdexOrderBookMaxDepth {
			return selling, buying, 0, "depth must be an integer between 1 and " + strconv.Itoa(sdexOrderBookMaxDepth)
		}
		depth = n
	}
	return selling, buying, depth, ""
}

// parseClassicBookAsset accepts `native` or classic `CODE-G...` — the
// only asset forms a classic offer can reference.
func parseClassicBookAsset(raw string) (canonical.Asset, error) {
	if raw == "" {
		return canonical.Asset{}, errRequiredAsset
	}
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return canonical.Asset{}, err
	}
	if a.Type != canonical.AssetNative && a.Type != canonical.AssetClassic {
		return canonical.Asset{}, errNotClassicAsset
	}
	return a, nil
}

var (
	errRequiredAsset   = constError("required (canonical asset id, e.g. `native` or `USDC-G...`)")
	errNotClassicAsset = constError("must be `native` or a classic `CODE-G...` asset — SDEX offers only trade classic assets")
)

type constError string

func (e constError) Error() string { return string(e) }

// bookLevel accumulates one price level exactly.
type bookLevel struct {
	price  *big.Rat // quote per base, exact
	base   *big.Rat // stroop units of the base asset
	quote  *big.Rat // stroop units of the quote asset
	offers int
}

// aggregateOrderBookSide folds one side's offers into sorted,
// cumulative price levels.
//
// Asks sell the market's base asset: the offer's price N/D is already
// quote-per-base and Amount is base stroops. Bids sell the market's
// QUOTE asset: the offer's N/D is base-per-quote, so the level price
// is the exact inverse D/N and Amount is quote stroops; the base
// equivalent is Amount × N/D. All arithmetic is exact big.Rat — the
// only rounding is at render time (7 decimal places, the classic
// stroop precision).
func aggregateOrderBookSide(offers []clickhouse.LiveOffer, invert bool, depth int) []SDEXOrderBookLevelView {
	levels := map[[2]int64]*bookLevel{}
	for _, o := range offers {
		n, d := int64(o.PriceN), int64(o.PriceD)
		if invert {
			n, d = d, n
		}
		price := new(big.Rat).SetFrac64(n, d) // reduced by big.Rat
		key := [2]int64{price.Num().Int64(), price.Denom().Int64()}
		lvl := levels[key]
		if lvl == nil {
			lvl = &bookLevel{price: price, base: new(big.Rat), quote: new(big.Rat)}
			levels[key] = lvl
		}
		amt := new(big.Rat).SetInt64(o.Amount)
		if invert {
			// Offer sells quote: quote += amount; base += amount / price.
			lvl.quote.Add(lvl.quote, amt)
			lvl.base.Add(lvl.base, new(big.Rat).Quo(amt, price))
		} else {
			// Offer sells base: base += amount; quote += amount × price.
			lvl.base.Add(lvl.base, amt)
			lvl.quote.Add(lvl.quote, new(big.Rat).Mul(amt, price))
		}
		lvl.offers++
	}

	sorted := make([]*bookLevel, 0, len(levels))
	for _, lvl := range levels {
		sorted = append(sorted, lvl)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if invert { // bids: best (highest) price first
			return sorted[i].price.Cmp(sorted[j].price) > 0
		}
		return sorted[i].price.Cmp(sorted[j].price) < 0 // asks ascending
	})
	if len(sorted) > depth {
		sorted = sorted[:depth]
	}

	out := make([]SDEXOrderBookLevelView, 0, len(sorted))
	cumBase, cumQuote := new(big.Rat), new(big.Rat)
	stroops := new(big.Rat).SetInt64(10_000_000)
	for _, lvl := range sorted {
		cumBase.Add(cumBase, lvl.base)
		cumQuote.Add(cumQuote, lvl.quote)
		out = append(out, SDEXOrderBookLevelView{
			Price:          lvl.price.FloatString(7),
			PriceR:         SDEXPriceRatView{N: lvl.price.Num().Int64(), D: lvl.price.Denom().Int64()},
			BaseAmount:     new(big.Rat).Quo(lvl.base, stroops).FloatString(7),
			QuoteAmount:    new(big.Rat).Quo(lvl.quote, stroops).FloatString(7),
			CumBaseAmount:  new(big.Rat).Quo(cumBase, stroops).FloatString(7),
			CumQuoteAmount: new(big.Rat).Quo(cumQuote, stroops).FloatString(7),
			Offers:         lvl.offers,
		})
	}
	return out
}
