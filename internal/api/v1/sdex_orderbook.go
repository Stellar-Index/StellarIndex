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

// GET /v1/sdex/orderbook — live classic order-book depth for one (selling, buying) pair, served
// from an in-process book maintained off the certified lake.
//
// Why in-process: lake offer entries carry their assets only inside the entry_xdr blob, so a
// per-pair read would decode the whole ~1.02B-row offer slice per request. The book is loaded once
// and advanced with partition-pruned incremental reads (see clickhouse/sdex_offer_book_reader.go).

// SDEXOrderBookAdvanceInterval is the cadence the background goroutine advances the live book at, bounding staleness near what /v1/price's closed-bucket contract serves.
const SDEXOrderBookAdvanceInterval = 60 * time.Second

// SDEXOrderBookStaleAfter gates `flags.stale` at 2x the advance cadence, because Advance does not move `c.updated` on a read error and a stopped book would otherwise serve stale:false forever.
const SDEXOrderBookStaleAfter = 2 * SDEXOrderBookAdvanceInterval

// sdexOrderBookDefaultDepth / MaxDepth bound the served price levels
// per side.
const (
	sdexOrderBookDefaultDepth = 25
	sdexOrderBookMaxDepth     = 200
)

// SDEXOrderBookVerifyBatch bounds one VerifyPending sweep per tick (about 4 s of the 60 s tick); suspects are NOT served meanwhile, counted as `*_offers_withheld`.
const SDEXOrderBookVerifyBatch = 2500

// Order-book maintenance budgets and the self-heal cadence, applied by
// [SDEXOrderBookCache.MaintainTick].
const (
	// SDEXOrderBookLoadTimeout caps one full-slice load — minutes of
	// streaming IO on a populated lake; 30 min is a hard stop against a
	// wedged scan, not an expectation.
	SDEXOrderBookLoadTimeout = 30 * time.Minute
	// SDEXOrderBookAdvanceTimeout caps one tick's Advance + VerifyPending.
	SDEXOrderBookAdvanceTimeout = 2 * time.Minute
	// SDEXOrderBookReloadInterval is how often the book is rebuilt wholesale from the lake's
	// current-state projection. Advance only sees changes above its cursor: a change landing BELOW
	// it (a healed hole, a re-backfilled range) or one it could not apply stays wrong until a
	// reload, so daily bounds that to a day.
	//
	// Periodic, not triggered by the crossed-pairs gauge: a sustained crossed pair is a lake
	// coverage gap a re-load cannot heal, so re-firing a multi-minute scan would only add load.
	SDEXOrderBookReloadInterval = 24 * time.Hour
	// SDEXOrderBookReloadRetry spaces re-load attempts after a FAILED re-load: the book keeps
	// serving meanwhile, so a 30-minute scan must not hit a struggling ClickHouse every tick. The
	// INITIAL load still retries every tick, since until it lands the endpoint is a 503.
	SDEXOrderBookReloadRetry = time.Hour
)

// SDEXOrderBookReader is the lake seam the order-book cache maintains itself from; production wiring is *clickhouse.ExplorerReader.
type SDEXOfferBookReader interface {
	LoadLiveOffers(ctx context.Context) ([]clickhouse.LiveOffer, uint32, error)
	OfferChangesSince(ctx context.Context, fromLedger uint32) ([]clickhouse.OfferChange, uint32, error)
	// OfferRemovedAt reports which refs have a `removed` change row at
	// their own winning ledger — the version-tie zombie probe (the
	// clickhouse implementation's doc explains the tie).
	OfferRemovedAt(ctx context.Context, refs []clickhouse.OfferRemovalRef) (map[string]struct{}, error)
}

// SDEXOrderBookCache is the in-process live offer book (Load, Advance, MaintainTick); offers with intra_ledger_seq==0 are AMBIGUOUS and quarantined until VerifyPending proves no removal, and before the first Load the handler serves 503.
type SDEXOrderBookCache struct {
	reader SDEXOfferBookReader
	logger Logger

	mu       sync.RWMutex
	offers   map[string]clickhouse.LiveOffer // served book; key = LedgerKey XDR
	pending  map[string]clickhouse.LiveOffer // quarantined suspects awaiting verification
	withheld map[[2]string]int               // pending count per (selling, buying) market; kept in step by dropPendingLocked
	crossed  int                             // last computed crossed-pair count (for change-only logging)
	cursor   uint32                          // contiguous lake ledger the book has applied through
	updated  time.Time
	loadedOK bool
	loadedAt time.Time // last SUCCESSFUL Load — the re-load clock
	loadTry  time.Time // last Load attempt, either outcome — the re-load retry clock

	now func() time.Time // time.Now outside tests
}

// NewSDEXOrderBookCache constructs an empty (not-ready) cache.
func NewSDEXOrderBookCache(reader SDEXOfferBookReader, logger Logger) *SDEXOrderBookCache {
	return &SDEXOrderBookCache{reader: reader, logger: logger, now: time.Now}
}

// MaintainTick is one background maintainer pass every [SDEXOrderBookAdvanceInterval]: load retry, change-stream apply, quarantine drain, periodic reload; a failed step never takes the served book down.
func (c *SDEXOrderBookCache) MaintainTick(ctx context.Context) {
	c.mu.RLock()
	ready := c.loadedOK
	c.mu.RUnlock()
	if !ready {
		c.loadWithin(ctx)
		return
	}
	tickCtx, cancel := context.WithTimeout(ctx, SDEXOrderBookAdvanceTimeout)
	_ = c.Advance(tickCtx)
	_ = c.VerifyPending(tickCtx, SDEXOrderBookVerifyBatch)
	cancel()
	if c.ReloadDue(c.now()) {
		c.loadWithin(ctx)
	}
}

// loadWithin runs one Load under [SDEXOrderBookLoadTimeout].
func (c *SDEXOrderBookCache) loadWithin(ctx context.Context) {
	loadCtx, cancel := context.WithTimeout(ctx, SDEXOrderBookLoadTimeout)
	defer cancel()
	_ = c.Load(loadCtx)
}

// ReloadDue reports whether the periodic re-load is owed at now: loaded, last success older than [SDEXOrderBookReloadInterval], and last attempt older than [SDEXOrderBookReloadRetry].
func (c *SDEXOrderBookCache) ReloadDue(now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadedOK &&
		now.Sub(c.loadedAt) >= SDEXOrderBookReloadInterval &&
		now.Sub(c.loadTry) >= SDEXOrderBookReloadRetry
}

// observeMaintain records one Load/Advance/VerifyPending attempt against the order-book maintenance metrics. op is "load", "advance" or
// "verify"; the outcome label is op-qualified (load_ok, advance_error, ...) so cost classes chart apart.
func observeMaintain(op string, start time.Time, err error) {
	outcome := op + "_ok"
	if err != nil {
		outcome = op + "_error"
	}
	observeMaintainOutcome(outcome, start)
}

// observeMaintainOutcome records against an explicit outcome label, for the case observeMaintain's op+error inference can't express:
// Advance's "advance_held" (zero cursor progress, no error, see [SDEXOrderBookCache.Advance]).
func observeMaintainOutcome(outcome string, start time.Time) {
	obs.SDEXOrderBookMaintainTotal.WithLabelValues(outcome).Inc()
	obs.SDEXOrderBookMaintainDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// Load performs a full-book load (initial and periodic self-heal), replacing the book wholesale; a failed re-Load keeps the previous book serving and earned verification verdicts are kept.
func (c *SDEXOrderBookCache) Load(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { observeMaintain("load", start, err) }()
	c.mu.Lock()
	c.loadTry = c.now()
	c.mu.Unlock()
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
	c.mu.Lock()
	c.offers, c.pending = partitionLoadedBook(book, c.offers)
	c.withheld = make(map[[2]string]int)
	for _, o := range c.pending {
		c.withheld[[2]string{o.Selling, o.Buying}]++
	}
	c.cursor = cursor
	c.updated = time.Now().UTC()
	c.loadedOK = true
	c.loadedAt = c.now()
	c.updateHealthGaugesLocked()
	c.mu.Unlock()
	return nil
}

// partitionLoadedBook splits a freshly loaded book into the served book and the version-tie quarantine (intra_ledger_seq == 0); seed-
// snapshot rows carry intra 0xFFFFFFFF and stay trusted.
// trusted is the book being replaced (nil on first load): a suspect it already serves at the SAME version keeps its verdict.
func partitionLoadedBook(book, trusted map[string]clickhouse.LiveOffer) (served, pending map[string]clickhouse.LiveOffer) {
	served = make(map[string]clickhouse.LiveOffer, len(book))
	pending = make(map[string]clickhouse.LiveOffer)
	for k, o := range book {
		if o.Version&0xFFFFFFFF != 0 {
			served[k] = o
			continue
		}
		if prev, ok := trusted[k]; ok && prev.Version == o.Version {
			served[k] = o
			continue
		}
		pending[k] = o
	}
	return served, pending
}

// dropPendingLocked removes k from the quarantine and its market's
// withheld count. Caller holds mu for writing.
func (c *SDEXOrderBookCache) dropPendingLocked(k string) {
	o, ok := c.pending[k]
	if !ok {
		return
	}
	delete(c.pending, k)
	m := [2]string{o.Selling, o.Buying}
	if c.withheld[m] <= 1 {
		delete(c.withheld, m)
		return
	}
	c.withheld[m]--
}

// VerifyPending drains up to limit quarantined offers by probing the change stream for a removal at each offer's ledger; proven-dead are discarded, the rest join the served book.
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
		c.dropPendingLocked(ref.KeyXDR)
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

// Advance applies offer changes since the cursor by version (>= wins), so duplicated rows are idempotent; a tick with zero cursor progress records "advance_held".
func (c *SDEXOrderBookCache) Advance(ctx context.Context) error {
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
	changes, next, err := c.reader.OfferChangesSince(ctx, cursor)
	if err != nil {
		observeMaintain("advance", start, err)
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
		c.dropPendingLocked(ch.KeyXDR)
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
	if next == cursor {
		// The lake's contiguous tip has not moved past the cursor: an unhealed hole or a full ingest
		// halt, unlike a legitimate advance that applied zero CHANGES while the tip moved. advance_ok
		// would mask it (no error is raised), leaving the book stale with maintain_failing silent for
		// as long as the hold lasts. Mirrors the projector's watermark_held outcome.
		observeMaintainOutcome("advance_held", start)
		return nil
	}
	observeMaintain("advance", start, nil)
	return nil
}

// updateHealthGaugesLocked recomputes the book's data-quality gauges (caller holds mu). The crossed-pairs count is the tripwire
// this cache keeps at zero: Stellar's DEX executes crossing offers at submission, so a RESTING book never has best bid > best
// ask; sustained non-zero means phantom offers are served (e.g. zombies whose removal was never ingested, so [VerifyPending]
// cannot disprove them). The stellarindex_sdex_orderbook_crossed_book alert fires on it and the log line names the pairs.
func (c *SDEXOrderBookCache) updateHealthGaugesLocked() {
	pairs := crossedPairs(c.offers)
	crossed := len(pairs)
	if crossed != c.crossed && c.logger != nil {
		c.logger.Warn("sdex order book crossed-pair count changed",
			"crossed_pairs", crossed, "was", c.crossed, "pending_offers", len(c.pending),
			"pairs", pairs[:min(len(pairs), 10)])
	}
	c.crossed = crossed
	obs.SDEXOrderBookCrossedPairs.Set(float64(crossed))
	obs.SDEXOrderBookPendingOffers.Set(float64(len(c.pending)))
}

// minPriceRat returns the smaller of cur = curN/curD and n/d by exact cross-multiplication (curD == 0 means "unset"); price components
// are int32, so the products fit in int64.
func minPriceRat(curN, curD, n, d int64) (int64, int64) {
	if curD == 0 || n*curD < curN*d {
		return n, d
	}
	return curN, curD
}

// crossedPairs lists, sorted, the asset pairs whose book is crossed: best bid strictly above best ask. A touching book is legal (a
// PASSIVE offer rests at exactly the inverse price), so counting it would hold the gauge non-zero indefinitely.
func crossedPairs(offers map[string]clickhouse.LiveOffer) []string {
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
	var crossed []string
	for key, b := range pairs {
		if b.fwdD == 0 || b.revD == 0 {
			continue // one-sided book — nothing to cross
		}
		// Best ask (key[1] per key[0]) = fwdN/fwdD. Best bid in the same
		// unit is the inverse of the reverse side's best price: revD/revN.
		// Crossed iff revD/revN > fwdN/fwdD ⟺ revD·fwdD > revN·fwdN.
		if b.revD*b.fwdD > b.revN*b.fwdN {
			crossed = append(crossed, key[0]+"/"+key[1])
		}
	}
	sort.Strings(crossed)
	return crossed
}

// sdexMarketSnapshot is one consistent read of a (selling, buying) market: served offers per side, the quarantined offers withheld from
// each side, and the book's freshness.
type sdexMarketSnapshot struct {
	asks, bids                 []clickhouse.LiveOffer
	withheldAsks, withheldBids int
	cursor                     uint32
	at                         time.Time
}

// snapshotMarket collects both sides of the (selling, buying) market
// under one RLock. ok=false before the first Load.
func (c *SDEXOrderBookCache) snapshotMarket(selling, buying string) (snap sdexMarketSnapshot, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.loadedOK {
		return sdexMarketSnapshot{}, false
	}
	for _, o := range c.offers {
		switch {
		case o.Selling == selling && o.Buying == buying:
			snap.asks = append(snap.asks, o)
		case o.Selling == buying && o.Buying == selling:
			snap.bids = append(snap.bids, o)
		}
	}
	snap.withheldAsks = c.withheld[[2]string{selling, buying}]
	snap.withheldBids = c.withheld[[2]string{buying, selling}]
	snap.cursor, snap.at = c.cursor, c.updated
	return snap, true
}

// ─── Wire shapes ─────────────────────────────────────────────────────

// SDEXOrderBookLevelView is one aggregated price level; amounts are decimal STRINGS in whole 7-decimal asset units and cumulative sums use big math.
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
	// AskOffers / BidOffers are the total SERVED offer counts per side
	// BEFORE the depth cap, so a truncated book is visible as such.
	AskOffers int `json:"ask_offers"`
	BidOffers int `json:"bid_offers"`
	// AskOffersWithheld / BidOffersWithheld count the offers the lake
	// lists as live on each side but the book withholds until removal
	// verification clears them (see [SDEXOrderBookCache]). Non-zero
	// means the served depth — possibly the best price — is incomplete.
	AskOffersWithheld int `json:"ask_offers_withheld"`
	BidOffersWithheld int `json:"bid_offers_withheld"`
	// Depth is the applied per-side level cap.
	Depth int `json:"depth"`
}

// ─── Handler ─────────────────────────────────────────────────────────

// handleSDEXOrderbook serves GET /v1/sdex/orderbook?selling=&buying=&depth=.
func (s *Server) handleSDEXOrderbook(w http.ResponseWriter, r *http.Request) {
	if s.SDEXOrderBook == nil {
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

	snap, ready := s.SDEXOrderBook.snapshotMarket(selling.String(), buying.String())
	if !ready {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/orderbook-loading",
			"Order book snapshot is loading", http.StatusServiceUnavailable,
			"the live offer book is still loading from the lake after process start; retry shortly")
		return
	}

	view := SDEXOrderBookView{
		Selling:           selling.String(),
		Buying:            buying.String(),
		AsOfLedger:        snap.cursor,
		SnapshotAt:        snap.at.Format(time.RFC3339),
		Asks:              aggregateOrderBookSide(snap.asks, false, depth),
		Bids:              aggregateOrderBookSide(snap.bids, true, depth),
		AskOffers:         len(snap.asks),
		BidOffers:         len(snap.bids),
		AskOffersWithheld: snap.withheldAsks,
		BidOffersWithheld: snap.withheldBids,
		Depth:             depth,
	}
	// snap.at is the last SUCCESSFUL Advance (a failed one
	// leaves it untouched — see Advance's early return), so this is the
	// only honest staleness signal a wedged book has.
	// A stale book is the last good snapshot carried past failed advances.
	stale := time.Since(snap.at) > SDEXOrderBookStaleAfter
	writeJSON(w, view, Flags{Stale: stale, Degraded: stale})
}

// parseOrderBookParams validates selling/buying (canonical classic forms only, since the SDEX trades classic assets) and the depth cap;
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

// aggregateOrderBookSide folds one side's offers into sorted, cumulative price levels. Asks sell the base: N/D is already
// quote-per-base, Amount is base stroops. Bids sell the QUOTE: N/D is base-per-quote, so the level price is the exact inverse
// D/N, Amount is quote stroops and the base equivalent is Amount x N/D. All big.Rat; rounding only at render time (7 decimal
// places, stroop precision).
func aggregateOrderBookSide(offers []clickhouse.LiveOffer, invert bool, depth int) []SDEXOrderBookLevelView {
	levels := map[[2]int64]*bookLevel{}
	for _, o := range offers {
		n, d := int64(o.PriceN), int64(o.PriceD)
		if invert {
			n, d = d, n
		}
		price := new(big.Rat).SetFrac64(n, d)                       // reduced by big.Rat
		key := [2]int64{price.Num().Int64(), price.Denom().Int64()} // i128:ok reduced n/d of an int32 XDR price always fits int64
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
			PriceR:         SDEXPriceRatView{N: lvl.price.Num().Int64(), D: lvl.price.Denom().Int64()}, // i128:ok reduced n/d of an int32 XDR price always fits int64
			BaseAmount:     new(big.Rat).Quo(lvl.base, stroops).FloatString(7),
			QuoteAmount:    new(big.Rat).Quo(lvl.quote, stroops).FloatString(7),
			CumBaseAmount:  new(big.Rat).Quo(cumBase, stroops).FloatString(7),
			CumQuoteAmount: new(big.Rat).Quo(cumQuote, stroops).FloatString(7),
			Offers:         lvl.offers,
		})
	}
	return out
}
