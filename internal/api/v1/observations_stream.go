package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// Observations-stream tunables. interval_seconds is the per-connection
// tick cadence — independent of the tip surface's window_seconds (the
// raw-observations surface doesn't aggregate, so there's no window to
// pin).
const (
	defaultObservationsIntervalSeconds = 5
	minObservationsIntervalSeconds     = 1
	maxObservationsIntervalSeconds     = 60

	// observationsStreamProducerQueueDepth is the per-connection
	// channel capacity between the producer goroutine and the SSE
	// writer. 4 is enough for the writer to fall a tick behind without
	// the producer blocking.
	observationsStreamProducerQueueDepth = 4
)

// handleObservationsStream serves GET /v1/observations/stream, the SSE counterpart to
// /v1/observations (ADR-0018).
//
// Wire shape per connection:
//
//   - Initial event: the current per-source observations (`[]` when the pair has no trades yet).
//   - Recurring events: every interval_seconds (default 5, clamp 1-60) a fresh
//     LatestTradePerSource scan runs, bypassing the SWR history cache because each event is
//     stamped as_of=now, and an `observations_update` fires UNCONDITIONALLY (no dedupe).
//   - Heartbeats: every streaming.DefaultHeartbeatInterval when no real event has flowed.
//
// ?granularity= and ?window_seconds= return 400, as on /v1/observations; `interval_seconds` is
// named differently because the raw surface has no aggregation window.
func (s *Server) handleObservationsStream(w http.ResponseWriter, r *http.Request) {
	// Admit against the concurrency caps FIRST, before the pre-flight computeObservations scan;
	// otherwise a client at its cap would still trigger the full DB scan before rejection. release
	// is idempotent and deferred so every return path releases exactly once, and the stream is
	// handed to StreamFromChannelPreAdmitted so it does not acquire a second slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	if s.History == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/observations-unavailable",
			"Observations serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	if !rejectObservationsTierParams(w, r) {
		return
	}

	asset, quote, ok := parseObservationsAssetQuote(w, r)
	if !ok {
		return
	}
	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest, err.Error())
		return
	}

	source := r.URL.Query().Get("source")
	if !rawTradeSourceFilterOK(w, r, source) {
		return
	}

	aggregate := r.URL.Query().Get("aggregate")
	if aggregate != "" && aggregate != "latest" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-aggregate",
			"Invalid aggregate parameter", http.StatusBadRequest,
			`aggregate must be "latest" or omitted`)
		return
	}

	interval, ok := parseObservationsIntervalSeconds(w, r)
	if !ok {
		return
	}

	// Synchronous first compute. We commit to streaming here — even
	// an empty array is a valid steady-state for this surface, so we
	// don't 404 on emptiness (the request endpoint doesn't either).
	first, err := s.computeObservations(withFreshHistory(r.Context()), pair, source, aggregate)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if IsCacheUnavailable(err) {
			s.logger.Warn("computeObservations cache unavailable (stream prelude)",
				"err", err, "asset", asset.String(), "quote", quote.String(), "source", source)
			writeCacheUnavailableProblem(w, r)
			return
		}
		s.logger.Error("LatestTradePerSource failed (stream prelude)",
			"err", err, "asset", asset.String(), "quote", quote.String(), "source", source)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	var gen streaming.Generator
	ch := make(chan streaming.Event, observationsStreamProducerQueueDepth)
	prodCtx, cancelProd := context.WithCancel(r.Context())
	defer cancelProd()

	go s.runObservationsStreamProducer(
		prodCtx, ch, &gen, pair, source, aggregate, interval, first,
	)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
}

// computeObservations is the shared core of [Server.handleObservations] and
// [Server.handleObservationsStream]; it returns the post-aggregate trades ready for encoding.
//
// Alias fan-in: trades are stored under whichever spelling the decoder stamped (SDEX XLM legs
// under `native`, CEX feeds under `crypto:XLM`), so a literal lookup would leave `?asset=native`
// blind to CEX observations. Every alias spelling of the pair is scanned and the newest trade per
// source kept, as the price read paths do. Non-XLM pairs have one spelling and one scan.
func (s *Server) computeObservations(
	ctx context.Context, pair canonical.Pair, source, aggregate string,
) ([]canonical.Trade, error) {
	// Bound the scan even on the stream path, whose prelude and ticks have no ceiling of their
	// own. One ceiling spans ALL alias scans so the loop cannot multiply the worst-case hold.
	scanCtx, cancel := context.WithTimeout(ctx, observationsScanTimeout)
	defer cancel()

	var aliasPairs []canonical.Pair
	for _, b := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			if aliasPair, err := canonical.NewPair(b, q); err == nil {
				aliasPairs = append(aliasPairs, aliasPair)
			}
		}
	}
	// The alias scans are independent, so they run concurrently: an XLM
	// pair's cold read costs its slowest spelling, not the sum of all of them.
	results := make([][]canonical.Trade, len(aliasPairs))
	errs := make([]error, len(aliasPairs))
	var wg sync.WaitGroup
	for i, ap := range aliasPairs {
		wg.Go(func() {
			results[i], errs[i] = s.History.LatestTradePerSource(scanCtx, ap, source)
			if errs[i] != nil {
				cancel()
			}
		})
	}
	wg.Wait()

	// Merge in alias order so a same-timestamp tie resolves to the same
	// spelling it did when the scans ran one after another.
	var merged []canonical.Trade
	bySource := map[string]int{}
	for i := range aliasPairs {
		if errs[i] != nil {
			return nil, firstScanError(errs)
		}
		merged = mergeNewestPerSource(merged, bySource, results[i])
	}
	if aggregate == "latest" {
		merged = collapseToLatest(merged)
	}
	return merged, nil
}

// firstScanError returns the root cause among concurrent alias scans: an
// error that is not the cancellation a failing sibling triggered.
func firstScanError(errs []error) error {
	var first error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if !errors.Is(err, context.Canceled) {
			return err
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// mergeNewestPerSource folds `trades` into `merged`, keeping the most
// recent trade per source. `bySource` maps source → index in merged
// and is mutated in place.
func mergeNewestPerSource(merged []canonical.Trade, bySource map[string]int, trades []canonical.Trade) []canonical.Trade {
	for _, t := range trades {
		// Backstop for the storage-side filter: no exchange row is served raw.
		if !external.IsOnChain(t.Source) {
			continue
		}
		if i, ok := bySource[t.Source]; ok {
			if isLater(t, merged[i]) {
				merged[i] = t
			}
			continue
		}
		bySource[t.Source] = len(merged)
		merged = append(merged, t)
	}
	return merged
}

// observationsScanTimeout bounds a single LatestTradePerSource scan,
// matching the request handler's 8s ceiling in observations.go.
const observationsScanTimeout = 8 * time.Second

// runObservationsStreamProducer is the per-connection compute + push loop: it emits the
// pre-computed initial event, then recomputes and emits every `intervalSeconds` unconditionally.
// Per-tick failures are logged and skipped (heartbeats keep the connection alive). It returns
// when ctx cancels, closing ch so [streaming.StreamFromChannel] returns cleanly.
func (s *Server) runObservationsStreamProducer(
	ctx context.Context,
	ch chan<- streaming.Event,
	gen *streaming.Generator,
	pair canonical.Pair,
	source, aggregate string,
	intervalSeconds int,
	first []canonical.Trade,
) {
	// This producer runs in its OWN goroutine, which middleware.Recoverer does not wrap, and the
	// stream is reachable unauthenticated, so an unrecovered panic would kill the whole process.
	// Recover so only this connection dies. Registered BEFORE `defer close(ch)` so the close runs
	// FIRST (LIFO) and the SSE writer ends the response cleanly, then this logs; never swallow
	// the panic silently.
	defer s.recoverStreamProducer("observations")
	defer close(ch)

	if firstEv, ok := s.observationsStreamEvent(gen, pair, first); ok {
		select {
		case <-ctx.Done():
			return
		case ch <- firstEv:
		}
	}

	ticker := time.NewTicker(s.streamCadence(intervalSeconds))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trades, err := s.computeObservations(withFreshHistory(ctx), pair, source, aggregate)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("computeObservations failed (stream tick) — skipping emit",
						"err", err, "pair", pair.String(), "source", source)
				}
				continue
			}
			ev, ok := s.observationsStreamEvent(gen, pair, trades)
			if !ok {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}
}

// observationsStreamEvent builds one SSE event payload from trades, returning (_, false) on
// JSON-marshal failure. The envelope mirrors the request endpoint, including the single-source
// flag.
//
// stale, frozen and divergence_checked are structurally false on every tick, BY DESIGN: this is
// the raw per-source surface, with no aggregation contract to fall short of and no aggregated
// value for a cross-reference verdict to vouch for, so s.Divergence is never consulted. See the
// FRESHNESS CONTRACT block in handleObservations; pinned by
// TestObservationsStream_DivergenceCheckedStructurallyFalse.
func (s *Server) observationsStreamEvent(gen *streaming.Generator, pair canonical.Pair, trades []canonical.Trade) (streaming.Event, bool) {
	rows := make([]TradeRow, len(trades))
	srcSet := make(map[string]struct{}, len(trades))
	for i, t := range trades {
		rows[i] = tradeRowFrom(t, 0)
		srcSet[t.Source] = struct{}{}
	}
	// dex-nonstandard-decimals forward normalization of each row's Price (M2),
	// shared with /v1/observations + /v1/history via normalizeTradeRowPrices.
	// Byte-identical no-op for a pair with no confirmed non-7-decimals leg.
	s.normalizeTradeRowPrices(rows, trades, pair.Base, pair.Quote)
	srcs := make([]string, 0, len(srcSet))
	for src := range srcSet {
		srcs = append(srcs, src)
	}
	// Map iteration is unordered; sort so the `sources` array is
	// byte-identical across ticks (the response-equality contract).
	sort.Strings(srcs)

	body, err := json.Marshal(observationsStreamPayload{
		Data:    rows,
		AsOf:    WireTime(time.Now().UTC()),
		Sources: srcs,
		Flags:   Flags{SingleSource: len(srcs) == 1},
	})
	if err != nil {
		return streaming.Event{}, false
	}
	return streaming.Event{
		ID:   gen.Next(),
		Type: "observations_update",
		Data: body,
	}, true
}

// observationsStreamPayload is the SSE-data shape — same envelope
// as the request endpoint so SDK consumers can use one type for
// both polling and streaming.
type observationsStreamPayload struct {
	Data    []TradeRow `json:"data"`
	AsOf    WireTime   `json:"as_of"`
	Sources []string   `json:"sources,omitempty"`
	Flags   Flags      `json:"flags"`
}

// parseObservationsIntervalSeconds reads the optional interval_seconds
// query param, defaulting to defaultObservationsIntervalSeconds and
// rejecting values outside the documented clamp.
func parseObservationsIntervalSeconds(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("interval_seconds")
	if raw == "" {
		return defaultObservationsIntervalSeconds, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minObservationsIntervalSeconds || n > maxObservationsIntervalSeconds {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-interval",
			"Invalid interval_seconds", http.StatusBadRequest,
			"interval_seconds must be an integer in [1, 60]")
		return 0, false
	}
	return n, true
}
