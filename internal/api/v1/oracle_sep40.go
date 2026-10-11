package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SEP40Price is the wire shape for the SEP-40 passthrough
// endpoints (/v1/oracle/lastprice, /v1/oracle/x_last_price).
//
// Field set is deliberately minimal — SEP-40 oracle contracts
// expose `(price, timestamp)` only. Adding source/confidence/
// price_type would let in a richer view via the SEP-40 surface,
// but those fields are already on /v1/oracle/latest and
// /v1/price; mixing them in here would break the "this surface
// matches what an on-chain SEP-40 oracle returns" contract that
// integrators rely on.
type SEP40Price struct {
	Asset     string   `json:"asset"`
	Price     string   `json:"price"`
	Timestamp WireTime `json:"timestamp"`
}

// handleOracleLastPrice serves GET /v1/oracle/lastprice?asset=<id>.
//
// SEP-40 `lastprice(asset) -> Option<PriceData>` passthrough.
// The on-chain oracle contract's native quote is fixed by the
// contract; our API mirrors that semantic by quoting in
// fiat:USD always — clients wanting a different quote should
// hit /v1/price?asset=&quote= or /v1/oracle/x_last_price.
//
// 404 when no price observation exists for the asset. A frozen pair
// (ADR-0019) serves the held last-known-good with flags.frozen, or 503
// when none is held — never the refused bucket.
func (s *Server) handleOracleLastPrice(w http.ResponseWriter, r *http.Request) {
	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required")
		return
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return
	}
	if asset.Equal(defaultPriceQuote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset is the SEP-40 quote", http.StatusBadRequest,
			"price of fiat:USD in itself is always 1; SEP-40 lastprice quotes everything in fiat:USD")
		return
	}

	// 8s ceiling on the price read, matching every other
	// hypertable/Redis-backed handler on this surface (markets.go,
	// vwap.go, oracle.go, …).
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Route the primary read through the XLM dual-form alias loop,
	// exactly as handlePrice does. Without it, SEP-40
	// `lastprice(native)` queries only the literal `native/fiat:USD`
	// key and misses a fresh `crypto:XLM/fiat:USD` VWAP that CEX
	// trades populate — returning stale/empty here while /v1/price
	// serves fresh. See readPriceWithAliases for the full rationale.
	snapshot, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, reader, asset, defaultPriceQuote)
	// Substance-gated pair: withheld beats the fallback chain — same
	// rationale as handlePrice (see ErrPriceWithheld). The SEP-40
	// surface is the LAST place a substanceless price belongs: its
	// consumers are oracle integrators.
	if errors.Is(err, ErrPriceWithheld) {
		writePriceWithheldProblem(w, r, asset, defaultPriceQuote, priceWithheldReason(err))
		return
	}
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
	if errors.Is(err, ErrPriceNotFound) {
		// Same fallback chain as /v1/price (priceFallback): Redis VWAP
		// cache (stablecoin-proxy rewrites + triangulated chains) →
		// read-time stablecoin-fiat proxy → fiat-vs-fiat cross-rate.
		// Without this, SEP-40 `lastprice(native)` 404s in steady
		// state because prices_1m has no literal native/fiat:USD
		// bucket, while /v1/price?asset=native&quote=fiat:USD succeeds
		// via the same fallback.
		viaFallback = true
		fb := s.priceFallback(ctx, asset, defaultPriceQuote)
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
		writeFrozenNothingHeldProblem(w, r, asset, defaultPriceQuote)
		return
	}
	out, flags, sources := s.sep40Serve(ctx, asset, defaultPriceQuote, sep40Read{
		snapshot: snapshot, sources: sources, stale: stale, triangulated: triangulated, viaFallback: viaFallback,
	}, held)
	writeJSON(w, out, flags, sources...)
}

// isDeclaredPeg reports a fallback answer that is the operator's peg
// declaration rather than an observation. SEP40Price carries no
// price_type to mark it, so the point reads answer SEP-40's None (404).
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

// sep40Serve applies the freeze verdict the handler already resolved and
// builds the response. A held value replaces the read wholesale — value,
// sources, triangulation — and is stale and single-sourced by
// construction, as on /v1/price.
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
	}
	frozen := held.outcome == frozenServeHeld
	flags := Flags{
		Stale:          rd.stale,
		Triangulated:   rd.triangulated,
		ProxyDeviation: rd.triangulated && s.proxyDeviation(ctx, time.Now().UTC()),
		Frozen:         frozen,
		Degraded:       frozen,
		FrozenChecked:  held.checked,
		SingleSource:   frozen,
	}
	return out, flags, rd.sources
}

// handleOraclePrices serves GET /v1/oracle/prices?asset=<id>&records=N.
//
// SEP-40 `prices(asset, records) -> Option<Vec<PriceData>>`
// passthrough. Returns up to `records` most-recent CLOSED 1-minute
// VWAP snapshots for the asset/USD pair (newest first), per the
// SEP-40 spec semantic that prices() is "the last N price records."
//
// Per ADR-0015 only closed buckets are returned; the in-progress
// bucket is excluded.
//
// Defaults + caps from the OpenAPI: records default 60, max 200.
//
// 200 with empty array when the asset has no closed buckets yet.
// 400 when records is out of range or asset is malformed.
// 503 when no PriceReader is wired.
func (s *Server) handleOraclePrices(w http.ResponseWriter, r *http.Request) {
	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required")
		return
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return
	}
	if asset.Equal(defaultPriceQuote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset is the SEP-40 quote", http.StatusBadRequest,
			"price of fiat:USD in itself is always 1; SEP-40 prices() quotes everything in fiat:USD")
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

	snapshots, viaPeg, err := s.recentClosedWithStablecoinFallback(ctx, asset, defaultPriceQuote, records)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		// Substance-gated pair: same 404 verdict as lastprice — a
		// historical snapshot series is still an aggregated price
		// claim per bucket.
		if errors.Is(err, ErrPriceWithheld) {
			writePriceWithheldProblem(w, r, asset, defaultPriceQuote, priceWithheldReason(err))
			return
		}
		if handlerTimedOut(ctx, err) {
			s.logger.Warn("RecentClosedSnapshots (sep40 prices) deadline exceeded",
				"asset", asset.String(), "records", records)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/oracle-prices-timeout",
				"Oracle prices query timed out", http.StatusServiceUnavailable,
				"the price history read didn't return in 8s; retry shortly.")
			return
		}
		s.logger.Error("RecentClosedSnapshots (sep40 prices) failed",
			"err", err, "asset", asset.String(), "records", records)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := make([]SEP40Price, len(snapshots))
	for i, snap := range snapshots {
		// dex-nonstandard-decimals forward normalization — the snapshots
		// come from RecentClosedSnapshots, the same raw prices_1m CAGG
		// ratio /v1/price's closed-1m-bucket path reads (see
		// normalizeRawPriceSnapshot in price.go). The quote leg is always
		// fiat:USD or a 7dp classic USD peg here, so resolving against
		// defaultPriceQuote is exact for both the direct and the
		// peg-fallback branch. No-op (byte-identical) for an unflagged
		// asset.
		s.normalizeRawPriceSnapshot(&snap, asset, defaultPriceQuote)
		out[i] = SEP40Price{
			Asset:     asset.String(),
			Price:     snap.Price,
			Timestamp: snap.ObservedAt,
		}
	}
	// A peg-proxied series is not a print of the fiat:USD pair and sits
	// below the surface's baseline: stale as well as triangulated, the
	// rule lastprice/x_last_price follow.
	writeJSON(w, out, Flags{Stale: viaPeg, Triangulated: viaPeg})
}

// recentClosedWithStablecoinFallback wraps PriceReader.RecentClosedSnapshots
// with the same X/fiat:USD -> X/<peg> retry shape used in the other
// handler-side stablecoin-proxy fallbacks. When the literal asset/fiat:USD
// lookup returns an empty slice AND quote is fiat:USD AND the operator declared
// classic USD pegs, walks the pegs and returns the first non-empty
// asset/<peg> result. viaPeg=true on the return so the envelope can
// stamp the peg-served series stale and triangulated.
//
// A withheld peg leg is not a miss: the asset HAS a price there that
// policy declines to publish. A later peg may still serve (as in
// [Server.walkUSDPegs]); if none does, the first withheld verdict is
// returned so the handler answers withheld rather than 200 [].
//
// Both reads go through [Server.recentClosedForAliases], the XLM dual-form
// alias loop, so `/v1/oracle/prices?asset=native` finds a `crypto:XLM/fiat:USD`
// bucket written under the alias spelling.
func (s *Server) recentClosedWithStablecoinFallback(
	ctx context.Context, asset, quote canonical.Asset, n int,
) (snapshots []PriceSnapshot, viaPeg bool, err error) {
	snapshots, err = s.recentClosedForAliases(ctx, asset, quote, n)
	if err != nil {
		return nil, false, err
	}
	if len(snapshots) > 0 {
		return snapshots, false, nil
	}
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return snapshots, false, nil
	}
	var withheldErr error
	for _, peg := range s.USDPeggedClassics {
		// A peg asked for under any of its spellings — the classic id or
		// its SAC wrapper — is not a market against itself.
		if sameAsset(peg, asset) {
			continue
		}
		pegSnapshots, pegErr := s.recentClosedForAliases(ctx, asset, peg, n)
		if errors.Is(pegErr, ErrPriceWithheld) && withheldErr == nil {
			withheldErr = pegErr
		}
		if pegErr != nil || len(pegSnapshots) == 0 {
			continue
		}
		return pegSnapshots, true, nil
	}
	if withheldErr != nil {
		return nil, false, withheldErr
	}
	return snapshots, false, nil
}

// recentClosedForAliases tries each XLM dual-form alias of asset (see
// [assetAliases]) against quote, in priority order, and returns the
// first non-empty result. Mirrors [Server.readPriceWithAliasesServed]'s
// alias loop so /v1/oracle/prices doesn't miss a native-vs-crypto:XLM
// split of the same market.
func (s *Server) recentClosedForAliases(ctx context.Context, asset, quote canonical.Asset, n int) ([]PriceSnapshot, error) {
	var firstErr error
	for _, a := range assetAliases(asset) {
		snapshots, err := s.Prices.RecentClosedSnapshots(ctx, a, quote, n)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(snapshots) > 0 {
			return snapshots, nil
		}
	}
	return nil, firstErr
}

// oraclePricesDefault + Max mirror the OpenAPI bounds for the
// `records` parameter on /v1/oracle/prices. Documented inline in
// the spec; pinned here so the handler validates against the same
// numbers the spec promises.
const (
	oraclePricesDefault = 60
	oraclePricesMax     = 200
)

// handleOracleXLastPrice serves
// GET /v1/oracle/x_last_price?base=<id>&quote=<id>.
//
// SEP-40 `x_last_price(base, quote)` passthrough — returns the
// last observed price of `base` in terms of `quote`. The
// `asset` field in the response carries the canonical base
// identifier so existing SEP-40 clients can reuse their
// lastprice parsing path; the implicit quote is whatever was
// passed in the request.
//
// 404 when no observation exists for the pair. Freeze handling as
// handleOracleLastPrice.
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

	// Route the primary read through the XLM dual-form alias loop,
	// exactly as handlePrice does — so `x_last_price(native,
	// fiat:USD)` resolves a fresh `crypto:XLM/fiat:USD` VWAP that CEX
	// trades populate rather than missing it on the literal form.
	snapshot, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, reader, base, quote)
	// Substance-gated pair: withheld beats the fallback chain — same
	// rationale as handleOracleLastPrice above.
	if errors.Is(err, ErrPriceWithheld) {
		writePriceWithheldProblem(w, r, base, quote, priceWithheldReason(err))
		return
	}
	// viaFallback mirrors handlePrice / handleOracleLastPrice: the
	// normalizeRawPriceSnapshot below must run ONLY on the RAW closed-1m-bucket
	// read, never on a priceFallback result (already normalized at its own
	// source) — see normalizeRawPriceSnapshot's doc comment.
	viaFallback := false
	// triangulated rides with the value — see handleOracleLastPrice.
	triangulated := false
	if errors.Is(err, ErrPriceNotFound) {
		// Same fallback chain as /v1/price (priceFallback): Redis VWAP
		// cache → read-time stablecoin-fiat proxy → fiat-vs-fiat
		// cross-rate, for the reason /v1/oracle/lastprice gives — see
		// that handler's comment.
		viaFallback = true
		fb := s.priceFallback(ctx, base, quote)
		snapshot, sources, served, triangulated = fb.snap, fb.sources, fb.served, fb.triangulated
		ok := fb.ok
		// A withheld proxy-leg verdict is reported as withheld, as above.
		if !ok && (fb.withheld != "" || fb.err != nil) {
			s.writeFallbackMiss(w, r, base, quote, fb, nil)
			return
		}
		// Fallback responses surface flags.stale=true: the chain itself
		// is the staleness signal.
		stale = fb.stale
		if !ok || isDeclaredPeg(snapshot) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/price-not-found",
				"No price data for pair", http.StatusNotFound,
				"no observation for "+base.String()+" / "+quote.String())
			return
		}
		err = nil
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
		if basis, ok := s.preferUSDAnchoredBasis(ctx, base, quote, snapshot, sources); ok {
			snapshot, sources, served, triangulated, stale = basis.snap, basis.sources, basis.served, true, basis.stale
			viaFallback = true
		}
	}

	// Freeze, then the shared tail — see handleOracleLastPrice.
	held := s.resolveFrozenServeFor(r, snapshot, base, served, quote)
	if held.outcome == frozenServeNothingHeld {
		writeFrozenNothingHeldProblem(w, r, base, quote)
		return
	}
	out, flags, sources := s.sep40Serve(ctx, base, quote, sep40Read{
		snapshot: snapshot, sources: sources, stale: stale, triangulated: triangulated, viaFallback: viaFallback,
	}, held)
	writeJSON(w, out, flags, sources...)
}

// parseXLastPriceBaseQuote extracts + validates the base/quote pair for
// x_last_price (funlen split of handleOracleXLastPrice). Returns
// ok=false after writing a problem response.
func (s *Server) parseXLastPriceBaseQuote(w http.ResponseWriter, r *http.Request) (base, quote canonical.Asset, ok bool) {
	rawBase := r.URL.Query().Get("base")
	if rawBase == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-base",
			"Missing base parameter", http.StatusBadRequest,
			"base query parameter is required")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	base, err := canonical.ParseAsset(rawBase)
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
	quote, err = canonical.ParseAsset(rawQuote)
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
