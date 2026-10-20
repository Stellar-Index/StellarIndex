package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// PriceStreamTopic returns the Hub key `closed:<asset>/<quote>/<window_seconds>`, exported so publishers match the handler; the window is in the key so prices do not flap across windows.
func PriceStreamTopic(asset, quote canonical.Asset, windowSeconds int) string {
	return "closed:" + asset.String() + "/" + quote.String() + "/" + strconv.Itoa(windowSeconds)
}

// Closed-bucket stream window bounds. The ceiling is a sanity bound, not an allowlist: operators
// can reconfigure `[aggregate].windows` and the API cannot see that, so an unknown-but-sane window
// just subscribes to a topic that may stay quiet.
const (
	defaultClosedWindowSeconds = 300
	maxClosedWindowSeconds     = 366 * 24 * 60 * 60
)

// parseClosedWindowSeconds reads the optional window_seconds param for /v1/price/stream. Unlike
// tip windows, it names which aggregator bucket series to follow.
func parseClosedWindowSeconds(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("window_seconds")
	if raw == "" {
		return defaultClosedWindowSeconds, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxClosedWindowSeconds {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-window",
			"Invalid window_seconds", http.StatusBadRequest,
			"window_seconds must be a positive integer naming an aggregation window (default 300; the standard windows are 300, 3600, and 86400)")
		return 0, false
	}
	return n, true
}

// closedStreamGateSurface is this surface's label on obs.PriceServe{Scam,Substance}WithheldTotal,
// distinct so an operator can tell which surface withheld.
const closedStreamGateSurface = "price_stream"

// closedStreamGateBudget bounds ONE withholding consultation. The budget is mandatory (/stream skips the request-timeout
// middleware and statement_timeout is 0), and generous because both gates fail OPEN on timeout, so a tight budget would silently
// re-open the hole the gate closes.
const closedStreamGateBudget = tipStreamTickTimeout

// closedStreamWithheld reports whether an aggregated closed-bucket price claim for (asset, quote) may be published here. Both
// gates, never one: substance AND scam through the priceWithheld chokepoint (TestWithholdingGatesAreSpelledOnlyAtTheChokepoint).
// Scam is asked first, on both legs via [scamWithheld] (a flagged QUOTE issuer gets no inverted price). Nil gates withhold
// nothing.
func (s *Server) closedStreamWithheld(ctx context.Context, asset, quote canonical.Asset) pricingguard.Withholding {
	if s.Substance == nil && s.Scam == nil {
		return pricingguard.NotWithheld
	}
	ctx, cancel := context.WithTimeout(ctx, closedStreamGateBudget)
	defer cancel()
	return withheldBy(ctx, s.Substance, s.Scam, asset, quote, closedStreamGateSurface)
}

// closedStreamSeries picks ONE alias spelling's series per connection: XLM prices under `native` and `crypto:XLM` (disjoint trade
// sets), and forwarding both would interleave independent series that look like market moves. A frame is forwarded only when no
// higher-priority spelling (/v1/price's alias order) produced a bucket within [cachekeys.VWAPMaxAge] of it; time is the frame's
// bucket end, not wall clock, so replicas forward the same frames. Not goroutine-safe: one forwarder owns it.
type closedStreamSeries struct {
	rank     map[string]int    // "asset/quote" -> priority, 0 is preferred
	lastSeen map[int]time.Time // rank -> newest bucket end seen
}

// newClosedStreamSeries returns the series selector and the Hub topics, one per alias spelling.
func newClosedStreamSeries(asset, quote canonical.Asset, windowSeconds int) (*closedStreamSeries, []string) {
	c := &closedStreamSeries{rank: map[string]int{}, lastSeen: map[int]time.Time{}}
	var topics []string
	for _, a := range assetAliases(asset) {
		for _, q := range assetAliases(quote) {
			c.rank[a.String()+"/"+q.String()] = len(topics)
			topics = append(topics, PriceStreamTopic(a, q, windowSeconds))
		}
	}
	return c, topics
}

// admit reports whether a frame may be forwarded: a payload naming none of this connection's spellings passes (selection only
// chooses between alias series). A price_withheld frame is ranked but never recorded, since its as_of is wall clock and must not
// steer selection.
func (c *closedStreamSeries) admit(data []byte) bool {
	key, at, bucket, ok := closedStreamFrameSeries(data)
	if !ok {
		return true
	}
	r, ok := c.rank[key]
	if !ok {
		return true
	}
	if prev, seen := c.lastSeen[r]; bucket && (!seen || at.After(prev)) {
		c.lastSeen[r] = at
	}
	for higher := 0; higher < r; higher++ {
		if seen, ok := c.lastSeen[higher]; ok && at.Sub(seen) < cachekeys.VWAPMaxAge {
			return false
		}
	}
	return true
}

// closedStreamFrameSeries reads a frame's "asset/quote" series and time: a price_update's
// data.observed_at (bucket true) or a price_withheld body's as_of.
func closedStreamFrameSeries(data []byte) (key string, at time.Time, bucket, ok bool) {
	var env struct {
		Data struct {
			AssetID    string `json:"asset_id"`
			Quote      string `json:"quote"`
			ObservedAt string `json:"observed_at"`
		} `json:"data"`
		AssetID string `json:"asset_id"`
		Quote   string `json:"quote"`
		AsOf    string `json:"as_of"`
	}
	if json.Unmarshal(data, &env) != nil {
		return "", time.Time{}, false, false
	}
	if env.Data.ObservedAt != "" {
		at, err := time.Parse(time.RFC3339Nano, env.Data.ObservedAt)
		return env.Data.AssetID + "/" + env.Data.Quote, at, true, err == nil
	}
	at, err := time.Parse(time.RFC3339Nano, env.AsOf)
	return env.AssetID + "/" + env.Quote, at, false, err == nil && env.AssetID != ""
}

// forwardClosedStream bridges the Hub subscription onto the SSE writer channel, re-consulting the withholding gates for every event; it
// closes ch when ctx cancels or the Hub closes.
// The per-event re-check matters: the aggregator publishes with no pricingguard consultation, so connections opened before an issuer was
// flagged would keep receiving its rate.
// A withheld bucket is REPLACED by a price_withheld event: closing would cause an EventSource reconnect storm, dropping would read as a
// quiet market.
func (s *Server) forwardClosedStream(
	ctx context.Context,
	ch chan<- streaming.Event,
	sub <-chan streaming.Event,
	asset, quote canonical.Asset,
	series *closedStreamSeries,
) {
	defer s.recoverStreamProducer("price_stream")
	defer close(ch)

	var batch []streaming.Event
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-sub:
			if !open {
				return
			}
			// One verdict per drained batch: gating each event serially lets a slow gate back the
			// Hub queue up until Publish evicts this subscriber.
			batch = series.drainAdmitted(batch[:0], ev, sub)
			if len(batch) == 0 {
				continue
			}
			if withheld := s.closedStreamWithheld(ctx, asset, quote); withheld != pricingguard.NotWithheld {
				batch = closedStreamWithheldBatch(batch, asset, quote, withheldReasonFor(withheld))
			}
			if !sendClosedStreamBatch(ctx, ch, batch) {
				return
			}
		}
	}
}

// closedStreamWithheldBatch replaces every bucket with its price_withheld stand-in, dropping any
// that cannot be rendered.
func closedStreamWithheldBatch(batch []streaming.Event, asset, quote canonical.Asset, reason PriceWithheldReason) []streaming.Event {
	kept := batch[:0]
	for _, ev := range batch {
		if w, ok := closedStreamWithheldEvent(ev, asset, quote, reason); ok {
			kept = append(kept, w)
		}
	}
	return kept
}

// closedStreamWithheldEvent stands in for a refused closed bucket, keeping its id (so a resume
// cursor moves past it) and its as_of.
func closedStreamWithheldEvent(ev streaming.Event, asset, quote canonical.Asset, reason PriceWithheldReason) (streaming.Event, bool) {
	var bucket struct {
		AsOf WireTime `json:"as_of"`
	}
	if json.Unmarshal(ev.Data, &bucket) != nil || bucket.AsOf.IsZero() {
		bucket.AsOf = WireTime(time.Now().UTC())
	}
	body, err := json.Marshal(tipWithheldPayload{
		AssetID: asset.String(),
		Quote:   quote.String(),
		Reason:  reason,
		AsOf:    bucket.AsOf,
	})
	if err != nil {
		return streaming.Event{}, false
	}
	ev.Type, ev.Data = "price_withheld", body
	return ev, true
}

// drainAdmitted appends first and whatever sub already has queued, keeping the frames admit allows.
func (c *closedStreamSeries) drainAdmitted(batch []streaming.Event, first streaming.Event, sub <-chan streaming.Event) []streaming.Event {
	ev := first
	for n := len(sub); ; n-- {
		if c.admit(ev.Data) {
			batch = append(batch, ev)
		}
		if n == 0 {
			return batch
		}
		ev = <-sub
	}
}

// sendClosedStreamBatch reports false when ctx ended before the batch was handed over.
func sendClosedStreamBatch(ctx context.Context, ch chan<- streaming.Event, batch []streaming.Event) bool {
	for _, ev := range batch {
		select {
		case <-ctx.Done():
			return false
		case ch <- ev:
		}
	}
	return true
}

// closedStreamQueueDepth is the forwarder→writer hand-off buffer. It stays shallow because buckets
// arrive at most once per window; deeper would only stage staler prices behind a slow client.
const closedStreamQueueDepth = 4

// handlePriceStream serves GET /v1/price/stream, the SSE endpoint for the closed-bucket contract of /v1/price. Hub-driven: one event per
// closed bucket, byte-identical for every subscriber of an (asset, quote) topic.
// Wire: optional replay from `Last-Event-ID`; `price_update` shaped like /v1/price, or `price_withheld` while a gate refuses the pair; 15
// s comment-line heartbeats.
// Pre-flight 503 with no Hub; pre-flight 404 `errors/price-withheld` (the Redis->Hub bridge asks no gate). Both gates apply at connect
// AND on every forwarded bucket ([Server.forwardClosedStream]).
func (s *Server) handlePriceStream(w http.ResponseWriter, r *http.Request) {
	if s.Hub == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/stream-unavailable",
			"Closed-bucket stream not configured", http.StatusServiceUnavailable,
			"this deployment has no streaming Hub wired — typically because the aggregator publish path isn't running yet")
		return
	}

	// Same asset+quote contract and default fiat:USD quote as the request endpoint.
	asset, quote, ok := s.parseTipAssetQuote(w, r)
	if !ok {
		return
	}

	// Bucket series are selected by ?window_seconds= (ADR-0015), not a granularity param.
	if r.URL.Query().Get("granularity") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-stream-param",
			"granularity is not valid on /v1/price/stream", http.StatusBadRequest,
			"the closed-bucket stream selects its aggregation window via window_seconds (default 300); use /v1/history/since-inception for chart granularities")
		return
	}

	window, ok := parseClosedWindowSeconds(w, r)
	if !ok {
		return
	}

	// Admit BEFORE the gate consultation and Hub.Subscribe: the gate reads the DB, so a flood must
	// hit the stream caps before spending a pool connection, and the topic key is client-supplied,
	// so a refused connection must never mint one. Release is deferred so every return path
	// releases once; StreamFromChannelPreAdmitted below does not take a second slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	// Withholding pre-flight, while a non-200 status can still be set; same verdict and problem type
	// as /v1/price and /v1/price/tip/stream.
	if withheld := s.closedStreamWithheld(r.Context(), asset, quote); withheld != pricingguard.NotWithheld {
		writePriceWithheldProblem(w, r, asset, quote, withheldReasonFor(withheld))
		return
	}

	series, topics := newClosedStreamSeries(asset, quote, window)
	sub, cancelSub, err := s.Hub.Subscribe(topics, streaming.LastEventIDFrom(r))
	if err != nil {
		streaming.WriteSubscribeRefused(w)
		return
	}
	defer cancelSub()

	ch := make(chan streaming.Event, closedStreamQueueDepth)
	go s.forwardClosedStream(r.Context(), ch, sub, asset, quote, series)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
}
