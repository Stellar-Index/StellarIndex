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
	if err := ValidateKeyBounds(newRateLimitPerMin, nil); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateRateLimit: %w", err)
	}
	return s.rewriteRecord(ctx, "UpdateRateLimit", keyID, func(rec *APIKeyRecord) {
		rec.RateLimitPerMin = newRateLimitPerMin
	})
}

// UpdateMonthlyQuota sets the monthly request cap of the key with the public KeyID; quota must be positive (0 would
// unmeter the key). Returns the updated record, ErrKeyNotFound, or a wrapped Redis error.
func (s *RedisAPIKeyStore) UpdateMonthlyQuota(ctx context.Context, keyID string, quota int64) (APIKeyRecord, error) {
	if quota <= 0 {
		return APIKeyRecord{}, fmt.Errorf("auth: UpdateMonthlyQuota: quota must be positive, got %d", quota)
	}
	return s.rewriteRecord(ctx, "UpdateMonthlyQuota", keyID, func(rec *APIKeyRecord) {
		rec.MonthlyQuota = quota
	})
}

// rewriteRecord applies mutate to the record of keyID and writes it back over the existing key only.
func (s *RedisAPIKeyStore) rewriteRecord(
	ctx context.Context, op, keyID string, mutate func(*APIKeyRecord),
) (APIKeyRecord, error) {
	if keyID == "" {
		return APIKeyRecord{}, fmt.Errorf("auth: %s: keyID is required", op)
	}
	hash, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: %s: %w", op, err)
	}
	if !found {
		return APIKeyRecord{}, ErrKeyNotFound
	}
	mutate(&rec)
	body, err := json.Marshal(rec)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: %s: marshal: %w", op, err)
	}
	k := cachekeys.APIKey(hash).String()
	// KeepTTL: a mirrored record carries the sliding idle TTL, and a bare SET would make it permanent. XX: a record
	// revoked or expired since the read must not be written back into existence.
	err = s.rdb.SetArgs(ctx, k, body, redis.SetArgs{Mode: "XX", KeepTTL: true}).Err()
	if errors.Is(err, redis.Nil) {
		return APIKeyRecord{}, ErrKeyNotFound
	}
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: %s: redis set %s: %w", op, k, err)
	}
	return rec, nil
}
