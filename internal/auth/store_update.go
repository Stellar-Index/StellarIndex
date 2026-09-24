package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ErrKeyNotFound is returned by [RedisAPIKeyStore.UpdateRateLimit]
// when no key with the supplied KeyID was found in Redis. Operators
// see this when they typo a KeyID on the upgrade-key CLI.
var ErrKeyNotFound = errors.New("auth: key_id not found")

// UpdateRateLimit lifts (or lowers) the per-minute rate-limit budget
// of an existing API key, identified by its public KeyID. Used by:
//
//   - `stellarindex-ops upgrade-key` (operator-side manual tier
//     changes).
//   - The admin tier-clamp path, which lowers every key an account
//     holds when its tier ceiling drops.
//
// The record is resolved through the KeyID index
// ([RedisAPIKeyStore.findRecordByKeyID]) — one index read and one GET —
// and only by walking the keyspace while that index is unusable.
//
// Returns the updated record (with the new RateLimitPerMin)
// and nil on success, or [ErrKeyNotFound] if no matching key
// exists, or a wrapped error on Redis I/O failure.
func (s *RedisAPIKeyStore) UpdateRateLimit(ctx context.Context, keyID string, newRateLimitPerMin int) (APIKeyRecord, error) {
	if keyID == "" {
		return APIKeyRecord{}, errors.New("auth: UpdateRateLimit: keyID is required")
	}
	if err := ValidateKeyBounds(newRateLimitPerMin, nil); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: %w", err)
	}

	hash, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: %w", err)
	}
	if !found {
		return APIKeyRecord{}, ErrKeyNotFound
	}

	// Apply the new rate-limit + write back.
	rec.RateLimitPerMin = newRateLimitPerMin
	body, err := json.Marshal(rec)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: marshal: %w", err)
	}
	k := cachekeys.APIKey(hash).String()
	// KeepTTL (Q186): this is a read-modify-write on a record that may
	// carry the register-mirror's sliding idle TTL
	// ([MirroredKeyIdleTTL]). A bare `SET ... 0` clears any existing TTL,
	// turning a bounded-lifetime mirrored key permanent the first time an
	// operator rate-limit change touches it — silently defeating the
	// idle-expiry that bounds open-registration keyspace growth.
	if err := s.rdb.Set(ctx, k, body, redis.KeepTTL).Err(); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: redis set %s: %w", k, err)
	}
	return rec, nil
}
