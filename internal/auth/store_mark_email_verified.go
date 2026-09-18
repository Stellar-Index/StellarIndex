// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// MarkEmailVerified flips an existing API key's
// `EmailVerifiedAt` timestamp to now (or to the optional
// `at` override, used by tests for determinism). F-1218 wave 45
// (codex audit-2026-05-12): the `/v1/signup/verify` handler
// calls this after Consume so the optional `RequireEmailVerified`
// middleware can gate /v1/* access on the flag.
//
// Implementation mirrors `UpdateRateLimit`: resolve the KeyID through
// the index ([RedisAPIKeyStore.findRecordByKeyID]), then
// read-modify-write the JSON record back.
//
// Idempotent: re-marking an already-verified key updates the
// timestamp to the new value but doesn't error. The verify
// handler relies on this so a customer clicking the link twice
// in 24h doesn't get a 500.
//
// Returns the updated record on success or [ErrKeyNotFound]
// if no matching key exists.
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
	if err := s.rdb.Set(ctx, k, body, 0).Err(); err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: MarkEmailVerified: redis set %s: %w", k, err)
	}
	return rec, nil
}
