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
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// RedisSubscriber is the subset of the Redis client surface
// [Subscriber] needs. Declared as an interface so tests can
// substitute miniredis without pulling the full UniversalClient.
type RedisSubscriber interface {
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

// Hub is the subset of [streaming.Hub] the subscriber needs.
// Declared as an interface so tests can substitute a recorder
// without spinning up the full Hub.
type Hub interface {
	Publish(topic, eventType string, data []byte) string
}

// Subscriber listens on the Redis channel the [Publisher] writes
// to (see [DefaultChannel]) and republishes each
// [ClosedBucketEvent] on the supplied Hub. The matching SSE
// topic key is `closed:<asset>/<quote>` — same format as
// `internal/api/v1.PriceStreamTopic`.
//
// Goroutine-safe: fields are read-only after construction.
type Subscriber struct {
	cache   RedisSubscriber
	channel string
	hub     Hub
	logger  *slog.Logger
}

// NewSubscriber constructs a Subscriber bound to the given Redis
// channel + Hub. Empty channel falls back to [DefaultChannel].
// nil logger falls back to [slog.Default].
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
	return &Subscriber{cache: cache, channel: channel, hub: hub, logger: logger}, nil
}

// Outcome labels for [obs.APIStreamSubscribeTotal]. Each rejection
// cause that points a responder somewhere different gets its own label.
const (
	outcomeOK               = "ok"
	outcomeDecodeError      = "decode_error"
	outcomeMalformed        = "malformed"
	outcomeFutureObservedAt = "future_observed_at"
	outcomeStaleObservedAt  = "stale_observed_at"
)

var subscribeOutcomes = []string{
	outcomeOK, outcomeDecodeError, outcomeMalformed, outcomeFutureObservedAt, outcomeStaleObservedAt,
}

// Sentinels validateEvent wraps so handleMessage can label the drop by
// cause: a clock-skewed API host rejects EVERY event as future-dated,
// and must not read as a wire-format bug.
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

// Run blocks until ctx is cancelled, consuming messages from the
// Redis channel and republishing each one on the Hub. Returns
// ctx.Err() on clean shutdown.
//
// A Redis failure does NOT end Run: go-redis v9 retries a refused,
// dropped or killed pubsub connection internally and closes the
// message channel only when the PubSub itself is closed. An outage
// therefore shows as a flat `ok` outcome on
// stellarindex_api_stream_subscribe_total while the aggregator keeps
// publishing, which stellarindex_api_price_stream_not_delivering
// alerts on; the channel-closed error below is a backstop.
//
// One Subscriber per binary; safe to invoke as a long-lived
// goroutine. The matching `cmd/stellarindex-api/main.go` wiring
// runs Run inside an errgroup alongside the HTTP server.
func (s *Subscriber) Run(ctx context.Context) error {
	pubsub := s.cache.Subscribe(ctx, s.channel)
	defer func() {
		if err := pubsub.Close(); err != nil {
			s.logger.Warn("redispub: pubsub close", "err", err)
		}
	}()

	ch := pubsub.Channel()
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

// handleMessage decodes one wire payload, validates it, and
// republishes the CANONICAL re-marshalled event on the Hub. JSON-decode
// failures log + increment the error metric; they're never propagated
// since one bad message must not stop the subscriber from processing
// the next.
//
// Defense-in-depth (F2): r1's Redis has no AUTH (network isolation is
// the primary control), so a host-adjacent process could PUBLISH a
// forged closed-bucket event onto the channel. Before this event
// reaches SSE clients we (1) bound every field — a non-numeric,
// non-positive, or absurd VWAP, an out-of-range window, or a
// missing/future/stale timestamp is dropped, not fanned out — and
// (2) fan out a re-marshal of the VALIDATED struct rather than the raw
// incoming bytes, so injected extra JSON fields cannot ride along to
// clients.
func (s *Subscriber) handleMessage(payload []byte) {
	var ev ClosedBucketEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeDecodeError).Inc()
		s.logger.Warn("redispub: decode message", "err", err, "payload_len", len(payload))
		return
	}
	if err := validateEvent(&ev, time.Now()); err != nil {
		outcome := rejectionOutcome(err)
		obs.APIStreamSubscribeTotal.WithLabelValues(outcome).Inc()
		s.logger.Warn("redispub: rejected event (failed validation)",
			"outcome", outcome, "err", err, "asset", ev.Asset, "quote", ev.Quote)
		return
	}
	// Fan out the DOCUMENTED envelope shape built from the validated
	// struct (cold audit 2026-08-03: the raw ClosedBucketEvent shape
	// shared zero field names with the /price/stream OpenAPI example,
	// and the two producers emitted incompatible shapes). Building from
	// the validated struct also keeps the sanitization property: any
	// attacker-injected extra fields in the raw payload are dropped
	// here.
	//
	// as_of is ObservedAt: the end of the closed 1-minute bucket the
	// orchestrator decided the window at (refreshPairWindow), over
	// [ObservedAt-window, ObservedAt). Deriving it from the event rather
	// than the local clock keeps every subscriber's payload byte-identical
	// (ADR-0015), as streampublish does for its 60 s series.
	sanitized, err := json.Marshal(closedBucketEnvelope{
		Data: closedBucketWireData{
			AssetID:       ev.Asset,
			Quote:         ev.Quote,
			Price:         ev.ValueDecimal,
			PriceType:     "vwap",
			ObservedAt:    ev.ObservedAt,
			WindowSeconds: ev.WindowSeconds,
		},
		AsOf: ev.ObservedAt,
	})
	if err != nil {
		// Marshalling a fully-typed, already-decoded struct cannot
		// fail; defensive for static analysis only.
		obs.APIStreamSubscribeTotal.WithLabelValues(outcomeMalformed).Inc()
		s.logger.Warn("redispub: re-marshal validated event", "err", err)
		return
	}
	topic := topicForPair(ev.Asset, ev.Quote, ev.WindowSeconds)
	s.hub.Publish(topic, "price_update", sanitized)
	obs.APIStreamSubscribeTotal.WithLabelValues(outcomeOK).Inc()
}

// closedBucketEnvelope is the SSE wire shape fanned out to
// /v1/price/stream subscribers — field-compatible with the /v1/price
// response envelope and the endpoint's OpenAPI example. No `flags`
// object is emitted on this path: the aggregator's pub/sub event
// doesn't carry stale/frozen verdicts, and fabricating `stale: false`
// would turn missing data into a false claim — clients treat absent
// flags as "not evaluated".
type closedBucketEnvelope struct {
	Data closedBucketWireData `json:"data"`
	AsOf time.Time            `json:"as_of"`
}

type closedBucketWireData struct {
	AssetID       string    `json:"asset_id"`
	Quote         string    `json:"quote"`
	Price         string    `json:"price"`
	PriceType     string    `json:"price_type"`
	ObservedAt    time.Time `json:"observed_at"`
	WindowSeconds int64     `json:"window_seconds"`
}

const (
	// maxWindowSeconds bounds ClosedBucketEvent.WindowSeconds. The
	// aggregator's default windows are 5m/1h/24h but operators may
	// override them via `[aggregate].windows`; the API subscriber
	// cannot see that config, so it bounds by sanity (positive, under a
	// year) rather than an allowlist that would silently drop a
	// legitimately-reconfigured window.
	maxWindowSeconds = int64(366 * 24 * 60 * 60)

	// observedAtFutureSkew is how far ahead of local time a bucket-end
	// timestamp may sit before we treat it as forged — absorbs clock
	// skew between the aggregator and API hosts.
	observedAtFutureSkew = 5 * time.Minute

	// observedAtMaxAge bounds how stale a closed-bucket event may be.
	// ObservedAt is the window's END — the last closed minute, not the
	// window's start — so even the 24h window lands within a minute of
	// real time; a day of slack absorbs delivery lag without admitting
	// an ancient replayed price.
	observedAtMaxAge = 24 * time.Hour
)

// maxValueDecimal is the exclusive upper bound on a published VWAP.
// 10^18 is astronomically beyond any real quote-per-base price yet
// still rejects overflow-style garbage. Range validation is
// defense-in-depth, not a substitute for the network isolation that is
// the primary control against a plausible-but-forged price.
var maxValueDecimal = new(big.Rat).SetInt(
	new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
)

// validateEvent rejects any decoded ClosedBucketEvent that a trusted
// aggregator would never have produced. Returns nil when the event is
// safe to fan out.
func validateEvent(ev *ClosedBucketEvent, now time.Time) error {
	if ev.Asset == "" || ev.Quote == "" {
		return errors.New("empty asset or quote")
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
	if _, err := parseValueDecimal(ev.ValueDecimal); err != nil {
		return err
	}
	return nil
}

// canonicalValueDecimal is the exact shape the aggregator's
// formatRatFixed emits: an optional leading '-', one or more digits,
// and an optional '.' plus more digits. Allowlisting this shape (rather
// than blacklisting the forms big.Rat.SetString also accepts) is the
// fix: SetString parses fractions ("1/2"), scientific notation ("1e9"),
// an explicit leading '+', and — as of Go's arbitrary-precision literal
// support — hex ("0x1p4"), binary ("0b101"), octal ("0o17") and
// underscore-separated ("1_000.5") forms, none of which the aggregator
// ever produces.
var canonicalValueDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// parseValueDecimal validates that s is a canonical, strictly-positive,
// in-range fixed-point decimal — the shape the aggregator's
// formatRatFixed emits — and returns it as an exact big.Rat (ADR-0003:
// no float64 ever touches a price value).
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

// topicForPair returns the Hub topic key for a (asset, quote, window)
// triple. Mirrors `internal/api/v1.PriceStreamTopic` — a sentinel
// test in this package's test suite verifies the format stays in
// sync. The window is part of the key so the aggregator's per-window
// publishes (r1: 5m/1h/24h) land on separate topics instead of
// interleaving on one (cold audit 2026-08-03, r1-confirmed).
func topicForPair(asset, quote string, windowSeconds int64) string {
	return "closed:" + asset + "/" + quote + "/" + strconv.FormatInt(windowSeconds, 10)
}
