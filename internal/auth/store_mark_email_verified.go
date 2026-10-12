// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// MarkEmailVerified sets a key's EmailVerifiedAt to now (or optional `at`, for tests), which RequireEmailVerified gates on; idempotent, returns the updated record or ErrKeyNotFound.
func (s *RedisAPIKeyStore) MarkEmailVerified(ctx context.Context, keyID string, at time.Time) (APIKeyRecord, error) {
	if keyID == "" {
		return APIKeyRecord{}, errors.New("auth: MarkEmailVerified: keyID is required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}

	hash, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: MarkEmailVerified: %w", err)
	}
	if !found {
		return APIKeyRecord{}, ErrKeyNotFound
	}

	rec.EmailVerifiedAt = at
	body, err := json.Marshal(rec)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: MarkEmailVerified: marshal: %w", err)
	}
	k := cachekeys.APIKey(hash).String()
	// KeepTTL, as in UpdateRateLimit: a bare `SET ... 0` would turn the FIRST verification click into a permanent credential by clearing a mirrored record's sliding idle TTL.
	if err := s.rdb.Set(ctx, k, body, redis.KeepTTL).Err(); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: MarkEmailVerified: redis set %s: %w", k, err)
	}
	return rec, nil
}
