package v1

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// SEP40Price is the SEP-40 passthrough shape (/v1/oracle/lastprice, x_last_price), deliberately minimal to `(price, timestamp)` like on-chain oracles.
type SEP40Price struct {
	Asset     string   `json:"asset"`
	Price     string   `json:"price"`
	Timestamp WireTime `json:"timestamp"`
	// Credit names the vendor when every source behind the price is one licensed off-chain feed;
	// absent for on-chain venues, oracles and mixes.
	Credit string `json:"credit,omitempty"`
}

// creditForSources names the one licensed vendor behind every source, or nothing when any source
// is on-chain or the sources span vendors: a composite is credited to no one.
func creditForSources(sources []string) string {
	credit := ""
	for _, src := range sources {
		c := external.Credit(src)
		if c == "" || (credit != "" && c != credit) {
			return ""
		}
		credit = c
	}
	return credit
}

// handleOracleLastPrice serves GET /v1/oracle/lastprice?asset=<id>: SEP-40 `lastprice` passthrough, quote always fiat:USD (other quotes:
// /v1/price, /v1/oracle/x_last_price).
// 404 when no observation exists. A frozen pair (ADR-0019) serves the held last-known-good with flags.frozen, or 503 when none is held;
// never the refused bucket.
func (s *Server) handleOracleLastPrice(w http.ResponseWriter, r *http.Request) {
	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	asset, ok := parseOracleAsset(w, r, "lastprice")
	if !ok {
		return
	}

	// 8s ceiling on the price read, matching every other
	// hypertable/Redis-backed handler on this surface (markets.go,
	// vwap.go, oracle.go, …).
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Loop the XLM aliases as handlePrice does; querying only the literal `native/fiat:USD` key
	// would miss a fresh `crypto:XLM/fiat:USD` VWAP from CEX trades.
	snapshot, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, reader, asset, defaultPriceQuote)
	// viaFallback mirrors handlePrice: normalizeRawPriceSnapshot below
	// must only run on the RAW closed-1m-bucket read, never on a
	// priceFallback result (already normalized upstream / peg / cross-
	// rate) — see normalizeRawPriceSnapshot's doc comment in price.go.
	viaFallback := false
	// triangulated rides with the value, as it does on /v1/price: a
	// composed fallback — the declared-peg XLM cross, a triangulated
	// chain, a fiat cross-rate — is not a print of the pair, and the
	// flag is the only place the envelope says so.
	triangulated := false
	// Substance-gated pair: withheld beats the fallback chain (see ErrPriceWithheld); only a thin verdict yields, and only to a
	// qualifying peg book ([Server.thinDirectPegAnswer]), the rule prices() applies.
	if errors.Is(err, ErrPriceWithheld) {
		fb, ok := s.writeLastPriceThinDirect(ctx, w, r, asset, err)
		if !ok {
			return
		}
		snapshot, sources, served, triangulated, stale, viaFallback, err = fb.snap, fb.sources, fb.served, fb.triangulated, fb.stale, true, nil
	}
	if errors.Is(err, ErrPriceNotFound) {
		// Same chain as /v1/price (priceFallback): Redis VWAP cache, stablecoin-fiat proxy, then
		// fiat cross-rate. prices_1m has no literal native/fiat:USD bucket, so without it
		// `lastprice(native)` would 404 in steady state.
		viaFallback = true
		fb := s.priceFallback(ctx, asset, defaultPriceQuote, err)
		snapshot, sources, served, triangulated = fb.snap, fb.sources, fb.served, fb.triangulated
		ok := fb.ok
		// A withheld verdict reached from the proxy leg must be
		// reported as withheld, not as "no price data" — the two are
		// different answers, and only the withheld problem names the raw
		// surfaces where the data IS available.
		if !ok && (fb.withheld != "" || fb.err != nil) {
			s.writeFallbackMiss(w, r, asset, defaultPriceQuote, fb, nil)
			return
		}
		// Every fallback degradation is below the surface's documented
		// baseline contract, so flags.stale MUST be true: the chain
		// itself is the staleness signal, as on /v1/price.
		stale = fb.stale
		if !ok || isDeclaredPeg(snapshot) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/price-not-found",
				"No price data for asset", http.StatusNotFound,
				"no observation for "+asset.String())
			return
		}
		err = nil
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.logger.Warn("LatestPrice (sep40 lastprice) deadline exceeded", "asset", asset.String())
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/oracle-lastprice-timeout",
				"Oracle lastprice query timed out", http.StatusServiceUnavailable,
				"the price read didn't return in 8s; retry shortly.")
			return
		}
		s.logger.Error("LatestPrice (sep40 lastprice) failed",
			"err", err, "asset", asset.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// A frozen pair serves the value the freeze is HOLDING, never the
	// refused bucket just read — see [Server.resolveFrozenServe].
	held := s.resolveFrozenServeFor(r, snapshot, asset, served, defaultPriceQuote)
	if held.outcome == frozenServeNothingHeld {
		writeFrozenNothingHeldProblem(w, r, asset, defaultPriceQuote, held)
		return
	}
	out, flags, sources := s.sep40Serve(ctx, asset, defaultPriceQuote, sep40Read{
		snapshot: snapshot, sources: sources, stale: stale, triangulated: triangulated, viaFallback: viaFallback,
	}, held)
	writeJSON(w, out, flags, sources...)
}

// parseOracleAsset reads a SEP-40 call's `asset` parameter, writing the 400 and returning ok=false when it is missing, invalid or
// the fiat:USD quote itself.
func parseOracleAsset(w http.ResponseWriter, r *http.Request, call string) (canonical.Asset, bool) {
	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required")
		return canonical.Asset{}, false
	}
	asset, err := parsePriceLeg(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, false
	}
	if asset.Equal(defaultPriceQuote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset is the SEP-40 quote", http.StatusBadRequest,
			"price of fiat:USD in itself is always 1; SEP-40 "+call+" quotes everything in fiat:USD")
		return canonical.Asset{}, false
	}
	return asset, true
}

// isDeclaredPeg reports a fallback answer that is the operator's peg declaration, not an observation; SEP40Price has no price_type, so
// point reads answer None (404).
func isDeclaredPeg(s PriceSnapshot) bool { return s.PriceType == "peg" }

// sep40Read is what a SEP-40 single-price handler read, before the
// freeze verdict is applied.
type sep40Read struct {
	snapshot     PriceSnapshot
	sources      []string
	stale        bool
	triangulated bool
	viaFallback  bool
}

// sep40Serve applies the already-resolved freeze verdict; a held value replaces the read wholesale (value, sources, triangulation) and is
// stale and single-sourced, as on /v1/price.
func (s *Server) sep40Serve(ctx context.Context, asset, quote canonical.Asset, rd sep40Read, held frozenResolution) (SEP40Price, Flags, []string) {
	if held.outcome == frozenServeHeld {
		rd = sep40Read{
			snapshot: held.snapshot, sources: held.sources, stale: true,
			triangulated: held.triangulated, viaFallback: true,
		}
	}
	// dex-nonstandard-decimals forward normalization on the raw
	// closed-1m-bucket read only — see handlePrice's equivalent call.
	if !rd.viaFallback {
		s.normalizeRawPriceSnapshot(&rd.snapshot, asset, quote)
	}
	out := SEP40Price{
		Asset:     asset.String(),
		Price:     rd.snapshot.Price,
		Timestamp: rd.snapshot.ObservedAt,
		Credit:    creditForSources(rd.sources),
	}
	frozen := held.outcome == frozenServeHeld
	flags := s.withProxyDeviation(ctx, Flags{
		Stale:         rd.stale,
		Triangulated:  rd.triangulated,
		Frozen:        frozen,
		Degraded:      frozen,
		FrozenChecked: held.checked,
		SingleSource:  frozen,
	}, quote, time.Now().UTC())
	return out, flags, rd.sources
}

// handleOraclePrices serves GET /v1/oracle/prices?asset=&records=N: SEP-40 `prices` passthrough, up to `records` (default 60, max 200)
// most recent CLOSED 1m VWAP snapshots, newest first (ADR-0015).
// 200 with an empty array if none; 400 for bad records or asset; 503 when no PriceReader is wired.
func (s *Server) handleOraclePrices(w http.ResponseWriter, r *http.Request) {
	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	asset, ok := parseOracleAsset(w, r, "prices()")
	if !ok {
		return
	}

	records := oraclePricesDefault
	if raw := r.URL.Query().Get("records"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > oraclePricesMax {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-records",
				"Invalid records parameter", http.StatusBadRequest,
				"records must be an integer in [1, 200]")
			return
		}
		records = n
	}

	// Same 8s ceiling as lastprice/x_last_price — see that
	// handler's comment.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	snapshots, served, viaPeg, directWithheld, err := s.recentClosedWithStablecoinFallback(ctx, asset, defaultPriceQuote, records)
	if err != nil {
		s.writeOraclePricesError(ctx, w, r, asset, records, err)
		return
	}

	held := s.resolveFrozenServe(r, asset, served, defaultPriceQuote).refusedBy(directWithheld)
	if writeFrozenOraclePrices(w, r, asset, held) {
		return
	}
	excluded := slices.ContainsFunc(snapshots, func(p PriceSnapshot) bool { return p.FillsWithheld })
	unread := slices.ContainsFunc(snapshots, func(p PriceSnapshot) bool { return p.FillsUnread })
	snapshots, dropped, windowsUnknown := s.dropFrozenBuckets(ctx, asset, snapshots)

	out := make([]SEP40Price, len(snapshots))
	for i, snap := range snapshots {
		// Snapshots are the raw prices_1m ratio, so apply the same decimals normalization as
		// /v1/price. The quote leg is fiat:USD or a 7dp USD peg, so defaultPriceQuote resolves it
		// exactly on both branches; no-op for an unflagged asset.
		s.normalizeRawPriceSnapshot(&snap, asset, defaultPriceQuote)
		out[i] = SEP40Price{
			Asset:     asset.String(),
			Price:     snap.Price,
			Timestamp: snap.ObservedAt,
		}
	}
	// A peg-proxied series is not a print of the fiat:USD pair and sits
	// below the surface's baseline: stale as well as triangulated, the
	// rule lastprice/x_last_price follow. A series short of the buckets a recovered freeze refused is a partial answer.
	writeJSON(w, out, markFillsWithheld(Flags{
		Stale: viaPeg || windowsUnknown, Triangulated: viaPeg, Degraded: dropped || windowsUnknown || unread,
		FrozenChecked: held.checked && !windowsUnknown,
	}, excluded))
}

// writeOraclePricesError answers a prices() series read that served nothing.
func (s *Server) writeOraclePricesError(
	ctx context.Context, w http.ResponseWriter, r *http.Request, asset canonical.Asset, records int, err error,
) {
	switch {
	case clientAborted(r, err):
	case errors.Is(err, ErrPriceWithheld):
		// Same 404 verdict as lastprice: a snapshot series is still an aggregated price claim per bucket.
		writePriceWithheldProblem(w, r, asset, defaultPriceQuote, priceWithheldReason(err))
	case errors.Is(err, errStablecoinNoOwnSeries):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-not-found",
			"No price data for asset", http.StatusNotFound,
			asset.String()+" is a USD stablecoin with no observed fiat:USD series; another stablecoin's book "+
				"never stands in for its price")
	case handlerTimedOut(ctx, err):
		s.logger.Warn("RecentClosedSnapshots (sep40 prices) deadline exceeded",
			"asset", asset.String(), "records", records)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/oracle-prices-timeout",
			"Oracle prices query timed out", http.StatusServiceUnavailable,
			"the price history read didn't return in 8s; retry shortly.")
	default:
		s.logger.Error("RecentClosedSnapshots (sep40 prices) failed",
			"err", err, "asset", asset.String(), "records", records)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
	}
}

// writeFrozenOraclePrices answers prices() for a pair frozen now and reports whether it wrote: it serves only the held value
// lastprice serves, or lastprice's 503 when nothing is held. Buckets a recovered freeze covered are dropped by [Server.dropFrozenBuckets].
func writeFrozenOraclePrices(w http.ResponseWriter, r *http.Request, asset canonical.Asset, held frozenResolution) bool {
	switch held.outcome {
	case frozenServeNothingHeld:
		writeFrozenNothingHeldProblem(w, r, asset, defaultPriceQuote, held)
		return true
	case frozenServeHeld:
		writeJSON(w, []SEP40Price{{
			Asset:     asset.String(),
			Price:     held.snapshot.Price,
			Timestamp: held.snapshot.ObservedAt,
			Credit:    creditForSources(held.sources),
		}}, Flags{
			Stale: true, Triangulated: held.triangulated, Frozen: true, Degraded: true,
			FrozenChecked: held.checked, SingleSource: true,
		})
		return true
	case frozenServeNotFrozen:
	}
	return false
}

// oracleLiveWindow: a series whose newest closed bucket is this recent is served without reading
// the remaining spellings and pegs.
const oracleLiveWindow = 10 * time.Minute

// recentClosedWithStablecoinFallback reads the X/fiat:USD series and, when that is empty or
// dormant, the X/<peg> series of every peg spelling, and serves whichever has the newest bucket
// (viaPeg=true marks the envelope stale and triangulated). A dormant book is non-empty forever, so
// "first non-empty" would serve months-old records beside a live series.
// With nothing to serve, a withheld verdict wins, then the first read error: neither is 200 [].
// served is the spelling of asset whose series was chosen (it governs the freeze check); directWithheld is the exclusion that
// withheld the asset's own quote series when a peg book answered in its place (it withdraws a frozen pair's hold).
func (s *Server) recentClosedWithStablecoinFallback(
	ctx context.Context, asset, quote canonical.Asset, n int,
) (snapshots []PriceSnapshot, served canonical.Asset, viaPeg bool, directWithheld, err error) {
	usdQuote := quote.Type == canonical.AssetFiat && quote.Code == "USD"
	best, bestAt, served, err := s.recentClosedForAliases(ctx, asset, quote, n)
	directErr := err
	var failed oracleLegErrors
	if err != nil {
		if refusal := directReadRefusal(usdQuote, err); refusal != nil {
			return nil, canonical.Asset{}, false, nil, refusal
		}
		failed.note(err)
		directWithheld = exclusionVerdict(err)
	}
	if !usdQuote || s.oracleSeriesLive(best, bestAt) {
		return best, served, false, nil, nil
	}
	// A USD stablecoin's book against another peg would hide a joint depeg (and its own peg is no market).
	if s.isUSDStablecoin(asset) {
		snapshots, served, err = stablecoinOwnSeries(best, served, failed)
		return snapshots, served, false, nil, err
	}
	for _, peg := range s.usdPegProxyQuotes() {
		pegSnapshots, pegAt, pegServed, ok := s.oraclePegSeries(ctx, asset, quote, peg, n, &failed)
		if failed.read != nil {
			// An unread leg is not a missing one: the next peg's market never stands in for it, but a leg read before it
			// still answers (walkUSDPegs' rule).
			break
		}
		if !ok {
			continue
		}
		if s.oracleSeriesSupersedes(best, bestAt, pegSnapshots, pegAt) {
			best, bestAt, served, viaPeg = pegSnapshots, pegAt, pegServed, true
		}
		if s.oracleSeriesLive(pegSnapshots, pegAt) {
			break
		}
	}
	if len(best) > 0 {
		if best, err = s.settledSeries(ctx, best, bestAt, failed, directErr); err != nil {
			return nil, canonical.Asset{}, false, nil, err
		}
		return best, served, viaPeg, directWithheld, nil
	}
	return nil, canonical.Asset{}, false, nil, cmp.Or(failed.read, failed.err())
}

// settledSeries is the chosen series as served: marked when an older one fills a withheld leg, and refused when it stands in for a
// direct series refused as thin without qualifying ([Server.thinDirectPegRefusal]).
func (s *Server) settledSeries(
	ctx context.Context, best []PriceSnapshot, bestAt time.Time, failed oracleLegErrors, directErr error,
) ([]PriceSnapshot, error) {
	if !s.oracleSeriesLive(best, bestAt) {
		best = seriesFillsWithheld(best, failed.excluded, failed.unread)
	}
	if err := s.thinDirectPegRefusal(ctx, directErr, best); err != nil {
		return nil, err
	}
	return best, nil
}

// directReadRefusal is the error prices() answers for a failed direct read, or nil when a peg book may answer in its place: a
// series withheld by a source exclusion is no direct series, and one refused as thin yields only to a qualifying peg book
// ([Server.thinDirectPegRefusal]). Any other refusal (a flagged issuer, an unreadable exclusion set) refuses the pair, as
// lastprice does.
func directReadRefusal(usdQuote bool, err error) error {
	if usdQuote && (withheldByExclusion(err) || thinOnly(err)) {
		return nil
	}
	return err
}

// stablecoinOwnSeries answers a USD stablecoin from its own series only: a withheld verdict outranks "no series".
func stablecoinOwnSeries(best []PriceSnapshot, served canonical.Asset, failed oracleLegErrors) ([]PriceSnapshot, canonical.Asset, error) {
	if len(best) > 0 {
		return best, served, nil
	}
	if err := failed.err(); err != nil {
		return nil, canonical.Asset{}, err
	}
	return nil, canonical.Asset{}, errStablecoinNoOwnSeries
}

// oraclePegSeries reads asset's series against one peg, judged under the requested pair; a failed or empty leg is noted.
func (s *Server) oraclePegSeries(
	ctx context.Context, asset, quote, peg canonical.Asset, n int, failed *oracleLegErrors,
) ([]PriceSnapshot, time.Time, canonical.Asset, bool) {
	snaps, _, served, err := s.recentClosedForAliases(ctx, asset, peg, n)
	if err == nil {
		snaps, err = s.pegSeriesClean(ctx, asset, quote, snaps)
	}
	if err != nil || len(snaps) == 0 {
		failed.note(err)
		return nil, time.Time{}, canonical.Asset{}, false
	}
	return snaps, newestSnapshotTime(snaps), served, true
}

// seriesFillsWithheld marks an older series served because a newer one was withheld: by an exclusion (the answer is not what an
// unexcluded market would give) or by an unreadable exclusion set (unknown). The reader's slice is copied, not mutated.
func seriesFillsWithheld(snaps []PriceSnapshot, excluded, unread bool) []PriceSnapshot {
	if !excluded && !unread {
		return snaps
	}
	out := slices.Clone(snaps)
	for i := range out {
		out[i].FillsWithheld, out[i].FillsUnread = out[i].FillsWithheld || excluded, out[i].FillsUnread || unread
	}
	return out
}

// errStablecoinNoOwnSeries: a USD stablecoin has no observed fiat:USD series, and no other stablecoin's book may stand in.
var errStablecoinNoOwnSeries = errors.New("usd stablecoin has no observed fiat:USD series")

// isUSDStablecoin: a declared USD peg under any spelling, or a `crypto:<STABLE>` ticker tracking USD.
func (s *Server) isUSDStablecoin(asset canonical.Asset) bool {
	if proxy, ok := aggregate.FiatProxy(asset); ok && proxy.Equal(defaultPriceQuote) {
		return true
	}
	return s.isDeclaredUSDPeg(asset)
}

// pegSeriesClean judges each bucket of a peg book's series, served as asset/quote, under that requested pair: the reader judged
// only the book, and an exclusion stored under the fiat pair names no book. Survivors are marked FillsWithheld when any bucket
// was dropped; a series the exclusion empties is withheld rather than read as a quiet market.
func (s *Server) pegSeriesClean(ctx context.Context, asset, quote canonical.Asset, snaps []PriceSnapshot) ([]PriceSnapshot, error) {
	kept := snaps[:0:0]
	for _, snap := range snaps {
		switch w := s.pegBucketWithheld(ctx, asset, quote, snap, snap.BucketSources); w {
		case pricingguard.NotWithheld:
			kept = append(kept, snap)
		case pricingguard.WithheldSourceExcluded:
		default:
			return nil, PriceWithheldError(w)
		}
	}
	if len(kept) == len(snaps) {
		return snaps, nil
	}
	if len(kept) == 0 {
		return nil, PriceWithheldError(pricingguard.WithheldSourceExcluded)
	}
	for i := range kept {
		kept[i].FillsWithheld = true
	}
	return kept, nil
}

// oracleLegErrors keeps what the peg walk's failed legs said: a withheld verdict outranks the
// first read error. excluded: some leg was withheld by a source exclusion; unread: by an unreadable exclusion set. These are the
// only refusals an older series fills.
type oracleLegErrors struct {
	withheld, first, read error
	excluded, unread      bool
}

func (e *oracleLegErrors) note(err error) {
	if err != nil && e.read == nil && isPriceReadFailure(err) {
		e.read = err
	}
	if errors.Is(err, ErrPriceWithheld) && e.withheld == nil {
		e.withheld = err
	}
	e.excluded = e.excluded || withheldByExclusion(err)
	e.unread = e.unread || withheldExclusionsUnread(err)
	if e.first == nil {
		e.first = err
	}
}

func withheldByExclusion(err error) bool {
	return errors.Is(err, ErrPriceWithheld) && priceWithheldReason(err) == PriceWithheldSourceExcluded
}

// exclusionVerdict is err when a source exclusion withheld it, else nil.
func exclusionVerdict(err error) error {
	if withheldByExclusion(err) {
		return err
	}
	return nil
}

func withheldExclusionsUnread(err error) bool {
	var e *withheldError
	return errors.As(err, &e) && e.unread
}

func (e oracleLegErrors) err() error {
	if e.withheld != nil {
		return e.withheld
	}
	return e.first
}

// oracleSeriesLive reports whether a series' newest bucket is inside oracleLiveWindow.
func (s *Server) oracleSeriesLive(snapshots []PriceSnapshot, newest time.Time) bool {
	return len(snapshots) > 0 && s.now().Sub(newest) <= oracleLiveWindow
}

// oracleSeriesSupersedes: a later leg replaces an earlier one only when it is live or newer by more
// than oracleLiveWindow. The walk order is depth order (direct, classic pegs, SAC forms), so a thin
// leg a minute newer must not displace it, nor the answer flip between requests.
func (s *Server) oracleSeriesSupersedes(best []PriceSnapshot, bestAt time.Time, cand []PriceSnapshot, candAt time.Time) bool {
	if len(best) == 0 {
		return true
	}
	return s.oracleSeriesLive(cand, candAt) || candAt.Sub(bestAt) > oracleLiveWindow
}

// recentClosedForAliases reads asset under each of its spellings (see [assetAliases]) against
// quote and returns the series with the newest bucket and that bucket's time; a live one ends the
// walk, plus the spelling that served it. An error is returned only when no spelling served.
func (s *Server) recentClosedForAliases(ctx context.Context, asset, quote canonical.Asset, n int) ([]PriceSnapshot, time.Time, canonical.Asset, error) {
	var (
		failed oracleLegErrors
		best   []PriceSnapshot
		bestAt time.Time
		served canonical.Asset
	)
	for _, a := range assetAliases(asset) {
		if failed.read != nil && a.Type == canonical.AssetSoroban {
			// The SAC spelling (walked after the established ones) is the thinner market: it does not stand in for an
			// established one that could not be read.
			continue
		}
		snapshots, err := s.Prices.RecentClosedSnapshots(ctx, a, quote, n)
		if err != nil {
			failed.note(err)
			continue
		}
		if len(snapshots) == 0 {
			continue
		}
		at := newestSnapshotTime(snapshots)
		if s.oracleSeriesSupersedes(best, bestAt, snapshots, at) {
			best, bestAt, served = snapshots, at, a
		}
		if s.oracleSeriesLive(snapshots, at) {
			break
		}
	}
	if len(best) > 0 {
		if !s.oracleSeriesLive(best, bestAt) {
			best = seriesFillsWithheld(best, failed.excluded, failed.unread)
		}
		return best, bestAt, served, nil
	}
	return nil, time.Time{}, canonical.Asset{}, failed.first
}

// newestSnapshotTime is the latest observed_at in a series, whatever its order.
func newestSnapshotTime(snapshots []PriceSnapshot) time.Time {
	var newest time.Time
	for i := range snapshots {
		if t := time.Time(snapshots[i].ObservedAt); t.After(newest) {
			newest = t
		}
	}
	return newest
}

// oraclePricesDefault + Max mirror the OpenAPI bounds for the
// `records` parameter on /v1/oracle/prices. Documented inline in
// the spec; pinned here so the handler validates against the same
// numbers the spec promises.
const (
	oraclePricesDefault = 60
	oraclePricesMax     = 200
)

// handleOracleXLastPrice serves GET /v1/oracle/x_last_price?base=&quote=: SEP-40 passthrough; `asset` carries the canonical base id so
// clients reuse lastprice parsing.
// 404 when no observation exists for the pair; freeze handling as handleOracleLastPrice.
func (s *Server) handleOracleXLastPrice(w http.ResponseWriter, r *http.Request) {
	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	base, quote, ok := s.parseXLastPriceBaseQuote(w, r)
	if !ok {
		return
	}

	// Same 8s ceiling as handleOracleLastPrice — see that
	// handler's comment.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Both legs walk their XLM/SAC spellings, as handlePrice does, so `x_last_price(native, fiat:USD)` finds the
	// crypto:XLM book and quote=crypto:XLM or the XLM SAC reads the book stored under native.
	// Each spelling runs the closed read then the fallback chain (Redis VWAP cache, stablecoin-fiat proxy, fiat cross-rate), the
	// walk /v1/price takes, so both closed surfaces serve one number; the thin second pass is /v1/price's opt-in only.
	ps, _, market := walkQuoteAliases(base, quote, func(q canonical.Asset) (priceServe, *ThinAdmission) {
		return s.resolvePriceServe(ctx, reader, base, q), nil
	})
	// Substance-gated pair: withheld beats the fallback chain — same rationale as handleOracleLastPrice above.
	if errors.Is(ps.err, ErrPriceWithheld) {
		writePriceWithheldProblem(w, r, base, quote, priceWithheldReason(ps.err))
		return
	}
	// A withheld proxy-leg verdict is reported as withheld, as above.
	if ps.miss != nil && (ps.miss.withheld != "" || ps.miss.err != nil) {
		s.writeFallbackMiss(w, r, base, quote, *ps.miss, nil)
		return
	}
	// viaFallback: normalizeRawPriceSnapshot runs only on the raw closed-bucket read, never on a priceFallback result.
	snapshot, sources, stale, served, triangulated, viaFallback, err := ps.snap, ps.sources, ps.stale, ps.served, ps.triangulated, ps.viaFallback, ps.err
	if ps.miss != nil || (viaFallback && isDeclaredPeg(snapshot)) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-not-found",
			"No price data for pair", http.StatusNotFound,
			"no observation for "+base.String()+" / "+quote.String())
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.logger.Warn("LatestPrice (sep40 x_last_price) deadline exceeded",
				"base", base.String(), "quote", quote.String())
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/oracle-xlastprice-timeout",
				"Oracle x_last_price query timed out", http.StatusServiceUnavailable,
				"the price read didn't return in 8s; retry shortly.")
			return
		}
		s.logger.Error("LatestPrice (sep40 x_last_price) failed",
			"err", err, "base", base.String(), "quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// The ADR-0053 basis rule, as on /v1/price, so both closed surfaces
	// serve one number for the pair.
	if !viaFallback {
		if basis, ok := s.preferUSDAnchoredBasis(ctx, base, market, snapshot, sources); ok {
			snapshot, sources, served, triangulated, stale = basis.snap, basis.sources, basis.served, true, basis.stale
			viaFallback = true
		}
	}

	// Freeze, then the shared tail — see handleOracleLastPrice.
	held := s.resolveFrozenServeFor(r, snapshot, base, served, market)
	if held.outcome == frozenServeNothingHeld {
		writeFrozenNothingHeldProblem(w, r, base, quote, held)
		return
	}
	out, flags, sources := s.sep40Serve(ctx, base, quote, sep40Read{
		snapshot: snapshot, sources: sources, stale: stale, triangulated: triangulated, viaFallback: viaFallback,
	}, held)
	writeJSON(w, out, flags, sources...)
}

// parseXLastPriceBaseQuote validates the x_last_price base/quote pair; ok=false after writing a problem response.
func (s *Server) parseXLastPriceBaseQuote(w http.ResponseWriter, r *http.Request) (base, quote canonical.Asset, ok bool) {
	rawBase := r.URL.Query().Get("base")
	if rawBase == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-base",
			"Missing base parameter", http.StatusBadRequest,
			"base query parameter is required")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	base, err := parsePriceLeg(rawBase)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid base identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}

	rawQuote := r.URL.Query().Get("quote")
	if rawQuote == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-quote",
			"Missing quote parameter", http.StatusBadRequest,
			"quote query parameter is required")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	quote, err = parsePriceLeg(rawQuote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-quote",
			"Invalid quote identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}
	if base.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-pair",
			"Base and quote are the same", http.StatusBadRequest,
			"price of an asset in itself is always 1; base and quote must differ")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	return base, quote, true
}
