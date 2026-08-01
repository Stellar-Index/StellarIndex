package redispub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
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
	return &Subscriber{cache: cache, channel: channel, hub: hub, logger: logger}, nil
}

// Channel returns the Redis channel this Subscriber listens on.
func (s *Subscriber) Channel() string { return s.channel }

// Run blocks until ctx is cancelled, consuming messages from the
// Redis channel and republishing each one on the Hub. Returns
// ctx.Err() on clean shutdown; any unexpected stream-end is
// surfaced as an error so the caller can log and decide whether
// to retry.
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
				// go-redis closes the channel when the underlying
				// pubsub disconnects irrecoverably. Surface it so the
				// caller can restart.
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
		obs.APIStreamSubscribeTotal.WithLabelValues("decode_error").Inc()
		s.logger.Warn("redispub: decode message", "err", err, "payload_len", len(payload))
		return
	}
	if err := validateEvent(&ev, time.Now()); err != nil {
		obs.APIStreamSubscribeTotal.WithLabelValues("malformed").Inc()
		s.logger.Warn("redispub: rejected event (failed validation)",
			"err", err, "asset", ev.Asset, "quote", ev.Quote)
		return
	}
	// Re-marshal the validated struct so only the canonical four-field
	// shape reaches subscribers — any attacker-injected extra fields in
	// the raw payload are dropped here.
	sanitized, err := json.Marshal(&ev)
	if err != nil {
		// Marshalling a fully-typed, already-decoded struct cannot
		// fail; defensive for static analysis only.
		obs.APIStreamSubscribeTotal.WithLabelValues("malformed").Inc()
		s.logger.Warn("redispub: re-marshal validated event", "err", err)
		return
	}
	topic := topicForPair(ev.Asset, ev.Quote)
	s.hub.Publish(topic, "price_update", sanitized)
	obs.APIStreamSubscribeTotal.WithLabelValues("ok").Inc()
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
	// Bucket-end timestamps track ~now for every window (the value is
	// the bucket END, not its start), so even the 24h window lands near
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
		return fmt.Errorf("observed_at %s is in the future", ev.ObservedAt.Format(time.RFC3339))
	}
	if ev.ObservedAt.Before(now.Add(-observedAtMaxAge)) {
		return fmt.Errorf("observed_at %s is too old", ev.ObservedAt.Format(time.RFC3339))
	}
	if _, err := parseValueDecimal(ev.ValueDecimal); err != nil {
		return err
	}
	return nil
}

// parseValueDecimal validates that s is a canonical, strictly-positive,
// in-range fixed-point decimal — the shape the aggregator's
// formatRatFixed emits — and returns it as an exact big.Rat (ADR-0003:
// no float64 ever touches a price value).
func parseValueDecimal(s string) (*big.Rat, error) {
	if s == "" {
		return nil, errors.New("empty value_decimal")
	}
	// Reject the fraction ("1/2") and scientific ("1e9") forms
	// big.Rat.SetString would otherwise accept: the aggregator only
	// ever emits plain [-]?digits[.digits], so anything else is
	// injected — and rejecting them keeps the forwarded string canonical.
	if strings.ContainsAny(s, "/eE") {
		return nil, fmt.Errorf("non-decimal value_decimal %q", s)
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

// topicForPair returns the Hub topic key for a (asset, quote)
// pair. Mirrors `internal/api/v1.PriceStreamTopic` — a sentinel
// test in this package's test suite verifies the format stays
// in sync.
func topicForPair(asset, quote string) string {
	return "closed:" + asset + "/" + quote
}
