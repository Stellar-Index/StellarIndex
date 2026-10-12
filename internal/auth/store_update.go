package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ErrKeyNotFound is returned by [RedisAPIKeyStore.UpdateRateLimit] when no key with the supplied KeyID exists.
var ErrKeyNotFound = errors.New("auth: key_id not found")

// UpdateRateLimit sets the per-minute budget of the key with the public KeyID, resolved via the KeyID index; returns the updated record, ErrKeyNotFound, or a wrapped Redis error.
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
	// KeepTTL: this read-modify-write may touch a mirrored record carrying the sliding idle TTL; a bare `SET ... 0` would clear it and make the key permanent on the first rate-limit change.
	if err := s.rdb.Set(ctx, k, body, redis.KeepTTL).Err(); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: redis set %s: %w", k, err)
	}
	return rec, nil
}
