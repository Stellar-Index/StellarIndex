package redispub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// RedisPublisher is the subset of the Redis client surface
// [Publisher] needs. Declared as an interface so tests can
// substitute miniredis without pulling the full UniversalClient.
type RedisPublisher interface {
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
}

// Publisher implements
// [github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator.StreamPublisher]
// by encoding each closed-bucket event as JSON and PUBLISHing it
// to a configurable Redis channel.
//
// Goroutine-safe: fields are read-only after construction; the
// underlying RedisPublisher is concurrent-safe by contract.
type Publisher struct {
	cache    RedisPublisher
	channel  string
	producer string
}

// NewPublisher constructs a Publisher writing to the given Redis
// channel. Empty channel falls back to [DefaultChannel].
func NewPublisher(cache RedisPublisher, channel string) (*Publisher, error) {
	if cache == nil {
		return nil, errors.New("redispub: RedisPublisher is required")
	}
	if channel == "" {
		channel = DefaultChannel
	}
	return &Publisher{cache: cache, channel: channel, producer: newProducerID()}, nil
}

// newProducerID returns host-pid-random: stable for the process, and
// distinct across a restart overlap on the same host.
func newProducerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var nonce [4]byte
	_, _ = rand.Read(nonce[:]) // crypto/rand.Read never returns an error
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(nonce[:]))
}

// Channel returns the Redis channel this Publisher writes to.
// Useful in startup logs and matching the Subscriber's channel.
func (p *Publisher) Channel() string { return p.channel }

// ProducerID returns the id stamped on every event this Publisher sends.
func (p *Publisher) ProducerID() string { return p.producer }

// PublishClosedBucket implements
// `orchestrator.StreamPublisher.PublishClosedBucket`. JSON-marshals
// the event and PUBLISHes to the configured channel. Returns the
// underlying Redis error wrapped on failure; callers
// (orchestrator) log + continue, since the next tick retries.
func (p *Publisher) PublishClosedBucket(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	valueDecimal string,
	observedAt time.Time,
) error {
	ev := ClosedBucketEvent{
		Asset:         pair.Base.String(),
		Quote:         pair.Quote.String(),
		WindowSeconds: int64(window / time.Second),
		ValueDecimal:  valueDecimal,
		ObservedAt:    observedAt.UTC(),
		ProducerID:    p.producer,
	}
	body, err := json.Marshal(ev)
	if err != nil {
		// JSON encoding of a fully-typed struct should never fail;
		// defensive for static analysis only.
		return fmt.Errorf("redispub: marshal event: %w", err)
	}
	if err := p.cache.Publish(ctx, p.channel, body).Err(); err != nil {
		return fmt.Errorf("redispub: publish %s: %w", p.channel, err)
	}
	return nil
}
