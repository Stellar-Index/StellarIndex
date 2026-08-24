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

// FXQuote is the storage-layer record the worker writes per refresh.
// Mirrors the timescale.FXQuote shape but lives in this package so
// the forex worker doesn't import internal/storage/timescale (which
// imports forex types via the v1 API package; the dependency would
// cycle).
type FXQuote struct {
	Bucket     time.Time
	Ticker     string
	RateUSD    float64
	InverseUSD float64
	Source     string
}

// maxRateDeviation is the per-refresh sanity band on an upstream FX rate:
// a ticker whose new rate differs from its last ACCEPTED rate by more than
// this fraction is not written to fx_quotes on the first sighting
// (C2-030, audit-2026-07-23).
//
// Why 0.50 and not something tighter: fx_quotes is the denominator of
// every fiat-quoted usd_volume, so the band exists to catch a BROKEN BAR
// — a decimal shift (10x = 900% deviation), a unit-scale change, a
// redenomination applied upstream without a ticker change — not to
// second-guess the FX market. Real single-step fiat devaluations do get
// large: EGP fell ~38% in a day (Mar 2024), NGN ~40% (Jun 2023), TRY ~25%
// (Dec 2021). A band tighter than those would reject genuine history and
// wedge the ticker; 50% sits above every such episode while still being
// far below any decimal-shift error.
const maxRateDeviation = 0.50

// fxSource is the source tag stamped on persisted rows and on the
// rejection metric's `source` label.
const fxSource = "massive"

// rateGuard is the per-ticker state behind the sanity band. lastAccepted
// is the baseline a new rate is measured against; pending holds an
// outlier that was rejected ONCE so a second, agreeing fetch can confirm
// it.
//
// The two-strike shape is the whole point: a one-off bad bar never
// reaches fx_quotes, but a REAL devaluation — which the upstream will
// keep reporting — is accepted on the next refresh (≈1 h of lag) instead
// of wedging the ticker on a stale rate forever. A permanent reject would
// trade one wrong number for an indefinitely wrong one.
type rateGuard struct {
	lastAccepted float64
	pending      float64

	// Stuck-upstream tracking for the HISTORY band (2026-08-24, the
	// Massive ETB=44 incident): a provider serving the SAME broken
	// historical bar refresh after refresh is a documented, already-
	// handled condition — the guard keeps refusing it — but counting
	// every repeat under `history_deviation` kept the rejection alert
	// firing for days with no new information, training operators to
	// ignore the channel. After [stuckRejectionThreshold] consecutive
	// refusals of the SAME value, further repeats count under the
	// `history_deviation_stuck` reason (excluded from the alert, still
	// WARN-logged + graphable). Any acceptance, or a DIFFERENT rejected
	// value, resets the streak — fresh disagreement always alerts.
	stuckRejectedRate float64
	stuckCount        int

	// bootstrapUnconfirmed marks a baseline seeded from a SINGLE
	// upstream sample (the no-baseline bootstrap arm) that nothing has
	// corroborated yet. The 2026-08-24 Massive UZS incident: at process
	// restart the current feed served a broken 1820 (true level ≈
	// 11,800), the bootstrap arm accepted it sight-unseen, and the
	// guard then spent the rest of the day rejecting the CORRECT 7-day
	// history against the poisoned baseline — evidence pointing the
	// wrong way. The flag lets the history-majority heal in
	// [Worker.persistSnapshot] also scrub the poisoned current-day row
	// from the write batch: a bootstrap row that the ticker's own
	// history refutes was never evidence. Cleared by any within-band
	// acceptance or a two-fetch pending confirmation (two agreeing
	// samples = corroborated).
	bootstrapUnconfirmed bool
}

// stuckRejectionThreshold is how many consecutive identical history-bar
// refusals reclassify the ticker as stuck. The worker refreshes hourly,
// so 12 ≈ half a day of the provider serving the same broken bar —
// far past the point where repeats carry signal, and long enough that
// a real multi-refresh scale change has already fired the alert.
const stuckRejectionThreshold = 12

// stuckSameRateTolerance treats consecutive rejected bars within this
// relative distance as the SAME stuck value. The original exact float
// equality never matched a LIVE broken upstream — Massive's UZS current
// feed jittered (11791.69 → 11785 → 11817.69 …) around the level the
// poisoned baseline rejected, so the streak reset on every refresh and
// the reclassification could not engage. 1% is far tighter than any
// genuine day-over-day FX move that should read as fresh news, and far
// wider than provider jitter.
const stuckSameRateTolerance = 0.01

// History-majority heal (2026-08-24 Massive UZS incident): when one
// refresh rejects at least [historyHealMinBars] of a ticker's trailing-7d
// bars, those rejected bars agree with each other within
// [historyHealAgreement] of their median, AND the baseline is a
// still-unconfirmed bootstrap (one uncorroborated sample), the BASELINE
// is the outlier, not the bars — ≥4 independent dated samples beat the
// single sample the baseline came from. persistSnapshot then re-points
// lastAccepted at the bars' median, admits the bars, and scrubs the
// poisoned current-day row from the batch.
//
// Two deliberate limits keep this from becoming MR-1 in a new coat:
//   - The agreement band is much tighter than [maxRateDeviation]. Nuance
//     (verifier 2026-08-24): for a redenomination at FIRST-EVER sighting,
//     the in-band half of the split series is simply ACCEPTED, so the
//     rejected subset can still be homogeneous — there the heal may flip
//     the baseline to the other level and the two-fetch pending
//     confirmation restores it within ~2 refreshes (per-date rows stay
//     correct throughout; executed in the verifier's trace). The
//     agreement test is the guard for the broken-bootstrap case where
//     the rejected series itself spans levels.
//   - A CONFIRMED baseline is never healed: two agreeing current fetches
//     vs an agreeing history series means one of the provider's two
//     endpoints is systemically broken and we cannot tell which from in
//     here — those tickers keep rejecting (stuck-reclassified, quiet
//     alert) and wait for an operator.
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
	logger      *slog.Logger
	interval    time.Duration
	circulation map[string]CirculationEntry // loaded once at startup

	// guards holds the sanity-band state per ticker. Only touched from
	// refreshOnce → persistSnapshot, which is single-goroutine (Run owns
	// the ticker loop). Empty on process start: the first refresh has no
	// baseline, so it accepts and establishes one.
	guards map[string]*rateGuard
}

// NewWorker constructs the worker. interval is the refresh
// cadence — Massive's hourly grain means anything < 15 min is
// wasted fetches; 1h is a reasonable default that keeps the
// cache fresh across operator restarts.
//
// The curated monetary-base CSV is loaded once at construction
// (lives in internal/sources/external/forex/circulation_data.csv). Parse
// errors per row are non-fatal: rows that parse install, the
// rest are logged as a warning. The map is then attached to
// every snapshot built by refreshOnce.
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
	}
}

// WithWriter attaches a persistent quote writer. When set, every
// successful refreshOnce also persists the latest rates + history
// to the fx_quotes hypertable. nil writer keeps the worker in
// cache-only mode (the pre-fx_quotes behaviour).
func (w *Worker) WithWriter(writer FXQuoteWriter) *Worker {
	w.writer = writer
	return w
}

// Run blocks until ctx is cancelled. Fetches once immediately so
// the cache is populated before the first /v1/currencies request
// (subject to the upstream's response time), then refreshes every
// interval. Failures are logged but never crash the worker — the
// cache holds the prior snapshot until a refresh succeeds.
func (w *Worker) Run(ctx context.Context) error {
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
func (w *Worker) refreshOnce(ctx context.Context) {
	rates, publishedAt, err := w.client.LatestUSDRates(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("forex: rates fetch failed", "err", err)
		return
	}
	names, err := w.client.CurrencyNames(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("forex: names fetch failed", "err", err)
		return
	}

	// Carry forward the prior snapshot's history while we backfill.
	// The first install on a fresh worker has no history yet — the
	// per-currency page renders the sparkline panel as "—" until a
	// later refresh fills it in. We re-fetch history once a day
	// (cheap on the upstream's CDN — 7 dated URLs, all cached).
	var history map[string][]HistoryPoint
	if prev := w.cache.Latest(); prev != nil {
		history = prev.History7d
	}
	if w.shouldRefreshHistory(history, publishedAt) {
		history = w.fetchHistory(ctx, names, publishedAt)
	}

	snap := buildSnapshot(rates, names, publishedAt, time.Now().UTC(), history, w.circulation)
	w.cache.Set(snap)
	w.logger.Info("forex: snapshot installed",
		"currencies", len(snap.Currencies),
		"history_currencies", len(snap.History7d),
		"published_at", publishedAt,
	)

	w.persistSnapshot(ctx, snap)
}

// persistSnapshot writes the latest rates + history to fx_quotes if
// a writer is attached. Safe to call with a nil writer (no-op).
//
// Two writes happen:
//  1. Today's row per ticker, from `snap.Currencies` — the canonical
//     "current" rate. Re-running upserts on the (ticker, today) PK so
//     repeated refreshes within the same day idempotently update the
//     row.
//  2. Trailing-7d rows from `snap.History7d` — these only differ on
//     the first install of each day (the worker's gap-detector
//     short-circuits unchanged history).
//
// Every "current" rate passes the [maxRateDeviation] sanity band before it
// is written (C2-030): fx_quotes is the denominator of every fiat-quoted
// usd_volume, so one bad upstream bar would mis-scale a whole currency's
// history. Rejections are logged at WARN and counted on
// [obs.ExternalFXRateRejectedTotal]; the ticker keeps its last accepted
// row rather than gaining a wrong one.
//
// The trailing-7d history rows are ALSO banded (MR-1, audit-2026-08-14).
// They are dated snapshots, not a moving current rate, so [acceptHistoryRate]
// bands each point against the ticker's current accepted rate WITHOUT
// mutating the guard state: a trailing-7d bar sits at most a week from
// today's rate, far inside the 50% decimal-shift band, so a bar >50% off
// the live rate is a broken upstream bar about to overwrite a correct
// stored rate in place — exactly the denominator poisoning MR-1 describes.
// A point with no baseline yet (the current-rate loop runs first and
// establishes one) still bootstraps rather than dropping legitimate history.
//
// Errors get logged at warn level; persistence is best-effort
// alongside the in-memory cache, never a crash condition.
func (w *Worker) persistSnapshot(ctx context.Context, snap *Snapshot) {
	if w.writer == nil || snap == nil {
		return
	}
	today := snap.PublishedAt.UTC().Truncate(24 * time.Hour)

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
			Bucket:     today,
			Ticker:     c.Ticker,
			RateUSD:    c.RateUSD,
			InverseUSD: 1.0 / c.RateUSD,
			Source:     fxSource,
		})
	}
	for ticker, points := range snap.History7d {
		var rejected []HistoryPoint
		for _, p := range points {
			if !w.acceptHistoryRate(ticker, p.RateUSD) {
				rejected = append(rejected, p)
				continue
			}
			batch = append(batch, FXQuote{
				Bucket:     p.Date.UTC().Truncate(24 * time.Hour),
				Ticker:     ticker,
				RateUSD:    p.RateUSD,
				InverseUSD: 1.0 / p.RateUSD,
				Source:     fxSource,
			})
		}
		// History-majority heal (see the constants' doc). Fires ONLY
		// against a bootstrapUnconfirmed baseline: one uncorroborated
		// sample vs ≥historyHealMinBars mutually-agreeing dated bars is
		// unambiguous — the sample loses. A CONFIRMED baseline (two
		// agreeing current fetches) against an agreeing history series
		// is genuinely ambiguous from in here (which endpoint is
		// broken?), so it stays rejected + stuck-reclassified for an
		// operator instead of letting a systemically-broken history
		// endpoint overwrite a correct baseline — the MR-1 poisoning in
		// a new coat.
		if med, ok := historyMajority(rejected); ok {
			g := w.guards[ticker]
			if g == nil || !g.bootstrapUnconfirmed || withinBand(med, g.lastAccepted) {
				continue
			}
			w.logger.Warn("forex: unconfirmed bootstrap baseline refuted by agreeing history majority; healing",
				"ticker", ticker, "baseline", g.lastAccepted, "median", med,
				"bars", len(rejected))
			obs.ExternalFXBaselineHealedTotal.WithLabelValues(fxSource).Inc()
			g.lastAccepted = med
			g.pending = 0
			g.stuckCount = 0
			g.stuckRejectedRate = 0
			g.bootstrapUnconfirmed = false
			// The current-day row came from the very sample the
			// majority refuted — scrub it (marker filtered below).
			if ix, ok := currentRowIx[ticker]; ok {
				batch[ix].RateUSD = 0
			}
			for _, p := range rejected {
				batch = append(batch, FXQuote{
					Bucket:     p.Date.UTC().Truncate(24 * time.Hour),
					Ticker:     ticker,
					RateUSD:    p.RateUSD,
					InverseUSD: 1.0 / p.RateUSD,
					Source:     fxSource,
				})
			}
		}
	}
	// Drop scrubbed rows (poisoned bootstrap current-day bars).
	clean := batch[:0]
	for _, q := range batch {
		if q.RateUSD > 0 {
			clean = append(clean, q)
		}
	}
	batch = clean

	if err := w.writer.InsertFXQuoteBatch(ctx, batch); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("forex: fx_quotes persist failed",
			"rows", len(batch), "err", err)
		return
	}
	w.logger.Info("forex: fx_quotes persisted", "rows", len(batch))

	// Stamp the FX-feed liveness gauge ONLY on a committed non-empty
	// write. An empty batch (upstream returned no usable rates) or a
	// failed InsertFXQuoteBatch (returned above) deliberately leaves the
	// prior stamp untouched so a wedged-but-erroring worker cannot keep
	// the feed looking fresh. The staleness of this gauge is what the
	// stellarindex_external_fx_feed_stale alert keys off — it catches a
	// dry fiat-FX feed BEFORE the 7-day fx_snap lookback expires and
	// fiat-quoted pairs silently break.
	if len(batch) > 0 {
		obs.ExternalFXLastQuoteUnix.WithLabelValues(fxSource).Set(float64(time.Now().Unix()))
	}
}

// acceptRate is the C2-030 sanity band. It reports whether `rate` for
// `ticker` may be written to fx_quotes, and updates the per-ticker guard
// state as a side effect.
//
// Accept / reject rules, in order:
//   - non-finite or non-positive → reject (a broken upstream field can
//     never be a rate; 1/rate would poison InverseUSD too).
//   - no baseline yet (first sighting since process start) → accept and
//     establish the baseline. There is nothing to compare against, and
//     refusing to bootstrap would leave the feed permanently empty.
//   - within [maxRateDeviation] of the last accepted rate → accept.
//   - outside the band but WITHIN the band of the outlier we already
//     rejected once → accept: two independent fetches agree, so this is
//     a real move (devaluation / redenomination), not a bad bar.
//   - otherwise → reject, remember it as pending, count + log.
//
// Rejecting holds the ticker on its last accepted row rather than writing
// a wrong one; the confirmation arm bounds that hold to one refresh
// interval.
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
		return true
	}
	if g.pending > 0 && withinBand(rate, g.pending) {
		w.logger.Warn("forex: large FX move confirmed by a second fetch; accepting",
			"ticker", ticker, "previous", g.lastAccepted, "rate", rate,
			"band", maxRateDeviation)
		g.lastAccepted = rate
		g.pending = 0
		g.bootstrapUnconfirmed = false
		return true
	}
	g.pending = rate
	w.rejectRate(ticker, rate, g.lastAccepted, "deviation")
	return false
}

// acceptHistoryRate is the [maxRateDeviation] sanity band applied to a
// trailing-7d HISTORY point (MR-1, audit-2026-08-14). Unlike [acceptRate]
// it is READ-ONLY on the guard state — a dated historical bar is not the
// moving "current" rate, so it must neither advance lastAccepted nor arm
// the pending confirmation slot (doing so would let a wrong past bar
// corrupt the baseline the current-rate band depends on).
//
// It bands the point against the ticker's current accepted baseline:
// fx_quotes.rate_usd is the denominator of every fiat-quoted usd_volume,
// and a trailing-7d bar is at most a week from today's rate, far inside
// the 50% decimal-shift band, so a bar >50% off the live rate is a broken
// upstream bar (provider glitch) about to overwrite a correct stored rate
// in place — the exact durability hole MR-1 describes.
//
// Rules, in order:
//   - non-finite or non-positive → reject (as [acceptRate]; a broken field
//     can never be a rate and 1/rate would poison InverseUSD).
//   - no baseline yet for the ticker → accept: there is nothing to band
//     against and refusing would drop legitimate history (mirrors the
//     acceptRate bootstrap arm). The current-rate loop runs first, so a
//     live ticker normally already has a baseline here.
//   - within [maxRateDeviation] of the last accepted rate → accept.
//   - otherwise → reject, count + log (reason "history_deviation"; after
//     [stuckRejectionThreshold] consecutive refusals of the SAME value the
//     reason becomes "history_deviation_stuck" — excluded from the
//     rejection alert, so a provider stuck on one documented broken bar
//     stops re-paging while fresh disagreement still does).
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
		// Upstream agrees again — a later deviation is fresh news.
		// (Writing the stuck fields does not violate this method's
		// read-only contract, which is about lastAccepted/pending —
		// the CURRENT-rate baseline a historical bar must never
		// advance. The stuck streak is history-band bookkeeping.)
		g.stuckCount = 0
		g.stuckRejectedRate = 0
		return true
	}
	// Same broken bar, again? Track the streak; past the threshold the
	// repeat is reclassified so the rejection ALERT only carries fresh
	// disagreement (the guard still refuses the bar either way — see
	// the stuck fields on rateGuard for the incident this encodes).
	// "Same" is a tolerance match, not float equality: a LIVE broken
	// upstream jitters around its wrong level (Massive UZS: 11791.69 →
	// 11785 → 11817.69), and exact equality reset the streak on every
	// refresh, so the reclassification never engaged.
	if g.stuckRejectedRate > 0 &&
		math.Abs(rate-g.stuckRejectedRate)/g.stuckRejectedRate <= stuckSameRateTolerance {
		g.stuckCount++
	} else {
		g.stuckRejectedRate = rate
		g.stuckCount = 1
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
	obs.ExternalFXRateRejectedTotal.WithLabelValues(fxSource, reason).Inc()
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
	// Sample any one ticker's most-recent date — they all share the
	// same upstream date roll.
	for _, points := range prevHistory {
		if len(points) == 0 {
			continue
		}
		newest := points[len(points)-1].Date
		return newest.Before(publishedAt.Truncate(24 * time.Hour))
	}
	return true
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
		for code, rate := range rates {
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
