package mev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// HeartbeatCache is the Redis subset the heartbeat needs; *redis.Client satisfies it.
type HeartbeatCache interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
}

// RedisHeartbeat publishes the worker's run heartbeat (cachekeys.MEVHeartbeat) and reads it back for /v1/mev. Concurrent-safe.
type RedisHeartbeat struct {
	cache  HeartbeatCache
	logger *slog.Logger
}

// HeartbeatOptions wires a RedisHeartbeat; Logger nil means slog.Default().
type HeartbeatOptions struct {
	Cache  HeartbeatCache
	Logger *slog.Logger
}

// NewRedisHeartbeat returns a heartbeat backed by opts.Cache; a nil cache is an error.
func NewRedisHeartbeat(opts HeartbeatOptions) (*RedisHeartbeat, error) {
	if opts.Cache == nil {
		return nil, errors.New("mev: heartbeat cache is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisHeartbeat{cache: opts.Cache, logger: logger}, nil
}

// Beat records a completed run. A write failure is logged, not returned: the run happened, and the
// TTL makes the reader report the gap on its own.
func (h *RedisHeartbeat) Beat(ctx context.Context, full bool) {
	val := cachekeys.MEVHeartbeatPartial
	if full {
		val = cachekeys.MEVHeartbeatFull
	}
	err := h.cache.Set(ctx, cachekeys.MEVHeartbeat().String(), val, cachekeys.MEVHeartbeatTTL).Err()
	if err != nil && ctx.Err() == nil {
		h.logger.Warn("mev: heartbeat write failed", "err", err)
	}
}

// Pulse reads the last recorded run: present is false when no run completed within the TTL; full
// is false unless the last run resolved every ordering hash.
func (h *RedisHeartbeat) Pulse(ctx context.Context) (full, present bool, err error) {
	v, err := h.cache.Get(ctx, cachekeys.MEVHeartbeat().String()).Result()
	if errors.Is(err, redis.Nil) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("mev: heartbeat get: %w", err)
	}
	return v == cachekeys.MEVHeartbeatFull, true, nil
}
