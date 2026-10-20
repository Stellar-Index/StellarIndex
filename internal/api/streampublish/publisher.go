// Package streampublish bridges the closed-bucket VWAP cache to the SSE streaming Hub backing
// /v1/price/stream.
//
// One [Publisher] polls the same PriceReader as /v1/price for each configured pair and calls
// [streaming.Hub.Publish] when ObservedAt advances. Payloads are byte-identical to /v1/price,
// keeping the cross-region consistency property (ADR-0015). Pairs without observations stay silent;
// a withheld pair publishes price_withheld instead of its bucket.
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

// PriceReader is the publisher's read-side dependency, satisfied by the adapter the /v1/price handler uses; declared here to avoid an import cycle into v1's Server.
type PriceReader interface {
	LatestPrice(ctx context.Context, asset, quote canonical.Asset) (
		snapshot v1.PriceSnapshot, sources []string, stale bool, err error,
	)
}

// Publisher polls a fixed set of pairs and republishes every newly closed bucket to a Hub; construct via [New] and run [Run] in a long-lived goroutine, which starts one goroutine per pair.
type Publisher struct {
	hub      *streaming.Hub
	reader   PriceReader
	interval time.Duration
	logger   *slog.Logger
	decimals *v1.NonstandardDecimalsCache
	frozen   v1.FrozenLooker

	// lastPublished is the latest ObservedAt already fanned out, per topic. Each pair's poller owns
	// its own key; the mutex keeps the map race-clean.
	mu            sync.Mutex
	lastPublished map[string]time.Time
	// withheld is the reason last put on the wire for a withheld topic.
	withheld map[string]v1.PriceWithheldReason
}

// Options holds Publisher's optional knobs (trailing-struct constructor convention, docs/engineering-standards.md 14.2); the zero value is a valid, fully-functional Publisher.
type Options struct {
	// Decimals is the confirmed-decimals cache used to correct a raw snapshot before publishing
	// (see [v1.NormalizeRawPriceSnapshot]). nil treats every pair as standard-decimals.
	Decimals *v1.NonstandardDecimalsCache

	// Frozen is the ADR-0019 freeze marker, the same looker /v1/price uses. The raw prices_1m bucket
	// read here is never gated by the anomaly checker, so on a frozen pair it is the bucket the
	// freeze refused: it becomes a price_frozen event and every price_update carries frozen_checked.
	// nil leaves both off, so the stream makes no freeze claim.
	Frozen v1.FrozenLooker
}

// New constructs a Publisher reading via the same PriceReader as /v1/price and publishing to the hub passed as v1.Options.Hub; interval clamps to 1 s minimum (zero or negative uses [DefaultInterval]).
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

// DefaultInterval is the per-pair poll cadence when [New] gets 0 or negative; 5 s catches a new 1-minute bucket well inside the 30 s freshness target without hammering the reader.
const DefaultInterval = 5 * time.Second

// Run starts one polling goroutine per pair and blocks until ctx is cancelled, returning ctx.Err() so errgroup callers keep the cancel cause; pairs are immutable for the Publisher's lifetime, avoiding add/remove bookkeeping.
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
			// The binary's outer recover covers only the goroutine that calls Run, not these per-pair
			// goroutines, so each needs its own guard: that pair stops publishing, the others go on.
			defer worker.Recover(p.logger, "streampublish:"+pair.String())
			defer wg.Done()
			p.pollLoop(ctx, pair)
		}(pair)
	}
	wg.Wait()
	return ctx.Err()
}

// publishWindowSeconds is the window part of this publisher's Hub topics: /v1/price's 1-minute
// buckets are the 60-second series, distinct from the aggregator windows the redispub bridge
// publishes.
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

// tickOnce publishes one pair's latest price if ObservedAt advanced. Best-effort: a reader error logs at WARN so a Postgres outage
// does not take down other pairs. ErrPriceNotFound is silent; ErrPriceWithheld publishes price_withheld ([Publisher.publishWithheld]).
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

	// reader.LatestPrice returns the RAW ratio; /v1/price corrects it before serving, so this
	// producer must too, or a flagged Soroban pair streams a price off by a power of ten.
	// A no-op for standard-decimals pairs.
	v1.NormalizeRawPriceSnapshot(&snap, pair.Base, pair.Quote, p.decimals)

	// The envelope is field-compatible with /v1/price and the redispub fan-out. as_of is the
	// bucket's ObservedAt, keeping payloads byte-identical across regions. AsOf is v1.WireTime
	// because ObservedAt carries whatever location the bucket decoded into; a plain time.Time
	// would publish the server's local offset where /v1/price publishes Z.
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

// streamFlags is the subset of v1.Flags this publisher evaluates; an unevaluated flag is absent rather than a false it never checked.
// Frozen is set only on price_frozen: a frozen bucket is never published as a price_update.
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

// frozenVerdict reads the freeze marker of every alias of the pair's base, as /v1/price does. Any frozen alias governs; checked is
// true only when every read succeeded, so an unknown verdict never travels as "confirmed not frozen". A read error fails open.
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

// publishFrozen puts the freeze on the wire in place of the refused bucket. This series has no
// held value, so the event carries the bucket's identity and no price; silence would look like a
// pair with no trades. stale is the reader's verdict, carried as price_update carries it.
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

// publishWithheld puts a withholding on the wire once per reason, so an open stream reads withheld rather than "no trades".
// Forgetting the last published bucket republishes it once the pair is served again.
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

// shouldPublish records the latest observed timestamp for the topic and returns true on advance;
// false is the common case, as the reader repeats the same bucket until a new one closes. A served
// read also ends any withholding recorded for the topic.
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
