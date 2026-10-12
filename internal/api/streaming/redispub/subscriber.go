package redispub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/wiretime"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// RedisSubscriber is the subset of the Redis client [Subscriber] needs, so tests can substitute miniredis.
type RedisSubscriber interface {
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

// Hub is the subset of [streaming.Hub] the subscriber needs, so tests can substitute a recorder.
type Hub interface {
	Publish(topic, eventType string, data []byte) string
}

// Subscriber listens on the [Publisher]'s channel ([DefaultChannel]) and republishes each [ClosedBucketEvent] on the Hub under the `internal/api/v1.PriceStreamTopic` key. Per-topic dedupe state is mutex-guarded; other fields are read-only after construction.
type Subscriber struct {
	cache   RedisSubscriber
	channel string
	hub     Hub
	logger  *slog.Logger

	mu         sync.Mutex
	forwarded  map[string]topicMark // topic -> newest bucket fanned out
	lastDupLog time.Time
}

// topicMark is the newest bucket a topic has fanned out, and who sent it.
type topicMark struct {
	observedAt time.Time
	producer   string
}

// forwardedPruneAt is the topic count at which marks older than observedAtMaxAge are swept; validateEvent rejects anything that old, so a
// swept mark can't admit a duplicate.
const forwardedPruneAt = 4096

// duplicateProducerLogEvery rate-limits the two-producers warning: with
// two aggregators live every (pair, window) bucket arrives twice.
const duplicateProducerLogEvery = time.Minute

// NewSubscriber constructs a Subscriber for the given channel and Hub; empty channel falls back to [DefaultChannel], nil logger to [slog.Default].
func NewSubscriber(cache RedisSubscriber, channel string, hub Hub, logger *slog.Logger) (*Subscriber, error) {
	if cache == nil {
		return nil, errors.New("redispub: RedisSubscriber is required")
	}
	if hub == nil {
		return nil, errors.New("redispub: Hub is required")
	}
	if channel == "" {
		channel = DefaultChannel
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Seeded here, not in obs, so only a process that wires a subscriber
	// exports the family and "no ok events" reads as a real zero.
	for _, o := range subscribeOutcomes {
		obs.APIStreamSubscribeTotal.WithLabelValues(o)
	}
	installChannelDropCounter()
	return &Subscriber{
		cache: cache, channel: channel, hub: hub, logger: logger,
		forwarded: make(map[string]topicMark),
	}, nil
}

// claim reports whether ev is the first event for its bucket on topic, recording it. A repeat or older bucket is refused, so a topic
// carries one in-order frame per bucket. Kind is not in the key: it would let a delayed older bucket follow a newer one of the other kind.
func (s *Subscriber) claim(topic string, ev *ClosedBucketEvent, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, seen := s.forwarded[topic]
	if seen && !ev.ObservedAt.After(prev.observedAt) {
		if ev.ProducerID != prev.producer && now.Sub(s.lastDupLog) >= duplicateProducerLogEvery {
			s.lastDupLog = now
			s.logger.Warn("redispub: a second producer published a bucket already forwarded — more than one aggregator is publishing on this channel",
				"channel", s.channel, "topic", topic,
				"forwarded_producer", prev.producer, "duplicate_producer", ev.ProducerID)
		}
		return false
	}
	if len(s.forwarded) >= forwardedPruneAt {
		for t, m := range s.forwarded {
			if m.observedAt.Before(now.Add(-observedAtMaxAge)) {
				delete(s.forwarded, t)
			}
		}
	}
	s.forwarded[topic] = topicMark{observedAt: ev.ObservedAt, producer: ev.ProducerID}
	return true
}

// Outcome labels for [obs.APIStreamSubscribeTotal]. Each rejection
// cause that points a responder somewhere different gets its own label.
const (
	outcomeOK                  = "ok"
	outcomeDecodeError         = "decode_error"
	outcomeMalformed           = "malformed"
	outcomeFutureObservedAt    = "future_observed_at"
	outcomeStaleObservedAt     = "stale_observed_at"
	outcomeDuplicate           = "duplicate"
	outcomeDroppedSlowConsumer = "dropped_slow_consumer"
)

var subscribeOutcomes = []string{
	outcomeOK, outcomeDecodeError, outcomeMalformed, outcomeFutureObservedAt, outcomeStaleObservedAt,
	outcomeDuplicate, outcomeDroppedSlowConsumer,
}

// subscribeChannelSize and subscribeChannelSendTimeout are passed to PubSub.Channel explicitly, so the buffer is a documented choice,
// not go-redis's implicit default (100 messages / 1 minute).
const (
	subscribeChannelSize        = 100
	subscribeChannelSendTimeout = time.Minute
)

// channelFullLogFormat is the go-redis v9.22.0 PubSub.Channel log format on a send-timeout drop. It has no other hook, so
// [installChannelDropCounter] matches on it; a wording change only loses the count (fails safe).
const channelFullLogFormat = "redis: %v channel is full for %s (message is dropped)"

// channelDropCounterOnce guards [installChannelDropCounter]: go-redis's logger is one package-global var and there is one Subscriber
// per binary, so a second install would only replace an identical wrapper.
var channelDropCounterOnce sync.Once

// installChannelDropCounter installs a go-redis logger that counts [obs.APIStreamSubscribeTotal]'s dropped_slow_consumer on each
// PubSub.Channel drop, then forwards the line unchanged.
// Process-wide, safe only while this package is the sole PubSub.Channel caller.
func installChannelDropCounter() {
	channelDropCounterOnce.Do(func() {
		redis.SetLogger(channelDropCountingLogger{})
	})
}

// channelDropCountingLogger implements go-redis's internal.Logging structurally (Printf(ctx, format, args...)) without importing its
// internal package.
type channelDropCountingLogger struct{}

func (channelDropCountingLogger) Printf(_ context.Context, format string, v ...interface{}) {
	if format == channelFullLogFormat {
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeDroppedSlowConsumer).Inc()
	}
	slog.Default().Warn(fmt.Sprintf(format, v...))
}

// Sentinels validateEvent wraps so handleMessage can label the drop by cause: a clock-skewed API host rejects EVERY event as future-dated,
// which must not read as a wire-format bug.
var (
	errObservedAtFuture = errors.New("observed_at is in the future")
	errObservedAtStale  = errors.New("observed_at is too old")
)

// rejectionOutcome maps a validateEvent error to its outcome label.
func rejectionOutcome(err error) string {
	switch {
	case errors.Is(err, errObservedAtFuture):
		return outcomeFutureObservedAt
	case errors.Is(err, errObservedAtStale):
		return outcomeStaleObservedAt
	default:
		return outcomeMalformed
	}
}

// Channel returns the Redis channel this Subscriber listens on.
func (s *Subscriber) Channel() string { return s.channel }

// Run republishes each Redis message on the Hub until ctx is cancelled (returns ctx.Err()). A Redis outage does not end Run (go-redis retries internally) and shows as a flat `ok` on stellarindex_api_stream_subscribe_total, which stellarindex_api_price_stream_not_delivering alerts on; the channel-closed error is a backstop.
func (s *Subscriber) Run(ctx context.Context) error {
	pubsub := s.cache.Subscribe(ctx, s.channel)
	defer func() {
		if err := pubsub.Close(); err != nil {
			s.logger.Warn("redispub: pubsub close", "err", err)
		}
	}()

	ch := pubsub.Channel(
		redis.WithChannelSize(subscribeChannelSize),
		redis.WithChannelSendTimeout(subscribeChannelSendTimeout),
	)
	s.logger.Info("redispub: subscriber listening", "channel", s.channel)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				// Only a closed PubSub closes the channel (see the doc
				// above); surface it so the supervisor can restart.
				return errors.New("redispub: subscribe channel closed unexpectedly")
			}
			s.handleMessage([]byte(msg.Payload))
		}
	}
}

// handleMessage decodes and validates one wire payload and republishes the canonical re-marshalled event on the Hub; failures log and
// count but never propagate. Redis has no AUTH, so every field is bounded and the validated struct (not raw bytes) is fanned out,
// keeping forged JSON fields from clients. A bucket already fanned out is dropped ([Subscriber.claim]).
func (s *Subscriber) handleMessage(payload []byte) {
	var ev ClosedBucketEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeDecodeError).Inc()
		s.logger.Warn("redispub: decode message", "err", err, "payload_len", len(payload))
		return
	}
	now := time.Now()
	if err := validateEvent(&ev, now); err != nil {
		outcome := rejectionOutcome(err)
		obs.APIStreamSubscribeTotal.WithLabelValues(outcome).Inc()
		s.logger.Warn("redispub: rejected event (failed validation)",
			"outcome", outcome, "err", err, "asset", ev.Asset, "quote", ev.Quote)
		return
	}
	topic := topicForPair(ev.Asset, ev.Quote, ev.WindowSeconds)
	if !s.claim(topic, &ev, now) {
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeDuplicate).Inc()
		return
	}
	eventType, sanitized, err := sseFrame(&ev)
	if err != nil {
		// Marshalling a fully-typed, already-decoded struct cannot
		// fail; defensive for static analysis only.
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeMalformed).Inc()
		s.logger.Warn("redispub: re-marshal validated event", "err", err)
		return
	}
	s.hub.Publish(topic, eventType, sanitized)
	obs.APIStreamSubscribeTotal.WithLabelValues(outcomeOK).Inc()
}

// sseFrame renders a validated event as its /v1/price/stream frame (price_update, or price_frozen in the 60-second series shape).
// as_of is ObservedAt, the end of the closed 1-minute bucket; deriving it from the event, not the local clock, keeps payloads
// byte-identical across subscribers (ADR-0015).
func sseFrame(ev *ClosedBucketEvent) (eventType string, data []byte, err error) {
	if ev.Kind == KindFrozen {
		data, err = json.Marshal(frozenBucketEnvelope{
			Data: frozenBucketWireData{
				AssetID:       ev.Asset,
				Quote:         ev.Quote,
				ObservedAt:    wiretime.Time(ev.ObservedAt),
				WindowSeconds: ev.WindowSeconds,
				FrozenSince:   (*wiretime.Time)(ev.FrozenSince),
			},
			AsOf:  wiretime.Time(ev.ObservedAt),
			Flags: frozenFlags{Frozen: true, FrozenChecked: true},
		})
		return "price_frozen", data, err
	}
	data, err = json.Marshal(closedBucketEnvelope{
		Data: closedBucketWireData{
			AssetID:       ev.Asset,
			Quote:         ev.Quote,
			Price:         ev.ValueDecimal,
			PriceType:     "vwap",
			ObservedAt:    wiretime.Time(ev.ObservedAt),
			WindowSeconds: ev.WindowSeconds,
			Truncated:     ev.Truncated,
			CoveredFrom:   (*wiretime.Time)(ev.CoveredFrom),
		},
		AsOf: wiretime.Time(ev.ObservedAt),
	})
	return "price_update", data, err
}

// closedBucketEnvelope is the SSE wire shape for /v1/price/stream, field-compatible with the /v1/price envelope. No `flags` object:
// the event carries no stale/frozen verdicts, and `stale: false` would turn missing data into a false claim.
type closedBucketEnvelope struct {
	Data closedBucketWireData `json:"data"`
	AsOf wiretime.Time        `json:"as_of"`
}

type closedBucketWireData struct {
	AssetID       string         `json:"asset_id"`
	Quote         string         `json:"quote"`
	Price         string         `json:"price"`
	PriceType     string         `json:"price_type"`
	ObservedAt    wiretime.Time  `json:"observed_at"`
	WindowSeconds int64          `json:"window_seconds"`
	Truncated     *bool          `json:"truncated,omitempty"`
	CoveredFrom   *wiretime.Time `json:"covered_from,omitempty"`
}

// frozenBucketEnvelope is the price_frozen frame: the aggregator authored the freeze, so frozen is a checked verdict; stale is not
// evaluated here and is absent.
type frozenBucketEnvelope struct {
	Data  frozenBucketWireData `json:"data"`
	AsOf  wiretime.Time        `json:"as_of"`
	Flags frozenFlags          `json:"flags"`
}

type frozenBucketWireData struct {
	AssetID       string         `json:"asset_id"`
	Quote         string         `json:"quote"`
	ObservedAt    wiretime.Time  `json:"observed_at"`
	WindowSeconds int64          `json:"window_seconds"`
	FrozenSince   *wiretime.Time `json:"frozen_since,omitempty"`
}

type frozenFlags struct {
	Frozen        bool `json:"frozen"`
	FrozenChecked bool `json:"frozen_checked"`
}

const (
	// maxWindowSeconds bounds ClosedBucketEvent.WindowSeconds by sanity (positive, under a year), not an allowlist, since operators may
	// override the aggregator windows unseen by the API.
	maxWindowSeconds = int64(366 * 24 * 60 * 60)

	// observedAtFutureSkew is how far ahead of local time a bucket-end timestamp may sit before it is treated as forged; absorbs
	// aggregator/API clock skew.
	observedAtFutureSkew = 5 * time.Minute

	// observedAtMaxAge bounds how stale a closed-bucket event may be: ObservedAt is the window's END, so even 24h lands within a minute of
	// real time; a day of slack absorbs lag without admitting an ancient replayed price.
	observedAtMaxAge = 24 * time.Hour
)

// maxValueDecimal is the exclusive upper bound on a published VWAP: 10^18 is far beyond any real price yet rejects overflow-style garbage
// (defense in depth; network isolation is the primary control).
var maxValueDecimal = new(big.Rat).SetInt(
	new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
)

// validateEvent rejects any decoded ClosedBucketEvent a trusted aggregator would never have produced; nil means safe to fan out.
func validateEvent(ev *ClosedBucketEvent, now time.Time) error {
	if ev.Asset == "" || ev.Quote == "" {
		return errors.New("empty asset or quote")
	}
	if err := validateCanonicalAsset("asset", ev.Asset); err != nil {
		return err
	}
	if err := validateCanonicalAsset("quote", ev.Quote); err != nil {
		return err
	}
	if ev.Asset == ev.Quote {
		return fmt.Errorf("asset and quote are both %q", ev.Asset)
	}
	if ev.WindowSeconds <= 0 || ev.WindowSeconds > maxWindowSeconds {
		return fmt.Errorf("window_seconds %d out of range", ev.WindowSeconds)
	}
	if ev.ObservedAt.IsZero() {
		return errors.New("missing observed_at")
	}
	if ev.ObservedAt.After(now.Add(observedAtFutureSkew)) {
		return fmt.Errorf("%w: %s (local clock %s)", errObservedAtFuture,
			ev.ObservedAt.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if ev.ObservedAt.Before(now.Add(-observedAtMaxAge)) {
		return fmt.Errorf("%w: %s", errObservedAtStale, ev.ObservedAt.Format(time.RFC3339))
	}
	switch ev.Kind {
	case "":
		if _, err := parseValueDecimal(ev.ValueDecimal); err != nil {
			return err
		}
		if ev.FrozenSince != nil {
			return errors.New("frozen_since on a price event")
		}
		return validateCoverage(ev)
	case KindFrozen:
		return validateFrozen(ev)
	default:
		return fmt.Errorf("unknown kind %q", ev.Kind)
	}
}

// validateFrozen rejects a frozen event carrying any part of a value, or
// a frozen_since after the bucket it stands in for.
func validateFrozen(ev *ClosedBucketEvent) error {
	if ev.ValueDecimal != "" || ev.Truncated != nil || ev.CoveredFrom != nil {
		return errors.New("frozen event carries a value")
	}
	if ev.FrozenSince != nil && (ev.FrozenSince.IsZero() || ev.FrozenSince.After(ev.ObservedAt)) {
		return fmt.Errorf("frozen_since %s not at or before observed_at", ev.FrozenSince.Format(time.RFC3339))
	}
	return nil
}

// validateCoverage rejects a covered_from that is not the truncation
// point of a truncated window inside [observed_at-window, observed_at).
func validateCoverage(ev *ClosedBucketEvent) error {
	truncated := ev.Truncated != nil && *ev.Truncated
	if truncated != (ev.CoveredFrom != nil) {
		return errors.New("truncated and covered_from must be set together")
	}
	if !truncated {
		return nil
	}
	windowStart := ev.ObservedAt.Add(-time.Duration(ev.WindowSeconds) * time.Second)
	if ev.CoveredFrom.Before(windowStart) || !ev.CoveredFrom.Before(ev.ObservedAt) {
		return fmt.Errorf("covered_from %s outside the window", ev.CoveredFrom.Format(time.RFC3339))
	}
	return nil
}

// validateCanonicalAsset rejects a leg not in canonical asset-id spelling: Asset and Quote are echoed to clients and form the Hub
// topic key, so anything canonical.Asset.String() could not produce is forged.
func validateCanonicalAsset(field, raw string) error {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a canonical asset: %w", field, raw, err)
	}
	if a.String() != raw {
		return fmt.Errorf("%s %q is not in canonical form (want %q)", field, raw, a.String())
	}
	return nil
}

// canonicalValueDecimal is the shape the aggregator's formatRatFixed emits: optional '-', digits, optional '.' and digits. The
// allowlist is needed because big.Rat.SetString also accepts fractions, exponents, '+', hex, binary, octal and underscores.
var canonicalValueDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// parseValueDecimal validates that s is a canonical, strictly-positive, in-range fixed-point decimal and returns an exact big.Rat
// (ADR-0003: no float64 ever touches a price value).
func parseValueDecimal(s string) (*big.Rat, error) {
	if s == "" {
		return nil, errors.New("empty value_decimal")
	}
	if !canonicalValueDecimal.MatchString(s) {
		return nil, fmt.Errorf("non-canonical value_decimal %q", s)
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("unparseable value_decimal %q", s)
	}
	if v.Sign() <= 0 {
		return nil, fmt.Errorf("non-positive value_decimal %q", s)
	}
	if v.Cmp(maxValueDecimal) >= 0 {
		return nil, fmt.Errorf("out-of-range value_decimal %q", s)
	}
	return v, nil
}

// topicForPair returns the Hub topic key for an (asset, quote, window) triple, mirroring `internal/api/v1.PriceStreamTopic` (a sentinel
// test keeps them in sync);
// the window is in the key so per-window publishes don't interleave on one topic.
func topicForPair(asset, quote string, windowSeconds int64) string {
	return "closed:" + asset + "/" + quote + "/" + strconv.FormatInt(windowSeconds, 10)
}
