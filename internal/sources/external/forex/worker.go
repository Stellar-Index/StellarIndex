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

// FXQuoteWriter is the worker's write seam to storage. A nil writer means cache-only mode.
type FXQuoteWriter interface {
	InsertFXQuoteBatch(ctx context.Context, quotes []FXQuote) error
}

// FXQuoteReader is the nil-able read seam that restores held rates after a restart; without it a cold start holds nothing.
type FXQuoteReader interface {
	// LatestFXQuotes returns the newest row per ticker with Bucket >= since.
	LatestFXQuotes(ctx context.Context, since time.Time) ([]FXQuote, error)
}

// FXQuote is the storage-layer record the worker writes per refresh, mirroring timescale.FXQuote because importing internal/storage/timescale would cycle through the v1 API package.
type FXQuote struct {
	Bucket time.Time
	Ticker string
	//floatmoney:ok known debt — ingest DTO feeding timescale.FXQuote.RateUSD (same debt); upstream ECB/fallback sources are JSON floats today
	RateUSD float64
	Source  string
}

// maxRateDeviation is the per-refresh sanity band: a rate further than this fraction from the ticker's last ACCEPTED rate is not
// written on first sighting. fx_quotes is the denominator of every fiat usd_volume; 50% still admits real one-day devaluations (~40%).
const maxRateDeviation = 0.50

// fxSource is the source tag on persisted rows and the rejection metric's `source` label.
const fxSource = "massive"

// historySource labels dated bars: fetchHistory reads only the primary client.
const historySource = fxSource

// rateGuard is the per-ticker band state: lastAccepted is the baseline; pending holds an outlier rejected ONCE so a second, agreeing
// fetch can confirm it. A one-off bad bar never reaches fx_quotes; a real devaluation is accepted ~1h later.
type rateGuard struct {
	lastAccepted float64
	pending      float64

	// Stuck-upstream tracking for the HISTORY band: after [stuckRejectionThreshold] consecutive refusals of the SAME bar, repeats count as
	// `history_deviation_stuck` (logged, no alert). Any acceptance or a different rejected value resets it.
	//floatmoney:ok known debt — holds a copy of the same float64 RateUSD (above) for equality/streak comparison, not persisted or served
	stuckRejectedRate float64
	stuckCount        int

	// Streak for the CONFIRM VETO, quieting repeats of a feed stuck on one broken current bar. Separate from the history-band fields;
	// reset by any accepted current rate.
	//floatmoney:ok known debt — same class as stuckRejectedRate above
	conflictStuckRate  float64
	conflictStuckCount int

	// bootstrapUnconfirmed marks a baseline seeded from one uncorroborated sample, so a broken first sample cannot reject correct history
	// and the heal in [Worker.guardSnapshot] can scrub the current-day row. Cleared by a within-band acceptance or a two-fetch
	// confirmation.
	bootstrapUnconfirmed bool

	// healedUncorroborated marks a heal-installed baseline no current fetch has agreed with yet, so corrected history can re-point it.
	// Cleared as bootstrapUnconfirmed.
	healedUncorroborated bool
}

// stuckRejectionThreshold is how many consecutive identical history-bar refusals mark a ticker stuck; hourly refreshes make 12 half a
// day, long enough that a real scale change has already alerted.
const stuckRejectionThreshold = 12

// stuckSameRateTolerance treats rejected bars within this relative distance as the SAME stuck value: a live broken feed jitters, so
// exact equality never engages; 1% is tighter than any real day-over-day FX move and wider than provider jitter.
const stuckSameRateTolerance = 0.01

// History-majority heal: when [historyHealMinBars] rejected trailing-7d bars agree within
// [historyHealAgreement] of their median and the baseline is an uncorroborated bootstrap, the
// baseline is the outlier and is re-pointed at the median. The agreement band is far tighter than
// [maxRateDeviation], so a series spanning levels never heals; a CONFIRMED baseline is never
// healed, because agreeing current fetches against agreeing history means one endpoint is broken
// and only an operator can say which.
const (
	historyHealMinBars   = 4
	historyHealAgreement = 0.10
)

// Worker periodically fetches upstream rates and names into a Cache for the API process lifetime.
type Worker struct {
	client      *Client
	cache       *Cache
	writer      FXQuoteWriter
	reader      FXQuoteReader
	logger      *slog.Logger
	interval    time.Duration
	circulation map[string]CirculationEntry // loaded once at startup

	// guards holds per-ticker sanity-band state, touched only from refreshOnce -> guardSnapshot
	// (single goroutine: Run owns the loop). The first refresh has no baseline and establishes one.
	guards map[string]*rateGuard

	// historyVeto is the per-ticker 7d history-majority median ([historyMajority]), scoped to one guardSnapshot call. acceptRate's confirm
	// arm uses it: two agreeing samples from ONE broken endpoint are not two witnesses when 4+ dated bars refute them. It only refuses a
	// confirm.
	historyVeto map[string]float64

	// streakCounted is guardSnapshot-scoped: tickers whose stuck streak already moved this refresh,
	// so the streak counts refreshes, not broken bars. nil outside a refresh (every refusal counts).
	streakCounted map[string]bool

	// rawHistory is the trailing-7d series as fetched, including bars the band refuses. Never published: the heal and confirm veto must
	// re-see refused bars each refresh, while [Snapshot.History7d] carries only admitted bars.
	rawHistory map[string][]HistoryPoint

	// fallbacks are tried IN ORDER when the primary cannot serve rates, so an outage degrades
	// coverage instead of ending it.
	fallbacks []RateProvider

	// corroborator is held for the FX fixings plan and never consulted yet.
	corroborator RateProvider

	// activeSource is the feed behind the CURRENT snapshot, stamped into fx_quotes.source and the
	// `source` metric label so the metric shows the live feed. Single-goroutine, like guards.
	activeSource string

	// fixingWriter, when set, receives the vendor-time hourly bars after every refresh
	// (fx_fixings); fixingNewest is the newest committed bar_end. Only touched from refreshOnce.
	fixingWriter FXFixingWriter
	fixingNewest time.Time

	// now pins the clock in tests; nil is time.Now.
	now func() time.Time
}

// NewWorker constructs the worker; Massive's grouped aggregate is daily, so sub-15-min polling just re-fetches the same bar.
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

// WithFallbacks registers standby rate providers, tried in order when the primary fails.
func (w *Worker) WithFallbacks(providers ...RateProvider) *Worker {
	w.fallbacks = append(w.fallbacks, providers...)
	return w
}

// WithCorroborator registers a provider outside the serving chain; it is stored only, never fetched or stamped as a source.
func (w *Worker) WithCorroborator(p RateProvider) *Worker {
	w.corroborator = p
	return w
}

// Corroborator returns the provider registered by WithCorroborator, or nil.
func (w *Worker) Corroborator() RateProvider { return w.corroborator }

// WithWriter attaches a quote writer so each successful refresh also persists rates and history to fx_quotes; a nil writer keeps the worker cache-only.
func (w *Worker) WithWriter(writer FXQuoteWriter) *Worker {
	w.writer = writer
	return w
}

// WithReader attaches the fx_quotes reader the first refresh seeds held rates from; nil keeps a cold start holding nothing.
func (w *Worker) WithReader(reader FXQuoteReader) *Worker {
	w.reader = reader
	return w
}

// Run blocks until ctx is cancelled, fetching immediately then once per interval; failures are logged, never fatal, and the cache keeps the prior snapshot.
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

// sourceLabel is the feed name stamped on persisted rows and metric labels. It falls back to the
// primary when activeSource is unset so a zero-value Worker never writes an empty source.
func (w *Worker) sourceLabel() string {
	if w.activeSource == "" {
		return fxSource
	}
	return w.activeSource
}

// fetchRates returns rates from the first source that can serve them (primary, then fallbacks in registration order). Context
// cancellation short-circuits rather than burning through every fallback.
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
			// Loud on purpose: serving from a standby is a degraded state, not a silent swap.
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
		// Names are static labels: losing them must not cost a refresh whose rates we already
		// have, which matters most when the primary is down. Reuse the last snapshot's names.
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
			// Cold start: buildSnapshot drops unnamed codes and fetchHistory only walks named
			// ones, so an empty map would install nothing. Label rates by ticker until names return.
			names = tickerNames(rates)
			w.logger.Warn("forex: names fetch failed and no cached names — labelling rates by ticker",
				"err", err, "currencies", len(names))
		}
	}

	// History is re-fetched once a day. It is the RAW series, private to the worker: the heal and
	// the confirm veto need the bars the band REFUSED, while the served snapshot carries only
	// admitted ones.
	if w.shouldRefreshHistory(w.rawHistory, publishedAt) {
		w.rawHistory = w.fetchHistory(ctx, names, publishedAt)
	}

	// GUARD FIRST, INSTALL SECOND: only what the band clears reaches the cache that /v1/price's fiat
	// paths read; banding just the fx_quotes write would still price off a bad cached rate. The band
	// runs with or without a writer.
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

// maxHeldRateAge bounds how long [servedSnapshot] serves a rate no refresh has re-confirmed; it mirrors fxQuotesSnapLookback in
// internal/storage/timescale so the cache and the table go dark for a ticker at the same age.
const maxHeldRateAge = 7 * 24 * time.Hour

// guardResult is one pass of the sanity band: rows cleared for fx_quotes plus the same verdicts for [servedSnapshot], so the table and
// the cache never disagree about what was accepted.
type guardResult struct {
	// batch is the fx_quotes write: accepted current rows plus accepted (and healed) dated bars.
	batch []FXQuote
	// current is the tickers whose CURRENT rate the band accepted and the heal did not scrub.
	current map[string]bool
	// refuted is the tickers whose BASELINE the heal overturned; their last served rate is the
	// sample their own history refuted, so it must not be held either.
	refuted map[string]bool
	// history is the dated bars the band admitted, per ticker.
	history map[string][]HistoryPoint
}

// freshQuotes counts the upstream current rates this refresh committed. The synthetic anchor and
// carried-forward history bars are excluded: both are written whether or not the feed answered.
func (r guardResult) freshQuotes() int {
	n := 0
	for ticker, ok := range r.current {
		if ok && ticker != anchorTicker {
			n++
		}
	}
	return n
}

// servedSnapshot builds the cached snapshot from the raw one and the band's verdicts. An accepted rate is served as fetched; a refused
// or absent one holds the last served entry (original UpdateAt) until [maxHeldRateAge], then drops it (fail closed). A heal-refuted
// baseline drops at once.
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

// seedSnapshot rebuilds the previous process's held rates from fx_quotes, since upstream republishes tickers gradually and a restart
// would otherwise drop those not yet re-sent. It only stands in for prev: servedSnapshot still prefers this refresh's accepted rate.
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

// persistSnapshot bands and writes one snapshot to fx_quotes ([Worker.guardSnapshot] then [Worker.writeBatch]); a nil writer is a
// no-op. Everything is banded because one bad bar would mis-scale a currency's whole usd_volume history; an accepted bar dated today is
// skipped (stale first-fetch value).
func (w *Worker) persistSnapshot(ctx context.Context, snap *Snapshot) {
	if w.writer == nil || snap == nil {
		return
	}
	w.writeBatch(ctx, w.guardSnapshot(snap))
}

// guardSnapshot runs the sanity band over one raw snapshot and returns its verdicts. It is the ONLY place band state advances and must
// run exactly once per refresh: acceptRate arms pending on a refusal, so a second pass would confirm the outlier the first refused.
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
	// currentRowIx lets the history-majority heal scrub a poisoned bootstrap row (RateUSD set to 0,
	// filtered before the insert).
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
			// Today's history bar is stale from the day's first fetch; the current row owns today.
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

// writeBatch persists one guarded batch to fx_quotes and stamps the feed's liveness metrics.
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

	// This worker bypasses internal/pipeline's sink, so it counts SourceEventsTotal itself, labelled
	// so a fallback's rows count against the fallback. Only fresh upstream rates count: the
	// synthetic USD anchor and carried history are written even when upstream is dead.
	fresh := res.freshQuotes()
	if fresh > 0 {
		obs.SourceEventsTotal.WithLabelValues(w.sourceLabel()).Add(float64(fresh))
	}

	// Stamp the liveness gauge ONLY when the write carried an upstream current rate, else the
	// anchor and carried history would keep a dead feed looking fresh. The feed_stale alert keys
	// off it, firing before the 7-day fx_snap lookback expires.
	if fresh > 0 {
		obs.ExternalFXLastQuoteUnix.WithLabelValues(w.sourceLabel()).Set(float64(time.Now().Unix()))
	}
}

// computeHistoryVeto fills [Worker.historyVeto] before the current-rate loop from the FULL trailing-7d series, not the heal's rejected
// subset: a healed baseline's history bars must keep refuting a still-broken current feed. The caller clears the map.
func (w *Worker) computeHistoryVeto(snap *Snapshot) {
	w.historyVeto = make(map[string]float64, len(snap.History7d))
	for ticker, points := range snap.History7d {
		if med, ok := historyMajority(points); ok {
			w.historyVeto[ticker] = med
		}
	}
}

// healFromHistoryMajority applies the history-majority heal for one ticker. It fires only against an uncorroborated baseline (a
// CONFIRMED one stays rejected for an operator); on a heal the baseline re-points at the median and the current-day row is scrubbed
// (RateUSD=0).
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

// acceptRate is the per-refresh sanity band: reports whether `rate` may be written and updates guard state. Non-finite or non-positive
// rates are rejected; with no baseline it accepts. Outside the band, two agreeing fetches confirm a real move unless the 7d majority
// refutes it.
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

// vetoConfirmByHistory refuses acceptRate's confirm when the 7d majority ([Worker.historyVeto]) refutes the candidate by more than
// [maxRateDeviation]: two fetches of one broken bar are not independent witnesses. Repeats past [stuckRejectionThreshold] count as
// "deviation_history_conflict_stuck".
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

// acceptHistoryRate is the [maxRateDeviation] band for a trailing-7d HISTORY point. Unlike [acceptRate] it is READ-ONLY on guard state,
// so a wrong past bar cannot corrupt the baseline. Non-finite or non-positive rates are rejected; "history_deviation_stuck" is excluded
// from the alert.
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
		// Do NOT zero the stuck streak here: the 7d window is scored bar by bar in one sweep, so
		// in-band sibling bars would reset the streak a broken bar just bumped and the _stuck
		// reclassification would never engage. A NEW broken value still resets to 1 below.
		return true
	}
	// Same broken bar as last time (tolerance match, since live broken feeds jitter)? Past the
	// threshold the repeat is reclassified so the ALERT carries only fresh disagreement; the bar is
	// refused either way.
	if g.stuckRejectedRate > 0 &&
		math.Abs(rate-g.stuckRejectedRate)/g.stuckRejectedRate <= stuckSameRateTolerance {
		// A multi-bar break is re-scored in full every refresh; count it once per refresh so the
		// threshold stays ~12 refreshes.
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

// rejectRate records one refused rate: a WARN carrying the ticker (deliberately not a metric
// label, to bound cardinality) and a bump on the per-reason counter.
func (w *Worker) rejectRate(ticker string, rate, previous float64, reason string) {
	obs.ExternalFXRateRejectedTotal.WithLabelValues(w.sourceLabel(), reason).Inc()
	w.logger.Warn("forex: rejected upstream rate; keeping last accepted",
		"ticker", ticker, "rate", rate, "previous", previous,
		"reason", reason, "band", maxRateDeviation)
}

// withinBand reports whether rate is within [maxRateDeviation] of baseline, which the caller
// guarantees positive.
func withinBand(rate, baseline float64) bool {
	return math.Abs(rate-baseline)/baseline <= maxRateDeviation
}

// historyMajority reports the median of a rejected history series and whether it is heal-grade: at least [historyHealMinBars] bars, all
// within [historyHealAgreement] of the median. A series split across two levels (a real redenomination) fails.
func historyMajority(rejected []HistoryPoint) (float64, bool) {
	if len(rejected) < historyHealMinBars {
		return 0, false
	}
	rates := make([]float64, 0, len(rejected))
	for _, p := range rejected {
		// fetchHistory already drops these, but a NaN here would pass both the med<=0 guard and
		// the agreement check (NaN comparisons are false) and could bless a NaN median.
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

// shouldRefreshHistory reports whether to re-pull the 7-day series: on first install, then once
// per day as the published_at date rolls forward.
func (w *Worker) shouldRefreshHistory(prevHistory map[string][]HistoryPoint, publishedAt time.Time) bool {
	if len(prevHistory) == 0 {
		return true
	}
	// Tickers can end on different days (a 404 day is skipped per ticker): use the newest bar overall.
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

// fetchHistory assembles a per-ticker series from the trailing-7d daily snapshots. Days that 404
// (e.g. weekends for some tickers) are skipped, so a series has at most 7 points.
func (w *Worker) fetchHistory(ctx context.Context, names map[string]string, latest time.Time) map[string][]HistoryPoint {
	if latest.IsZero() {
		latest = time.Now().UTC()
	}
	const window = 7
	// Case-insensitive join as in buildSnapshot: reused UPPER-keyed names against lower-case dated
	// rates would otherwise drop every bar, and with them the heal and confirm-veto evidence.
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

// tickerNames labels each rate code with its upper-cased ticker, the last-resort display name.
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
