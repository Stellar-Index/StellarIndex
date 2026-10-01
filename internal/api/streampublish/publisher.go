// Package streampublish bridges the closed-bucket VWAP cache to the
// SSE streaming Hub backing /v1/price/stream.
//
// One [Publisher] runs in the API binary alongside the Hub. For
// each operator-configured (asset, quote) pair it polls the same
// PriceReader the /v1/price handler uses; when a new closed bucket
// arrives (detected by ObservedAt advancing past the last published
// timestamp) it serialises the snapshot envelope and calls
// [streaming.Hub.Publish] on the matching topic. Subscribers
// attached to the topic via /v1/price/stream receive byte-identical
// payloads — the same cross-region consistency property as
// /v1/price itself (ADR-0015).
//
// Static pair list: the operator declares which pairs to broadcast
// in the binary's `[api.streaming]` config section. Adding a pair
// requires a config + restart. Pairs without observations stay
// silent — no synthetic events; a withheld pair publishes a
// price_withheld event instead of its bucket.
package streampublish

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// PriceReader is the narrow read-side dependency the publisher
// needs. Satisfied by the same v1.PriceReader the /v1/price
// handler consumes — declared here to avoid an import cycle into
// the v1 package's `Server`. In practice the production wiring
// passes a shared adapter to both.
type PriceReader interface {
	LatestPrice(ctx context.Context, asset, quote canonical.Asset) (
		snapshot v1.PriceSnapshot, sources []string, stale bool, err error,
	)
}

// Publisher polls a fixed set of pairs and republishes every newly-
// closed bucket to a Hub. Construct via [New], then call [Run] in
// a long-lived goroutine.
//
// Goroutine-safe — Run starts one inner goroutine per pair, all
// driven off the supplied context.
type Publisher struct {
	hub      *streaming.Hub
	reader   PriceReader
	interval time.Duration
	logger   *slog.Logger
	decimals *v1.NonstandardDecimalsCache
	frozen   v1.FrozenLooker

	// lastPublished tracks the most recent ObservedAt we've already
	// fanned out, keyed by topic. Each pair's poller goroutine has
	// exclusive access to its own key — no cross-goroutine writes —
	// but we still wrap in a mutex for race-detector cleanliness and
	// to keep the data structure honest if a future caller wants to
	// inspect it from elsewhere.
	mu            sync.Mutex
	lastPublished map[string]time.Time
	// withheld is the reason last put on the wire for a withheld topic.
	withheld map[string]v1.PriceWithheldReason
}

// Options holds Publisher's optional knobs, per the repo's trailing-struct
// constructor convention (docs/engineering-standards.md 14.2) — the zero
// value is a valid, fully-functional Publisher.
type Options struct {
	// Decimals wires the dex-nonstandard-decimals confirmed-decimals cache
	// so tickOnce can correct a raw snapshot before publishing (see
	// [v1.NormalizeRawPriceSnapshot]). nil (the zero value) treats every
	// pair as standard-decimals — the same fail-open default every other
	// consumer of the cache gets when it isn't wired.
	Decimals *v1.NonstandardDecimalsCache

	// Frozen wires the ADR-0019 freeze marker — the same looker
	// v1.Options.Freeze gives /v1/price. The closed bucket this publisher
	// reads is raw prices_1m, which the anomaly checker never gates, so
	// on a frozen pair it is the very bucket the freeze refused. With a
	// looker wired such a bucket is replaced by a price_frozen event, and
	// every price_update carries frozen_checked. nil leaves both off:
	// the stream then makes no freeze claim at all.
	Frozen v1.FrozenLooker
}

// New constructs a Publisher. The reader is the same PriceReader
// the /v1/price handler uses; the hub is the same instance passed
// to v1.Options.Hub. Interval clamps to 1 s minimum (zero / negative
// uses [DefaultInterval]).
func New(hub *streaming.Hub, reader PriceReader, interval time.Duration, logger *slog.Logger, opts Options) *Publisher {
	if hub == nil {
		panic("streampublish: hub must not be nil")
	}
	if reader == nil {
		panic("streampublish: reader must not be nil")
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	if interval < time.Second {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{
		hub:           hub,
		reader:        reader,
		interval:      interval,
		logger:        logger,
		decimals:      opts.Decimals,
		frozen:        opts.Frozen,
		lastPublished: map[string]time.Time{},
		withheld:      map[string]v1.PriceWithheldReason{},
	}
}

// DefaultInterval is the per-pair poll cadence used when the
// caller passes 0 / negative to [New]. 5 s detects a new 1-minute
// closed bucket within 5 s of its end — well inside the 30 s
// freshness SLA target — without hammering the reader for
// pairs that update once per minute.
const DefaultInterval = 5 * time.Second

// Run starts one polling goroutine per pair and blocks until ctx
// is cancelled. Returns ctx.Err() on shutdown so callers can chain
// onto an errgroup without losing the cancel cause.
//
// Pairs is treated as immutable for the Publisher's lifetime —
// adding a pair requires a binary restart. The cost of that
// constraint is recovered by simpler bookkeeping (no
// add/remove/ref-count machinery).
func (p *Publisher) Run(ctx context.Context, pairs []canonical.Pair) error {
	if len(pairs) == 0 {
		// No pairs configured — nothing to do, but block until
		// shutdown so callers can wait on the same ctx without a
		// special-cased zero path.
		<-ctx.Done()
		return ctx.Err()
	}

	var wg sync.WaitGroup
	for _, pair := range pairs {
		wg.Add(1)
		go func(pair canonical.Pair) {
			// A panic in one pair's poll loop must not crash the whole API
			// process. The binary's outer recoverBackgroundWorker wraps
			// only the goroutine that CALLS Run — it cannot catch a panic
			// in these per-pair goroutines Run fans out, so each needs its
			// own guard (logged at Error with its stack; that pair stops
			// publishing, the others keep going).
			defer worker.Recover(p.logger, "streampublish:"+pair.String())
			defer wg.Done()
			p.pollLoop(ctx, pair)
		}(pair)
	}
	wg.Wait()
	return ctx.Err()
}

// publishWindowSeconds is the window component of this publisher's
// Hub topics. The snapshots it forwards are /v1/price's 1-minute
// closed buckets, so its series is the 60-second window — distinct
// from the aggregator windows (300/3600/86400) the redispub bridge
// publishes, now that the window is part of the topic key.
const publishWindowSeconds = 60

// pollLoop is the per-pair ticker. Returns when ctx is cancelled.
func (p *Publisher) pollLoop(ctx context.Context, pair canonical.Pair) {
	topic := v1.PriceStreamTopic(pair.Base, pair.Quote, publishWindowSeconds)
	t := time.NewTicker(p.interval)
	defer t.Stop()

	// Poll once immediately so a freshly-restarted API binary
	// publishes the most-recent closed bucket without waiting a full
	// interval. The detect-change machinery guards against
	// republishing the same bucket on the next tick.
	p.tickOnce(ctx, pair, topic)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tickOnce(ctx, pair, topic)
		}
	}
}

// tickOnce queries the latest price for one pair and publishes if
// the ObservedAt advanced past the last-published timestamp.
//
// Best-effort: a reader error logs at WARN and the loop continues —
// a Postgres outage shouldn't take the publisher down across other
// pairs. ErrPriceNotFound is silent (the pair has no closed bucket
// yet, or has fallen outside the freshness window); ErrPriceWithheld
// publishes a price_withheld event (see [Publisher.publishWithheld]).
func (p *Publisher) tickOnce(ctx context.Context, pair canonical.Pair, topic string) {
	pollCtx, cancel := context.WithTimeout(ctx, p.interval)
	defer cancel()

	snap, sources, stale, err := p.reader.LatestPrice(pollCtx, pair.Base, pair.Quote)
	if err != nil {
		if errors.Is(err, v1.ErrPriceWithheld) {
			p.publishWithheld(pair, topic, v1.PriceWithheldReasonOf(err))
			return
		}
		if errors.Is(err, v1.ErrPriceNotFound) {
			return
		}
		// Suppress log noise on shutdown: the parent ctx itself is
		// done. A pollCtx-only DeadlineExceeded (the parent still
		// live) is a reader stall, not shutdown, and must not be
		// mistaken for one — see [obs.StreamPublishStallTotal].
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			obs.StreamPublishStallTotal.WithLabelValues("price_stream").Inc()
			p.logger.Warn("streampublish: reader missed poll deadline",
				"pair", pair.String(), "interval", p.interval)
			return
		}
		p.logger.Warn("streampublish: LatestPrice failed",
			"err", err, "pair", pair.String())
		return
	}
	if !p.shouldPublish(topic, snap.ObservedAt.Time()) {
		return
	}

	frozen, frozenChecked := p.frozenVerdict(pollCtx, pair)
	if frozen {
		p.publishFrozen(pair, topic, snap, stale)
		return
	}

	// dex-nonstandard-decimals forward normalization (M2). reader.LatestPrice
	// returns the RAW closed-1m/last-trade ratio (see
	// v1.Server.normalizeRawPriceSnapshot's doc comment) — the handler-side
	// /v1/price path corrects it before serving, and this producer must too,
	// or a flagged Soroban pair streams the wrong price by a power of ten
	// while the REST surface serves the corrected one. Byte-identical no-op
	// for every standard-decimals pair (the overwhelming common case).
	v1.NormalizeRawPriceSnapshot(&snap, pair.Base, pair.Quote, p.decimals)

	// The DOCUMENTED envelope shape — field-compatible with /v1/price
	// responses and the redispub bridge's fan-out (cold audit
	// 2026-08-03: this producer previously emitted a bespoke
	// {snapshot, sources, stale} shape that matched neither). as_of is
	// the bucket's ObservedAt — deterministic, preserving the
	// byte-identical cross-region property this package's docs promise.
	// AsOf is v1.WireTime, not time.Time, for the same reason every
	// timestamp on the v1 wire is: ObservedAt carries whatever location
	// the stored bucket decoded into, and a plain time.Time field would
	// publish the server's local offset to SSE subscribers while the
	// field-compatible /v1/price response published `Z`.
	payload, err := json.Marshal(struct {
		Data    v1.PriceSnapshot `json:"data"`
		AsOf    v1.WireTime      `json:"as_of"`
		Sources []string         `json:"sources,omitempty"`
		Flags   streamFlags      `json:"flags"`
	}{Data: snap, AsOf: snap.ObservedAt, Sources: sources, Flags: streamFlags{Stale: stale, FrozenChecked: frozenChecked}})
	if err != nil {
		// json.Marshal of a fixed shape that already round-trips
		// through /v1/price — only surfaces on a Go runtime defect.
		p.logger.Error("streampublish: marshal failed",
			"err", err, "pair", pair.String())
		return
	}

	p.hub.Publish(topic, "price_update", payload)
	obs.StreamPublishTotal.WithLabelValues("price_stream").Inc()
}

// streamFlags is the subset of v1.Flags this publisher evaluates; a flag
// it does not evaluate is absent rather than a false it never checked.
// Frozen is set only on price_frozen: a frozen bucket is never published
// as a price_update.
type streamFlags struct {
	Stale         bool `json:"stale"`
	Frozen        bool `json:"frozen,omitempty"`
	FrozenChecked bool `json:"frozen_checked,omitempty"`
}

// frozenBucket identifies the bucket a price_frozen event stands in for.
type frozenBucket struct {
	AssetID       string      `json:"asset_id"`
	Quote         string      `json:"quote"`
	ObservedAt    v1.WireTime `json:"observed_at"`
	WindowSeconds int         `json:"window_seconds"`
}

// frozenVerdict reads the freeze marker of every spelling of the pair's
// base, as /v1/price does when it cannot tell which alias its bucket was
// read from (the reader here does not say). Any frozen spelling governs;
// checked is true only when every marker read succeeded, so an unknown
// verdict never travels as "confirmed not frozen". A read error fails
// open to publishing without frozen_checked — /v1/price's posture for
// the same error.
func (p *Publisher) frozenVerdict(ctx context.Context, pair canonical.Pair) (frozen, checked bool) {
	if p.frozen == nil {
		return false, false
	}
	checked = true
	for _, alias := range canonical.AssetAliases(pair.Base) {
		aliasFrozen, err := p.frozen.FrozenForPair(ctx, alias, pair.Quote)
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Warn("streampublish: freeze lookup failed",
					"err", err, "pair", pair.String(), "alias", alias.String())
			}
			checked = false
			continue
		}
		if aliasFrozen {
			return true, true
		}
	}
	return false, checked
}

// publishFrozen puts the freeze on the wire in place of the bucket it
// refused. /v1/price never serves that bucket under the flag (it serves
// the held VWAP or refuses), and this 60-second series has no held value
// of its own, so the event carries the bucket's identity and no price.
// Silence would be indistinguishable from a pair with no trades. stale is
// the reader's verdict on that bucket, carried as price_update carries it.
func (p *Publisher) publishFrozen(pair canonical.Pair, topic string, snap v1.PriceSnapshot, stale bool) {
	payload, err := json.Marshal(struct {
		Data  frozenBucket `json:"data"`
		AsOf  v1.WireTime  `json:"as_of"`
		Flags streamFlags  `json:"flags"`
	}{
		Data:  frozenBucket{AssetID: snap.AssetID, Quote: snap.Quote, ObservedAt: snap.ObservedAt, WindowSeconds: snap.WindowSeconds},
		AsOf:  snap.ObservedAt,
		Flags: streamFlags{Stale: stale, Frozen: true, FrozenChecked: true},
	})
	if err != nil {
		p.logger.Error("streampublish: marshal failed",
			"err", err, "pair", pair.String())
		return
	}
	p.hub.Publish(topic, "price_frozen", payload)
	obs.StreamPublishTotal.WithLabelValues("price_stream").Inc()
}

// withheldPair is the data of a price_withheld event, the shape
// /v1/price/tip/stream's event of the same name carries.
type withheldPair struct {
	AssetID string                 `json:"asset_id"`
	Quote   string                 `json:"quote"`
	Reason  v1.PriceWithheldReason `json:"reason"`
	AsOf    v1.WireTime            `json:"as_of"`
}

// publishWithheld puts a withholding on the wire once per reason, so an
// open stream reads it as withheld rather than as a pair with no trades.
// Forgetting the last published bucket republishes it once the pair is
// served again, even if no new bucket has closed since.
func (p *Publisher) publishWithheld(pair canonical.Pair, topic string, reason v1.PriceWithheldReason) {
	p.mu.Lock()
	prev, seen := p.withheld[topic]
	p.withheld[topic] = reason
	delete(p.lastPublished, topic)
	p.mu.Unlock()
	if seen && prev == reason {
		return
	}
	payload, err := json.Marshal(withheldPair{
		AssetID: pair.Base.String(),
		Quote:   pair.Quote.String(),
		Reason:  reason,
		AsOf:    v1.WireTime(time.Now().UTC()),
	})
	if err != nil {
		p.logger.Error("streampublish: marshal failed",
			"err", err, "pair", pair.String())
		return
	}
	p.hub.Publish(topic, "price_withheld", payload)
	obs.StreamPublishTotal.WithLabelValues("price_stream").Inc()
}

// shouldPublish records the latest observed timestamp for the
// topic and returns true on advance. False (no advance) is the
// common case between bucket closes — the reader returns the same
// bucket for every poll until a new one materialises. A served read
// also ends any withholding recorded for the topic.
func (p *Publisher) shouldPublish(topic string, observedAt time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.withheld, topic)
	prev, seen := p.lastPublished[topic]
	if seen && !observedAt.After(prev) {
		return false
	}
	p.lastPublished[topic] = observedAt
	return true
}
