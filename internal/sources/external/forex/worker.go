package forex

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// FXQuoteWriter is the write seam between the worker and persistent
// storage. nil-able — when nil the worker still functions (cache-only
// mode, useful in tests or pre-migration deploys).
type FXQuoteWriter interface {
	InsertFXQuoteBatch(ctx context.Context, quotes []FXQuote) error
}

// FXQuoteReader is the read seam that restores held rates after a
// restart. nil-able — without it a cold start holds nothing.
type FXQuoteReader interface {
	// LatestFXQuotes returns the newest row per ticker with Bucket >= since.
	LatestFXQuotes(ctx context.Context, since time.Time) ([]FXQuote, error)
}

// FXQuote is the storage-layer record the worker writes per refresh.
// Mirrors the timescale.FXQuote shape but lives in this package so
// the forex worker doesn't import internal/storage/timescale (which
// imports forex types via the v1 API package; the dependency would
// cycle).
type FXQuote struct {
	Bucket time.Time
	Ticker string
	//floatmoney:ok known debt — ingest DTO feeding timescale.FXQuote.RateUSD (same debt); upstream ECB/fallback sources are JSON floats today
	RateUSD float64
	Source  string
}

// maxRateDeviation is the per-refresh sanity band: a rate further than this fraction from the ticker's last
// ACCEPTED rate is not written on first sighting. fx_quotes is the denominator of every fiat-quoted
// usd_volume, so the band catches a BROKEN BAR (a 10x decimal shift is 900%, a unit or redenomination change)
// without second-guessing the market: real one-day devaluations reach EGP ~38%, NGN ~40%, TRY ~25%, and a
// tighter band would reject genuine history and wedge the ticker.
const maxRateDeviation = 0.50

// fxSource is the source tag stamped on persisted rows and on the
// rejection metric's `source` label.
const fxSource = "massive"

// historySource labels dated bars: fetchHistory reads only the primary
// client, whichever provider served the current rates.
const historySource = fxSource

// rateGuard is the per-ticker band state: lastAccepted is the baseline; pending holds an outlier rejected
// ONCE so a second, agreeing fetch can confirm it. A one-off bad bar never reaches fx_quotes, while a real
// devaluation, which upstream keeps reporting, is accepted ≈1h later instead of wedging the ticker forever.
type rateGuard struct {
	lastAccepted float64
	pending      float64

	// Stuck-upstream tracking for the HISTORY band: after [stuckRejectionThreshold] consecutive refusals of the
	// SAME broken historical bar, repeats count as `history_deviation_stuck` (excluded from the alert, still
	// WARN-logged and graphable) so a known broken bar stops paging for days. Any acceptance or a DIFFERENT
	// rejected value resets the streak: fresh disagreement always alerts.
	//floatmoney:ok known debt — holds a copy of the same float64 RateUSD (above) for equality/streak comparison, not persisted or served
	stuckRejectedRate float64
	stuckCount        int

	// Stuck-upstream tracking for the CONFIRM VETO: a feed persistently serving the same broken CURRENT bar
	// re-arms pending, and every second sighting is vetoed as `deviation_history_conflict`; this streak quiets
	// the repeats. Separate from the history-band fields because those reset on every in-band history bar, which
	// for a broken-current / healthy-history feed is every refresh. Reset by any accepted current rate.
	//floatmoney:ok known debt — same class as stuckRejectedRate above
	conflictStuckRate  float64
	conflictStuckCount int

	// bootstrapUnconfirmed marks a baseline seeded from one uncorroborated sample (the bootstrap arm). A restart
	// once bootstrapped UZS on a broken 1820 (true ≈ 11,800) and then rejected the CORRECT 7-day history against
	// it; this flag lets the history-majority heal in [Worker.guardSnapshot] also scrub that current-day row.
	// Cleared by a within-band acceptance or a two-fetch pending confirmation.
	bootstrapUnconfirmed bool

	// healedUncorroborated marks a baseline installed by the history-majority heal that no current fetch has
	// agreed with yet. A heal is history's word alone; if history was the broken side, keeping the baseline
	// healable lets a corrected history re-point it instead of wedging the ticker. Cleared as bootstrapUnconfirmed.
	healedUncorroborated bool
}

// stuckRejectionThreshold is how many consecutive identical history-bar
// refusals reclassify the ticker as stuck. The worker refreshes hourly,
// so 12 ≈ half a day of the provider serving the same broken bar —
// far past the point where repeats carry signal, and long enough that
// a real multi-refresh scale change has already fired the alert.
const stuckRejectionThreshold = 12

// stuckSameRateTolerance treats consecutive rejected bars within this relative distance as the SAME stuck
// value. A live broken feed jitters (UZS: 11791.69 → 11785 → 11817.69), so exact equality never engaged the
// streak; 1% is tighter than any day-over-day FX move worth reporting and wider than provider jitter.
const stuckSameRateTolerance = 0.01

// History-majority heal: when one refresh rejects ≥ [historyHealMinBars] trailing-7d bars that agree within
// [historyHealAgreement] of their median, AND the baseline is an uncorroborated bootstrap, the BASELINE is the
// outlier: ≥4 dated samples beat one. guardSnapshot re-points lastAccepted at the median, admits the bars and
// scrubs the poisoned current-day row. Two limits keep this from becoming denominator poisoning:
//   - The agreement band is far tighter than [maxRateDeviation], so a rejected series spanning levels never
//     heals. A redenomination at first-ever sighting can still flip the baseline; the two-fetch confirmation
//     restores it within ~2 refreshes, and per-date rows stay correct throughout.
//   - A CONFIRMED baseline is never healed: two agreeing current fetches against agreeing history means one
//     endpoint is systemically broken and we can't tell which, so the ticker keeps rejecting (quietly, once
//     stuck) and waits for an operator.
const (
	historyHealMinBars   = 4
	historyHealAgreement = 0.10
)

// Worker periodically fetches the upstream rates + names and
// installs the result into a [Cache]. Designed to run as a
// goroutine for the lifetime of the API process.
type Worker struct {
	client      *Client
	cache       *Cache
	writer      FXQuoteWriter
	reader      FXQuoteReader
	logger      *slog.Logger
	interval    time.Duration
	circulation map[string]CirculationEntry // loaded once at startup

	// guards holds the sanity-band state per ticker. Only touched from
	// refreshOnce → guardSnapshot, which is single-goroutine (Run owns
	// the ticker loop). Empty on process start: the first refresh has no
	// baseline, so it accepts and establishes one.
	guards map[string]*rateGuard

	// historyVeto is guardSnapshot-scoped state: the per-ticker heal-grade history-majority median
	// ([historyMajority]) of the current snapshot's trailing-7d bars, computed before the current-rate loop and
	// cleared on return; acceptRate's pending-confirm arm consults it. Two agreeing samples from ONE broken
	// endpoint (UZS kept serving 1820 after the heal and would have re-confirmed it) are not two witnesses when
	// ≥4 agreeing dated bars refute them. History never SETS a baseline here, it only refuses a confirm; a real
	// devaluation confirms once the trailing majority follows it or splits. Only touched from guardSnapshot.
	historyVeto map[string]float64

	// streakCounted is guardSnapshot-scoped: tickers whose stuck streak
	// already moved this refresh. nil outside a refresh (every refusal
	// counts), so the streak measures refreshes, not broken bars.
	streakCounted map[string]bool

	// rawHistory is the trailing-7d series exactly as last fetched —
	// every dated bar, including the ones the band refuses. It is carried
	// from refresh to refresh HERE and never published: the heal and the
	// confirm veto must re-see refused bars on every refresh, while the
	// served [Snapshot.History7d] carries only the bars the band admitted
	// (see [Worker.refreshOnce]). Only touched from refreshOnce.
	rawHistory map[string][]HistoryPoint

	// fallbacks are tried IN ORDER when the primary client cannot serve
	// rates, so a paid-feed outage degrades coverage instead of ending
	// it. Empty by default — a worker with no fallbacks behaves exactly
	// as before.
	fallbacks []RateProvider

	// corroborator is held for the FX fixings plan and never consulted
	// yet; see [Worker.WithCorroborator].
	corroborator RateProvider

	// activeSource is the feed that produced the CURRENT snapshot. It is
	// stamped into fx_quotes.source and used as the `source` metric
	// label, so `stellarindex_external_fx_last_quote_unix{source=...}`
	// shows which feed is actually live rather than always claiming the
	// primary. Only touched from refreshOnce -> guardSnapshot
	// (single-goroutine, as guards).
	activeSource string

	// fixingWriter, when set, receives the vendor-time hourly bars after
	// every refresh (fx_fixings); fixingNewest is the newest bar_end it
	// has committed. Only touched from refreshOnce.
	fixingWriter FXFixingWriter
	fixingNewest time.Time

	// now pins the clock in tests; nil is time.Now.
	now func() time.Time
}

// NewWorker constructs the worker. Massive's grouped aggregate is daily, so sub-15-min polling re-fetches the
// same bar; 1h keeps the cache fresh across restarts and picks up the new UTC day. The curated monetary-base
// CSV (circulation_data.csv) loads once; bad rows are logged and skipped, and the map rides every snapshot.
func NewWorker(client *Client, cache *Cache, logger *slog.Logger, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = time.Hour
	}
	circulation, err := loadCirculationTable()
	if err != nil {
		logger.Warn("forex: circulation csv parsed with skipped rows", "err", err)
	}
	logger.Info("forex: circulation table loaded", "entries", len(circulation))
	return &Worker{
		client:      client,
		cache:       cache,
		logger:      logger,
		interval:    interval,
		circulation: circulation,
		guards:      map[string]*rateGuard{},
		// Until a refresh says otherwise, the primary is the source.
		activeSource: fxSource,
	}
}

// WithFallbacks registers standby rate providers, tried in order when
// the primary client fails. See [RateProvider] for why this exists.
func (w *Worker) WithFallbacks(providers ...RateProvider) *Worker {
	w.fallbacks = append(w.fallbacks, providers...)
	return w
}

// WithCorroborator registers a provider outside the serving chain. The
// worker only stores it: it is never fetched, and never serves or stamps a
// source, until the FX fixings plan wires it in.
func (w *Worker) WithCorroborator(p RateProvider) *Worker {
	w.corroborator = p
	return w
}

// Corroborator returns the provider registered by [Worker.WithCorroborator],
// or nil.
func (w *Worker) Corroborator() RateProvider { return w.corroborator }

// WithWriter attaches a persistent quote writer. When set, every
// successful refreshOnce also persists the latest rates + history
// to the fx_quotes hypertable. nil writer keeps the worker in
// cache-only mode (the pre-fx_quotes behaviour).
func (w *Worker) WithWriter(writer FXQuoteWriter) *Worker {
	w.writer = writer
	return w
}

// WithReader attaches the fx_quotes reader the first refresh seeds its
// held rates from. nil keeps a cold start holding nothing.
func (w *Worker) WithReader(reader FXQuoteReader) *Worker {
	w.reader = reader
	return w
}

// Run blocks until ctx is cancelled: one fetch immediately so /v1/currencies is populated, then one per
// interval. Failures are logged, never fatal; the cache keeps the prior snapshot.
//
// It publishes stellarindex_source_enabled itself because `massive` is the one registry source not run by the
// indexer (the gauge's other writer), and /v1/sources/{name}/health reads the gauge as "switched on":
// without it the live FX feed read "massive: off". Fallback providers are standbys and stay unmarked.
func (w *Worker) Run(ctx context.Context) error {
	obs.SourceEnabled.WithLabelValues(fxSource).Set(1)
	defer obs.SourceEnabled.WithLabelValues(fxSource).Set(0)

	w.refreshOnce(ctx)

	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			w.refreshOnce(ctx)
		}
	}
}

// refreshOnce performs a single fetch+install cycle. Errors get
// logged at warn level (not error — a stale cache is degraded
// service, not a crash condition).
// sourceLabel is the feed name stamped on persisted rows and metric
// labels. It falls back to the primary when activeSource is unset so a
// zero-value Worker — a struct literal, as several tests build — keeps
// the exact pre-fallback behaviour instead of writing an empty source.
func (w *Worker) sourceLabel() string {
	if w.activeSource == "" {
		return fxSource
	}
	return w.activeSource
}

// fetchRates returns rates from the first source that can serve them,
// along with that source's name. The primary client is always tried
// first; fallbacks follow in registration order.
//
// A context cancellation is NOT a source failure — it means we are
// shutting down, so it short-circuits rather than burning through every
// fallback on the way out.
func (w *Worker) fetchRates(ctx context.Context) (map[string]float64, time.Time, string, error) {
	rates, publishedAt, err := w.client.LatestUSDRates(ctx)
	if err == nil {
		return rates, publishedAt, fxSource, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, time.Time{}, "", err
	}
	primaryErr := err

	for _, fb := range w.fallbacks {
		rates, publishedAt, err = fb.LatestUSDRates(ctx)
		if err == nil {
			// Loud on purpose: serving from a standby is a degraded
			// state an operator should see, not a silent substitution.
			w.logger.Warn("forex: primary failed — serving from fallback",
				"primary", fxSource, "primary_err", primaryErr,
				"fallback", fb.Name(), "currencies", len(rates))
			return rates, publishedAt, fb.Name(), nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, time.Time{}, "", err
		}
		w.logger.Warn("forex: fallback failed", "fallback", fb.Name(), "err", err)
	}
	return nil, time.Time{}, "", primaryErr
}

func (w *Worker) refreshOnce(ctx context.Context) {
	rates, publishedAt, source, err := w.fetchRates(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("forex: rates fetch failed on every source", "err", err)
		return
	}
	w.activeSource = source

	names, err := w.client.CurrencyNames(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		// Names are STATIC labels, not rates: losing them must not cost
		// us a refresh whose rates we already have. This matters most in
		// exactly the case the fallback exists for — when the primary is
		// down, its names endpoint is down too. Reuse the last snapshot's
		// names; only bail if we have none at all (first run).
		if prev := w.cache.Latest(); prev != nil && len(prev.Currencies) > 0 {
			names = make(map[string]string, len(prev.Currencies))
			for _, c := range prev.Currencies {
				if c.Name != "" {
					names[c.Ticker] = c.Name
				}
			}
			w.logger.Warn("forex: names fetch failed — reusing last known names",
				"err", err, "names", len(names))
		} else {
			// Cold start with the primary's names endpoint down — the
			// case the rates fallback exists for. buildSnapshot drops an
			// unnamed code and fetchHistory only walks named ones, so an
			// empty map would install nothing; label each rate with its
			// own ticker instead and let the next successful names fetch
			// replace the labels.
			names = tickerNames(rates)
			w.logger.Warn("forex: names fetch failed and no cached names — labelling rates by ticker",
				"err", err, "currencies", len(names))
		}
	}

	// Carry the last fetched history forward; it is re-fetched once a day (7 cached dated URLs). It is the RAW
	// series, private to the worker: the heal and the confirm veto need the bars the band REFUSED, while the
	// served snapshot carries only the admitted ones.
	if w.shouldRefreshHistory(w.rawHistory, publishedAt) {
		w.rawHistory = w.fetchHistory(ctx, names, publishedAt)
	}

	// GUARD FIRST, INSTALL SECOND: only what the band clears reaches the cache /v1/price's fiat paths read.
	// Banding only the fx_quotes write once kept UZS=1820 (true ≈ 11,800) out of the table all day while every
	// fiat:UZS request was priced off 1820. The band runs with or without a writer.
	raw := buildSnapshot(rates, names, publishedAt, time.Now().UTC(), w.rawHistory, w.circulation)
	for i := range raw.Currencies {
		raw.Currencies[i].Source = source
	}
	res := w.guardSnapshot(raw)
	prev := w.cache.Latest()
	if prev == nil {
		prev = w.seedSnapshot(ctx, names, raw.FetchedAt)
	}
	snap := servedSnapshot(raw, res, prev)
	w.cache.Set(snap)
	w.logger.Info("forex: snapshot installed",
		"currencies", len(snap.Currencies),
		"upstream_currencies", len(raw.Currencies),
		"history_currencies", len(snap.History7d),
		"published_at", publishedAt,
	)

	w.writeBatch(ctx, res)
	w.appendFixings(ctx, massiveTickers(raw, source), snap)
}

// maxHeldRateAge bounds how long [servedSnapshot] keeps serving a rate
// that no refresh has re-confirmed. It mirrors the fx_quotes read path's
// own bound (fxQuotesSnapLookback in internal/storage/timescale, the
// "7-day forex-snap lookback" of docs/operations/runbooks/external-pollers.md#stellarindex_external_fx_feed_stale)
// so the in-memory feed and the table go dark for a ticker at the same
// age rather than the cache serving a rate the table has already given
// up on.
const maxHeldRateAge = 7 * 24 * time.Hour

// guardResult is what one pass of the sanity band over a raw snapshot
// yields: the rows cleared for fx_quotes, and the same verdicts in the
// shape [servedSnapshot] needs to build the served cache entry. One pass
// produces both so the table and the cache can never disagree about what
// was accepted.
type guardResult struct {
	// batch is the fx_quotes write: accepted current rows plus accepted
	// (and healed) dated bars, scrubbed rows already removed.
	batch []FXQuote
	// current is the set of tickers whose CURRENT rate the band accepted
	// this refresh and the history-majority heal did not then scrub.
	current map[string]bool
	// refuted is the set of tickers whose BASELINE the heal overturned
	// this refresh. Their last served rate is the very sample the
	// ticker's own history refuted, so it must not be held either.
	refuted map[string]bool
	// history is the dated bars the band admitted, per ticker.
	history map[string][]HistoryPoint
}

// freshQuotes counts the upstream current rates this refresh committed.
// The synthetic anchor and carried-forward history bars are excluded:
// both are written on every refresh whether or not the feed answered.
func (r guardResult) freshQuotes() int {
	n := 0
	for ticker, ok := range r.current {
		if ok && ticker != anchorTicker {
			n++
		}
	}
	return n
}

// servedSnapshot builds the cached snapshot from the raw one and the band's verdicts. Per ticker:
//   - current rate ACCEPTED → served as fetched, stamped with this refresh's publication time.
//   - REFUSED, or ABSENT this refresh (the ECB standby covers ~30 currencies to the primary's ~110) → the last
//     served entry is HELD with its original UpdateAt, as fx_quotes keeps its last accepted row, until
//     [maxHeldRateAge]; then it is dropped (fail closed).
//   - baseline REFUTED by the heal → dropped (the holdable entry is the refuted sample); it returns once the
//     healed baseline accepts a current rate.
//
// Both /v1/price fiat paths answer "no rate" for a dropped ticker.
func servedSnapshot(raw *Snapshot, res guardResult, prev *Snapshot) *Snapshot {
	out := make([]Currency, 0, len(raw.Currencies))
	served := make(map[string]bool, len(raw.Currencies))
	for _, c := range raw.Currencies {
		if res.current[c.Ticker] {
			out = append(out, c)
			served[c.Ticker] = true
		}
	}
	if prev != nil {
		for _, c := range prev.Currencies {
			if served[c.Ticker] || res.refuted[c.Ticker] {
				continue
			}
			if raw.FetchedAt.Sub(c.UpdateAt) > maxHeldRateAge {
				continue
			}
			out = append(out, c)
			served[c.Ticker] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return &Snapshot{
		Currencies:  out,
		PublishedAt: raw.PublishedAt,
		FetchedAt:   raw.FetchedAt,
		History7d:   res.history,
		Circulation: raw.Circulation,
	}
}

// seedSnapshot rebuilds, from fx_quotes, the held rates a previous process
// was serving. The upstream republishes tickers gradually through the day,
// so without it a restart drops every one it has not re-sent yet. The seed
// only stands in for prev: servedSnapshot still prefers this refresh's
// accepted rate, skips refuted tickers and applies [maxHeldRateAge].
func (w *Worker) seedSnapshot(ctx context.Context, names map[string]string, now time.Time) *Snapshot {
	if w.reader == nil {
		return nil
	}
	rows, err := w.reader.LatestFXQuotes(ctx, now.Add(-maxHeldRateAge))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.logger.Warn("forex: fx_quotes seed read failed — holding nothing", "err", err)
		}
		return nil
	}
	byTicker := make(map[string]string, len(names))
	for code, n := range lowerKeyed(names) {
		byTicker[upper(code)] = n
	}
	seed := &Snapshot{Currencies: make([]Currency, 0, len(rows))}
	for _, q := range rows {
		if q.RateUSD <= 0 || !isFiniteFloat(q.RateUSD) {
			continue
		}
		ticker := upper(q.Ticker)
		name := ticker
		if n := byTicker[ticker]; n != "" {
			name = toTitle(n)
		}
		seed.Currencies = append(seed.Currencies, Currency{
			Ticker:   ticker,
			Name:     name,
			RateUSD:  q.RateUSD,
			UpdateAt: q.Bucket,
			Source:   q.Source,
		})
	}
	w.logger.Info("forex: seeded held rates from fx_quotes", "currencies", len(seed.Currencies))
	return seed
}

// persistSnapshot bands and writes one snapshot to fx_quotes ([Worker.guardSnapshot] then
// [Worker.writeBatch]); a nil writer is a no-op, and errors are logged at WARN, never fatal. It writes today's
// row per ticker (upserted on (ticker, today)) and the trailing-7d rows, except an accepted bar dated today,
// which is cached from the day's first fetch and would pin today's row to a stale value.
//
// Both are banded, since one bad bar would mis-scale a currency's whole usd_volume history; a rejected ticker
// keeps its last accepted row ([obs.ExternalFXRateRejectedTotal]). History points are banded read-only by
// [acceptHistoryRate] against the current baseline: a 7d-old bar >50% off the live rate is a broken bar.
//
// [Worker.refreshOnce] doesn't call this: it runs the same two halves with the cache install between them so
// the served snapshot follows the band's verdicts. This composition is what the band's tests drive.
func (w *Worker) persistSnapshot(ctx context.Context, snap *Snapshot) {
	if w.writer == nil || snap == nil {
		return
	}
	w.writeBatch(ctx, w.guardSnapshot(snap))
}

// guardSnapshot runs the sanity band over one raw snapshot and returns its verdicts ([guardResult]). It is
// the ONLY place band state advances and must run exactly once per refresh: acceptRate arms pending on a
// refusal, so a second pass would confirm the outlier the first refused. It needs no writer, since the band
// also decides what is served.
func (w *Worker) guardSnapshot(snap *Snapshot) guardResult {
	today := snap.PublishedAt.UTC().Truncate(24 * time.Hour)

	w.computeHistoryVeto(snap)
	w.streakCounted = map[string]bool{}
	defer func() { w.historyVeto, w.streakCounted = nil, nil }()

	res := guardResult{
		current: make(map[string]bool, len(snap.Currencies)),
		refuted: map[string]bool{},
		history: make(map[string][]HistoryPoint, len(snap.History7d)),
	}
	batch := make([]FXQuote, 0, len(snap.Currencies)+len(snap.History7d)*7)
	// currentRowIx remembers each ticker's current-day row position in
	// the batch so the history-majority heal below can scrub a poisoned
	// bootstrap row (marked by scrubbing RateUSD to 0; filtered before
	// the insert).
	currentRowIx := make(map[string]int, len(snap.Currencies))
	for _, c := range snap.Currencies {
		if !w.acceptRate(c.Ticker, c.RateUSD) {
			continue
		}
		currentRowIx[c.Ticker] = len(batch)
		batch = append(batch, FXQuote{
			Bucket:  today,
			Ticker:  c.Ticker,
			RateUSD: c.RateUSD,
			Source:  w.sourceLabel(),
		})
	}
	for ticker, points := range snap.History7d {
		var rejected []HistoryPoint
		for _, p := range points {
			if !w.acceptHistoryRate(ticker, p.RateUSD) {
				rejected = append(rejected, p)
				continue
			}
			res.history[ticker] = append(res.history[ticker], p)
			bucket := p.Date.UTC().Truncate(24 * time.Hour)
			// Today's history bar is cached from the day's first fetch; the
			// current row is the sole writer of today's bucket.
			if bucket.Equal(today) {
				continue
			}
			batch = append(batch, FXQuote{
				Bucket:  bucket,
				Ticker:  ticker,
				RateUSD: p.RateUSD,
				Source:  historySource,
			})
		}
		var healed bool
		batch, healed = w.healFromHistoryMajority(ticker, rejected, batch, currentRowIx)
		if healed {
			res.refuted[ticker] = true
			res.history[ticker] = append(res.history[ticker], rejected...)
			sort.Slice(res.history[ticker], func(i, j int) bool {
				return res.history[ticker][i].Date.Before(res.history[ticker][j].Date)
			})
		}
	}
	// A current row counts as accepted only if the heal left it standing.
	for ticker, ix := range currentRowIx {
		res.current[ticker] = batch[ix].RateUSD > 0
	}
	// Drop scrubbed rows (poisoned bootstrap current-day bars).
	clean := batch[:0]
	for _, q := range batch {
		if q.RateUSD > 0 {
			clean = append(clean, q)
		}
	}
	res.batch = clean
	return res
}

// writeBatch persists one guarded batch to fx_quotes and stamps the
// feed's liveness metrics. Safe to call with a nil writer (no-op).
func (w *Worker) writeBatch(ctx context.Context, res guardResult) {
	if w.writer == nil {
		return
	}
	batch := res.batch
	if err := w.writer.InsertFXQuoteBatch(ctx, batch); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("forex: fx_quotes persist failed",
			"rows", len(batch), "err", err)
		return
	}
	w.logger.Info("forex: fx_quotes persisted", "rows", len(batch))

	// Count SOURCE ENTRIES here: this worker bypasses internal/pipeline's sink, the only other writer of
	// SourceEventsTotal, so the active FX feed (the USD anchor behind usd_volume and ADR-0051 local-currency
	// pricing) showed 0 "entries (24h)" on the status page while writing 116 rows a day. Labelled with
	// sourceLabel() so a fallback's rows count against the fallback, not a down primary. Counted after the
	// committed write, and only fresh upstream rates: the synthetic USD anchor and carried history are written
	// even when upstream is dead.
	fresh := res.freshQuotes()
	if fresh > 0 {
		obs.SourceEventsTotal.WithLabelValues(w.sourceLabel()).Add(float64(fresh))
	}

	// Stamp the FX liveness gauge ONLY when the committed write carried an upstream current rate: the USD anchor
	// and carried history are written every refresh, so they would keep a dead feed looking fresh.
	// stellarindex_external_fx_feed_stale keys off this gauge, firing before the 7-day fx_snap lookback expires.
	if fresh > 0 {
		obs.ExternalFXLastQuoteUnix.WithLabelValues(w.sourceLabel()).Set(float64(time.Now().Unix()))
	}
}

// computeHistoryVeto fills [Worker.historyVeto] before the current-rate loop so acceptRate's confirm arm can
// consult it (split from guardSnapshot for gocognit). It uses the FULL trailing-7d series, not the heal's
// rejected subset: a healed baseline accepts its history bars, and those are what must keep refuting a
// still-broken current feed. The caller clears the map.
func (w *Worker) computeHistoryVeto(snap *Snapshot) {
	w.historyVeto = make(map[string]float64, len(snap.History7d))
	for ticker, points := range snap.History7d {
		if med, ok := historyMajority(points); ok {
			w.historyVeto[ticker] = med
		}
	}
}

// healFromHistoryMajority applies the history-majority heal for one ticker after its history bars were
// banded (split from guardSnapshot for gocognit; pinned by the BootstrapPoisonHealed /
// ConfirmedBaselineIsNeverHealed / SplitRejectedSeriesFailsAgreement tests). It fires only against an
// uncorroborated baseline (bootstrapUnconfirmed or healedUncorroborated): one sample loses to
// ≥historyHealMinBars agreeing dated bars. A CONFIRMED baseline is ambiguous from here and stays rejected for
// an operator. On a heal the baseline re-points at the median, the bars join the batch and the current-day
// row is scrubbed via the RateUSD=0 marker; the caller must then stop serving the refuted rate.
func (w *Worker) healFromHistoryMajority(
	ticker string,
	rejected []HistoryPoint,
	batch []FXQuote,
	currentRowIx map[string]int,
) ([]FXQuote, bool) {
	med, ok := historyMajority(rejected)
	if !ok {
		return batch, false
	}
	g := w.guards[ticker]
	if g == nil || (!g.bootstrapUnconfirmed && !g.healedUncorroborated) || withinBand(med, g.lastAccepted) {
		return batch, false
	}
	w.logger.Warn("forex: unconfirmed bootstrap baseline refuted by agreeing history majority; healing",
		"ticker", ticker, "baseline", g.lastAccepted, "median", med,
		"bars", len(rejected))
	obs.ExternalFXBaselineHealedTotal.WithLabelValues(w.sourceLabel()).Inc()
	g.lastAccepted = med
	g.pending = 0
	g.stuckCount = 0
	g.stuckRejectedRate = 0
	g.conflictStuckRate = 0
	g.conflictStuckCount = 0
	g.bootstrapUnconfirmed = false
	g.healedUncorroborated = true
	if ix, ok := currentRowIx[ticker]; ok {
		batch[ix].RateUSD = 0
	}
	for _, p := range rejected {
		batch = append(batch, FXQuote{
			Bucket:  p.Date.UTC().Truncate(24 * time.Hour),
			Ticker:  ticker,
			RateUSD: p.RateUSD,
			Source:  historySource,
		})
	}
	return batch, true
}

// acceptRate is the per-refresh sanity band: it reports whether `rate` may be written and updates the
// ticker's guard state. Rules, in order:
//   - non-finite or non-positive → reject (no 1/rate inverse_usd exists).
//   - no baseline (first sighting since start) → accept and set it; refusing would leave the feed empty.
//   - within [maxRateDeviation] of the last accepted rate → accept.
//   - outside the band but within it of the pending outlier → accept (two fetches agree: a real move),
//     UNLESS the heal-grade 7d majority refutes it ([Worker.vetoConfirmByHistory]): a broken feed agreeing
//     with itself is not corroboration.
//   - otherwise → reject, set pending, count and log.
//
// A reject holds the last accepted row; the confirm arm bounds that hold to one refresh.
func (w *Worker) acceptRate(ticker string, rate float64) bool {
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		w.rejectRate(ticker, rate, 0, "non_finite")
		return false
	}
	if rate <= 0 {
		w.rejectRate(ticker, rate, 0, "non_positive")
		return false
	}
	if w.guards == nil {
		w.guards = map[string]*rateGuard{}
	}
	g, ok := w.guards[ticker]
	if !ok || g.lastAccepted <= 0 {
		w.guards[ticker] = &rateGuard{lastAccepted: rate, bootstrapUnconfirmed: true}
		return true
	}
	if withinBand(rate, g.lastAccepted) {
		g.lastAccepted = rate
		g.pending = 0
		g.bootstrapUnconfirmed = false // second agreeing sample corroborates
		g.healedUncorroborated = false
		g.conflictStuckRate = 0
		g.conflictStuckCount = 0
		return true
	}
	if g.pending > 0 && withinBand(rate, g.pending) {
		if w.vetoConfirmByHistory(ticker, g, rate) {
			return false
		}
		w.logger.Warn("forex: large FX move confirmed by a second fetch; accepting",
			"ticker", ticker, "previous", g.lastAccepted, "rate", rate,
			"band", maxRateDeviation)
		g.lastAccepted = rate
		g.pending = 0
		g.bootstrapUnconfirmed = false
		g.healedUncorroborated = false
		g.conflictStuckRate = 0
		g.conflictStuckCount = 0
		return true
	}
	g.pending = rate
	w.rejectRate(ticker, rate, g.lastAccepted, "deviation")
	return false
}

// vetoConfirmByHistory, called only from acceptRate's pending-confirm arm, refuses the confirm when the
// ticker's heal-grade 7d majority ([Worker.historyVeto]) refutes the candidate by more than
// [maxRateDeviation]: two fetches of the same broken bar are not independent witnesses. History never SETS
// the baseline; only pending (re-armed) and the streak change. A real devaluation still confirms within days
// as the majority follows or splits, so the worst case is days of held rate, not a re-poisoned denominator.
// Repeats within [stuckSameRateTolerance] past [stuckRejectionThreshold] count as
// "deviation_history_conflict_stuck", excluded from the alert like history_deviation_stuck.
func (w *Worker) vetoConfirmByHistory(ticker string, g *rateGuard, rate float64) bool {
	med, ok := w.historyVeto[ticker]
	if !ok || withinBand(rate, med) {
		return false
	}
	if g.conflictStuckRate > 0 &&
		math.Abs(rate-g.conflictStuckRate)/g.conflictStuckRate <= stuckSameRateTolerance {
		g.conflictStuckCount++
	} else {
		g.conflictStuckRate = rate
		g.conflictStuckCount = 1
	}
	reason := "deviation_history_conflict"
	if g.conflictStuckCount > stuckRejectionThreshold {
		reason = "deviation_history_conflict_stuck"
	}
	g.pending = rate
	obs.ExternalFXRateRejectedTotal.WithLabelValues(w.sourceLabel(), reason).Inc()
	w.logger.Warn("forex: pending confirmation refuted by agreeing history majority; refusing",
		"ticker", ticker, "candidate", rate, "median", med,
		"previous", g.lastAccepted, "reason", reason, "band", maxRateDeviation)
	return true
}

// acceptHistoryRate is the [maxRateDeviation] band for a trailing-7d HISTORY point. Unlike [acceptRate] it
// is READ-ONLY on guard state, so a wrong past bar can't corrupt the baseline. A 7d bar sits within a week of
// today's rate, so one >50% off the live baseline is a broken bar about to overwrite a correct stored rate.
// Rules, in order:
//   - non-finite or non-positive → reject.
//   - no baseline → accept (the current-rate loop runs first, so live tickers usually have one).
//   - within [maxRateDeviation] of the last accepted rate → accept.
//   - otherwise → reject, count and log as "history_deviation", or "history_deviation_stuck" (excluded from
//     the alert) after [stuckRejectionThreshold] refusals of the SAME value.
func (w *Worker) acceptHistoryRate(ticker string, rate float64) bool {
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		w.rejectRate(ticker, rate, 0, "non_finite")
		return false
	}
	if rate <= 0 {
		w.rejectRate(ticker, rate, 0, "non_positive")
		return false
	}
	g, ok := w.guards[ticker]
	if !ok || g.lastAccepted <= 0 {
		return true
	}
	if withinBand(rate, g.lastAccepted) {
		// Do NOT zero the stuck streak here: a 7d window is scored bar by bar in one sweep, so an in-band sibling
		// (ETB's six good ~160 bars) would reset the streak the one broken bar (08-19 = 44) just bumped, and the
		// _stuck reclassification would never engage. A NEW broken value still resets to 1 below.
		return true
	}
	// Same broken bar as last time (a tolerance match, since live broken feeds jitter)? Past the threshold the
	// repeat is reclassified so the ALERT carries only fresh disagreement; the bar is refused either way.
	if g.stuckRejectedRate > 0 &&
		math.Abs(rate-g.stuckRejectedRate)/g.stuckRejectedRate <= stuckSameRateTolerance {
		// A break spanning several trailing bars is re-scored in full every
		// refresh; count it once per refresh so the threshold stays ~12 refreshes.
		if !w.streakCounted[ticker] {
			g.stuckCount++
		}
	} else {
		g.stuckRejectedRate = rate
		g.stuckCount = 1
	}
	if w.streakCounted != nil {
		w.streakCounted[ticker] = true
	}
	reason := "history_deviation"
	if g.stuckCount > stuckRejectionThreshold {
		reason = "history_deviation_stuck"
	}
	w.rejectRate(ticker, rate, g.lastAccepted, reason)
	return false
}

// rejectRate records one refused rate: a WARN carrying the ticker (which
// the metric deliberately does not label) and a bump on the bounded
// per-reason counter.
func (w *Worker) rejectRate(ticker string, rate, previous float64, reason string) {
	obs.ExternalFXRateRejectedTotal.WithLabelValues(w.sourceLabel(), reason).Inc()
	w.logger.Warn("forex: rejected upstream rate; keeping last accepted",
		"ticker", ticker, "rate", rate, "previous", previous,
		"reason", reason, "band", maxRateDeviation)
}

// withinBand reports whether `rate` is within [maxRateDeviation] of
// `baseline` in relative terms. baseline is guaranteed positive by the
// caller.
func withinBand(rate, baseline float64) bool {
	return math.Abs(rate-baseline)/baseline <= maxRateDeviation
}

// historyMajority reports the median of a rejected history series and
// whether the series constitutes heal-grade evidence: at least
// [historyHealMinBars] bars, every bar within [historyHealAgreement] of
// the median. A series split across two levels (a genuine mid-week
// redenomination) fails the mutual-agreement test by construction.
func historyMajority(rejected []HistoryPoint) (float64, bool) {
	if len(rejected) < historyHealMinBars {
		return 0, false
	}
	rates := make([]float64, 0, len(rejected))
	for _, p := range rejected {
		// Belt-and-braces (verifier note): fetchHistory already drops
		// non-finite/non-positive bars, but a NaN member here would be
		// invisible to both the med<=0 guard and the agreement check
		// (NaN comparisons are all false) and could bless a NaN median.
		if math.IsNaN(p.RateUSD) || math.IsInf(p.RateUSD, 0) || p.RateUSD <= 0 {
			continue
		}
		rates = append(rates, p.RateUSD)
	}
	if len(rates) < historyHealMinBars {
		return 0, false
	}
	sort.Float64s(rates)
	med := rates[len(rates)/2]
	if med <= 0 {
		return 0, false
	}
	for _, r := range rates {
		if math.Abs(r-med)/med > historyHealAgreement {
			return 0, false
		}
	}
	return med, true
}

// shouldRefreshHistory returns true when the worker should re-pull
// the 7-day historical series. Fires on first install (history nil
// or empty) and once per day thereafter (the published_at date
// rolling forward indicates the upstream snapshot rolled too).
func (w *Worker) shouldRefreshHistory(prevHistory map[string][]HistoryPoint, publishedAt time.Time) bool {
	if len(prevHistory) == 0 {
		return true
	}
	// Tickers can end on different days (a 404 day is skipped per ticker), so
	// decide from the newest bar across all of them, not one map entry.
	var newest time.Time
	for _, points := range prevHistory {
		if len(points) == 0 {
			continue
		}
		if d := points[len(points)-1].Date; d.After(newest) {
			newest = d
		}
	}
	if newest.IsZero() {
		return true
	}
	return newest.Before(publishedAt.Truncate(24 * time.Hour))
}

// fetchHistory pulls the trailing-7d daily snapshots from the
// upstream and assembles a per-ticker series. Days that 404 (e.g.
// weekends for some tickers) are skipped silently — the caller
// gets a series of length ≤ 7 for each ticker.
func (w *Worker) fetchHistory(ctx context.Context, names map[string]string, latest time.Time) map[string][]HistoryPoint {
	if latest.IsZero() {
		latest = time.Now().UTC()
	}
	const window = 7
	// Same case-insensitive join as buildSnapshot: the reused-names
	// path hands this UPPER-keyed names against the client's lower-case
	// dated rates, which would otherwise drop every bar — and with them the
	// evidence the heal and the confirm veto run on.
	names = lowerKeyed(names)
	out := map[string][]HistoryPoint{}
	// Walk oldest → newest so out[ticker] is sorted ascending.
	for i := window - 1; i >= 0; i-- {
		date := latest.AddDate(0, 0, -i).UTC()
		dateStr := date.Format("2006-01-02")
		rates, _, err := w.client.HistoricalUSDRates(ctx, dateStr)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return out
			}
			w.logger.Debug("forex: historical fetch missed",
				"date", dateStr, "err", err)
			continue
		}
		for code, rate := range lowerKeyed(rates) {
			if _, named := names[code]; !named {
				continue
			}
			if rate <= 0 || !isFiniteFloat(rate) {
				continue
			}
			ticker := upper(code)
			out[ticker] = append(out[ticker], HistoryPoint{
				Date:    date,
				RateUSD: rate,
			})
		}
	}
	return out
}

// tickerNames labels every rate code with its own upper-cased ticker —
// the display name of last resort when no names endpoint has answered.
func tickerNames(rates map[string]float64) map[string]string {
	out := make(map[string]string, len(rates))
	for code := range rates {
		out[code] = upper(code)
	}
	return out
}

// upper is local to avoid pulling strings into the worker file.
func upper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
