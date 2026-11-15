package v1

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// priceBatchConcurrency bounds the per-request fan-out in lookupPriceBatch (each id is a LatestPrice + fallback-chain round-trip; serial
// put p99 at the 10s ceiling).
// MEASURED; do not lower it as a throughput control: narrowing only lengthens connection hold times. The real bound on a 1000-id POST is
// the rate limiter.
const priceBatchConcurrency = 16

// batchRowResult is the per-id outcome computed by resolveBatchRow.
// Exactly one of {ok, skip, fail} characterises a result.
type batchRowResult struct {
	snap         PriceSnapshot
	sources      []string
	stale        bool
	frozen       bool
	triangulated bool
	asset        canonical.Asset

	ok   bool // a real price row — include in the envelope
	skip bool // per-asset miss — omit, do NOT 404 the batch
	// withheld qualifies a skip: a gate declined to publish a price the server holds, so the id
	// goes on the envelope's `withheld` list rather than reading as "no data".
	withheld bool
	// withheldReason keys the include_thin second pass.
	withheldReason PriceWithheldReason
	// thin: served under include_thin from a market below the substance floor; no change_24h_pct.
	thin bool
	// readFailed qualifies a skip: dropped because a read failed, not because there is no price.
	readFailed bool
	// unstarted qualifies a readFailed skip: the request's deadline passed before the row began.
	unstarted bool
	// freezeUnknown: a wired freeze marker did not answer, so the row is served with no verdict and derives no single_source.
	freezeUnknown bool
	fail          *batchRowFailure
}

// batchRowFailure aborts the whole batch with a problem+json. It is a value because resolveBatchRow runs in a worker goroutine,
// where touching the shared http.ResponseWriter would race; the orchestrator writes the first failure.
type batchRowFailure struct {
	status int
	typ    string
	title  string
	detail string
}

// resolveBatchRowShared coalesces [Server.resolveBatchRow] across concurrent requests for the same (id, quote, include_thin):
// sharded explorer builds ask for the same cold 100-id chunks at once, and each duplicate re-ran the gates and fallback chain.
// The shared work runs on its own priceReadFlightTimeout budget, never the leader's deadline or cancellation: a leader that had
// overrun would otherwise fail the freeze read and hand every follower the bucket the freeze refused.
func (s *Server) resolveBatchRowShared(ctx context.Context, r *http.Request, raw string, quote canonical.Asset) batchRowResult {
	key := raw + "/" + quote.String() + "/thin:" + strconv.FormatBool(includeThinRequested(r))
	v, _, _ := s.batchRowFlight.Do(key, func() (any, error) { //nolint:contextcheck // deliberately detached from the leader's cancellation, see above
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), priceReadFlightTimeout)
		defer cancel()
		return s.resolveBatchRow(fctx, r.WithContext(fctx), raw, quote), nil
	})
	row, _ := v.(batchRowResult)
	return row
}

// resolveBatchRow resolves one (raw_id, quote) pair, goroutine-safe (never touches w); returns ok (a row), skip (per-asset miss, omitted)
// or fail (aborts the batch).
func (s *Server) resolveBatchRow(ctx context.Context, r *http.Request, raw string, quote canonical.Asset) batchRowResult {
	asset, err := parsePriceLeg(raw)
	if err != nil {
		return batchRowResult{fail: &batchRowFailure{
			status: http.StatusBadRequest,
			typ:    "https://api.stellarindex.io/errors/invalid-asset-id",
			title:  "Invalid asset identifier",
			detail: raw + ": " + err.Error(),
		}}
	}
	if asset.Equal(quote) {
		return batchRowResult{fail: &batchRowFailure{
			status: http.StatusBadRequest,
			typ:    "https://api.stellarindex.io/errors/identity-price",
			title:  "Asset and quote are the same",
			detail: "price of an asset in itself is always 1; " + raw + " matches the quote",
		}}
	}
	offCtx, off := WithThinAdmission(ctx, asset, quote, false)
	row := s.readBatchRow(offCtx, r, asset, quote)
	if row.withheld && includeThinRequested(r) && off.ThinWithheld(row.withheldReason) {
		onCtx, on := WithThinAdmission(ctx, asset, quote, true)
		thin := s.readBatchRow(onCtx, r, asset, quote)
		if thin.ok && thin.snap.Price != "" {
			thin.thin = on.Admitted()
			row = thin
		}
	}
	// No derived change from a thin price (ADR-0018); computed on the final (possibly held) snapshot.
	if row.ok && !row.thin {
		row.snap.Change24hPct = s.batchChange24h(ctx, asset, quote, row.snap.Price)
	}
	return row
}

// readBatchRow is one pass of a batch row's read under ctx's [ThinAdmission].
func (s *Server) readBatchRow(ctx context.Context, r *http.Request, asset, quote canonical.Asset) batchRowResult {
	// Alias loop, as handlePrice: the literal `native` alone would return stale/empty while
	// /v1/price serves the fresh VWAP published under crypto:XLM.
	snap, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, s.Prices, asset, quote)
	if errors.Is(err, ErrPriceWithheld) {
		// Omit and name it on `withheld`; do NOT run priceFallback, which would re-serve the
		// withheld market via the Redis/proxy side doors.
		return batchRowResult{skip: true, withheld: true, withheldReason: priceWithheldReason(err)}
	}
	if errors.Is(err, ErrPriceNotFound) {
		// Share /v1/price's fallback chain: skipping the stablecoin proxy would drop ids that return
		// 200 there. A withheld verdict from any leg goes on `withheld`, never into "no data".
		fb := s.priceFallback(ctx, asset, quote, err)
		fs, fsrc, fserved, ftri := fb.snap, fb.sources, fb.served, fb.triangulated
		if fb.ok {
			// Fallbacks sit below the closed-bucket contract, so stale; triangulated carries through
			// for parity with /v1/price. fserved is the freeze check's governing pair.
			return s.holdFrozenBatchRow(r, batchRowResult{
				snap: fs, sources: fsrc, stale: fb.stale, triangulated: ftri,
				asset: asset, ok: true,
			}, fserved, quote)
		}
		readFailed := fb.err != nil && !clientAborted(r, fb.err)
		if readFailed {
			s.logger.Warn("batch: price fallback read failed", "err", fb.err, "asset", asset.String())
		}
		// A failed read omits the row without listing it as withheld.
		return batchRowResult{skip: true, withheld: fb.withheld != "", withheldReason: fb.withheld, readFailed: readFailed} // omit, do not 404 the batch
	}
	if err != nil {
		if clientAborted(r, err) {
			// Client gone: the orchestrator sees the cancelled context after join and writes nothing.
			return batchRowResult{skip: true}
		}
		s.logger.Error("LatestPrice (batch) failed",
			"err", err, "asset", asset.String(), "quote", quote.String())
		// One id's failed read is that row's: omitted and named on `unread`, as a failed fallback read is.
		return batchRowResult{skip: true, readFailed: true}
	}
	if basis, ok := s.preferUSDAnchoredBasis(ctx, asset, quote, snap, sources); ok {
		basis.snap.Change24hPct = s.batchChange24h(ctx, asset, quote, basis.snap.Price)
		return s.holdFrozenBatchRow(r, batchRowResult{
			snap: basis.snap, sources: basis.sources, stale: basis.stale, triangulated: true,
			asset: asset, ok: true,
		}, basis.served, quote)
	}
	// Raw closed-1m read: normalize here; the priceFallback branch above is already normalized.
	s.normalizeRawPriceSnapshot(&snap, asset, quote)
	// A frozen row carries the held value, not the raw bucket just read.
	return s.holdFrozenBatchRow(r, batchRowResult{
		snap: snap, sources: sources, stale: stale, asset: asset, ok: true,
	}, served, quote)
}

// batchChange24h computes the trailing-24h percentage change for a USD-quoted batch row (anchor: the USD price 24h ago, as
// /v1/assets/{id}); nil on any failure, by design, so a missing change never costs the caller the price.
func (s *Server) batchChange24h(ctx context.Context, asset, quote canonical.Asset, price string) *string {
	if s.Change24h == nil || !quote.Equal(defaultPriceQuote) {
		return nil
	}
	then, err := s.Change24h.USDPrice24hAgo(ctx, asset)
	if err != nil {
		return nil
	}
	pct, err := pctChange(price, then)
	if err != nil {
		return nil
	}
	return &pct
}

// lookupPriceBatch resolves every id concurrently (bounded by priceBatchConcurrency) into an index-keyed slice, so output keeps first-
// occurrence order.
// Misses are omitted; the first validation/internal failure in input order aborts the batch with a problem+json.
func (s *Server) lookupPriceBatch(w http.ResponseWriter, r *http.Request, ids []string, quote canonical.Asset) {
	ctx := r.Context()
	results := make([]batchRowResult, len(ids))

	// Each worker writes its own results[i] slot (no lock); the semaphore caps concurrent round-trips.
	sem := make(chan struct{}, priceBatchConcurrency)
	var wg sync.WaitGroup
	for i, raw := range ids {
		wg.Add(1)
		go func(i int, raw string) {
			// Registered first so it unwinds LAST: wg.Done must run or wg.Wait() deadlocks.
			defer wg.Done()
			// middleware.Recoverer doesn't cover this goroutine. On panic results[i] stays ZERO, so
			// the id is DROPPED rather than served with a fabricated price.
			defer worker.Recover(s.logger, "api-price-batch-row")
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				// The request is over: starting a row now would only open a flight for others to join.
				results[i] = batchRowResult{skip: true, readFailed: true, unstarted: true}
				return
			}
			results[i] = s.resolveBatchRowShared(ctx, r, raw, quote)
		}(i, raw)
	}
	wg.Wait()

	// Client disconnected mid-batch: nothing to write. A server-side deadline
	// (middleware.RequestTimeout) is NOT a client abort and must fall through to the fail-scan, so
	// writeProblem can answer a retryable 503 instead of net/http's bodyless 200 (see clientAborted).
	if clientAborted(r, nil) {
		return
	}
	// First failure in INPUT order aborts: deterministic whatever the completion order.
	for i := range results {
		if f := results[i].fail; f != nil {
			writeProblem(w, r, f.typ, f.title, f.status, f.detail)
			return
		}
	}

	if batchUnanswered(results) {
		// Nothing was read, or the deadline cut rows off: a 200 would say those ids have no price.
		w.Header().Set("Retry-After", retryAfterRequestTimeout)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price unavailable", http.StatusServiceUnavailable,
			"a price source could not be read; retry")
		return
	}

	env := batchEnvelope(ids, results)
	env.Flags = s.withProxyDeviation(r.Context(), env.Flags, time.Now().UTC())
	env.Flags.Degraded = env.Flags.Degraded || batchDegraded(results)
	writeEnvelope(w, env)
}

// batchDegraded reports a row dropped by a failed read or recovered panic, not a genuine miss, or served with no freeze verdict.
func batchDegraded(results []batchRowResult) bool {
	for i := range results {
		r := &results[i]
		if r.readFailed || r.freezeUnknown || (!r.ok && !r.skip && r.fail == nil) {
			return true
		}
	}
	return false
}

// batchUnanswered reports a batch that must not be answered 200: a row the deadline never let start, or no row with a
// known answer (a price, a miss or a withheld verdict) beside the ones that were lost.
func batchUnanswered(results []batchRowResult) bool {
	answered, lost := false, false
	for i := range results {
		r := &results[i]
		if r.unstarted {
			return true
		}
		// Lost: a failed read, or a row a recovered panic left zero.
		if r.readFailed || (!r.ok && !r.skip && r.fail == nil) {
			lost = true
		} else {
			answered = true
		}
	}
	return lost && !answered
}

// batchEnvelope folds the per-id results (index-aligned with ids) into the envelope; each flag is
// the OR over served rows.
func batchEnvelope(ids []string, results []batchRowResult) Envelope {
	out := make([]PriceSnapshot, 0, len(ids))
	var withheld, unread, thin, stale, triangulated []string
	allSources := map[string]struct{}{}
	anyFrozen := false
	anySingleSource := false
	for i := range results {
		row := results[i]
		if !row.ok {
			switch {
			case row.withheld:
				withheld = append(withheld, ids[i])
			case row.readFailed || !row.skip:
				// A failed read, or a row lost to a recovered panic: unknown, not "no price".
				unread = append(unread, ids[i])
			}
			continue // skip — per-asset miss
		}
		if row.stale {
			stale = append(stale, ids[i])
		}
		if row.triangulated {
			triangulated = append(triangulated, ids[i])
		}
		if row.thin {
			thin = append(thin, ids[i])
		}
		for _, src := range row.sources {
			allSources[src] = struct{}{}
		}
		if row.frozen {
			anyFrozen = true
			anySingleSource = true // freeze implies single-source
		} else if !row.freezeUnknown && marketSingleSource(row.snap, row.sources) {
			anySingleSource = true
		}
		out = append(out, row.snap)
	}
	srcs := make([]string, 0, len(allSources))
	for src := range allSources {
		srcs = append(srcs, src)
	}
	// Sort for ADR-0015 byte-identical cross-region output: map iteration order would break it.
	sort.Strings(srcs)
	return Envelope{
		Data:         out,
		Sources:      srcs,
		Withheld:     withheld,
		Unread:       unread,
		Thin:         thin,
		Stale:        stale,
		Triangulated: triangulated,
		Flags: Flags{
			ThinMarket:   len(thin) > 0,
			Stale:        len(stale) > 0,
			Frozen:       anyFrozen,
			Degraded:     anyFrozen,
			SingleSource: anySingleSource,
			Triangulated: len(triangulated) > 0,
		},
	}
}
