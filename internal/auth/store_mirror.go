package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// MirroredKey is an already-minted credential being written into the
// Redis validator store under a plaintext the CALLER generated.
//
// Why this exists (v0.32.0 post-deploy, 2026-08-12): a deployment can
// run the Redis validator (r1: `backend=redis`) while an issuance path
// records its management row in Postgres. A key that exists only in
// Postgres 401s the moment it is used — POST /v1/register shipped a
// well-formed key that never authenticated. [RedisAPIKeyStore.Create]
// cannot serve that path because it GENERATES the secret; mirroring
// requires writing the caller's secret verbatim so one plaintext
// validates on either backend.
type MirroredKey struct {
	// Plaintext is the caller-generated secret (`sip_<64hex>`). The
	// store writes only its SHA-256; the plaintext is never persisted.
	Plaintext string
	// KeyID must match the management row's id so revocation and
	// listings line up across the two stores.
	KeyID string
	// Identifier is the owner reference — use [AccountIdentifier].
	Identifier string
	Label      string
	// RateLimitPerMin: zero means the tier default.
	RateLimitPerMin int
	// MonthlyQuota is the per-key monthly request cap. MUST be set
	// explicitly: the quota middleware treats <= 0 as "unmetered" and
	// short-circuits (middleware/monthly_quota.go), so omitting this
	// silently ships an UNLIMITED key — the register endpoint's
	// response and the public docs both advertise a cap, and the
	// mirrored record is what the deployed validator actually reads.
	// (Audit 2026-08-13 F1: this field did not exist, and every
	// registered key was unmetered in production.)
	MonthlyQuota int64
}

// CreateWithSecret writes an already-minted credential into the Redis
// validator store. Same record shape and key layout as
// [RedisAPIKeyStore.Create] — including PermissionsAll, without which
// the permission middleware 403s every request from the key.
func (s *RedisAPIKeyStore) CreateWithSecret(ctx context.Context, k MirroredKey) error {
	switch {
	case k.Plaintext == "":
		return errors.New("auth: CreateWithSecret: Plaintext is required")
	case k.KeyID == "":
		return errors.New("auth: CreateWithSecret: KeyID is required")
	case k.Identifier == "":
		return errors.New("auth: CreateWithSecret: Identifier is required")
	}

	rec := APIKeyRecord{
		KeyID:           k.KeyID,
		Identifier:      k.Identifier,
		Label:           k.Label,
		KeyPrefix:       keyPrefix(k.Plaintext),
		Tier:            TierAPIKey,
		RateLimitPerMin: k.RateLimitPerMin,
		MonthlyQuota:    k.MonthlyQuota,
		CreatedAt:       s.now().UTC(),
		PermissionsAll:  true,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth: CreateWithSecret: marshal record: %w", err)
	}
	hash := hashAPIKey(k.Plaintext)
	// Write with the sliding idle TTL rather than 0 (no-expiry): the
	// register mirror is the open-registration growth vector, so its
	// records must not accumulate forever in the allkeys-lru pool. The
	// validator re-warms this TTL on every successful Lookup
	// ([RedisAPIKeyValidator.refreshIdleTTL]), so a key that is actually
	// used never expires — only an abandoned one ages out
	// (W1-flow-register-2).
	if err := s.rdb.Set(ctx, cachekeys.APIKey(hash).String(), body, MirroredKeyIdleTTL).Err(); err != nil {
		return fmt.Errorf("auth: CreateWithSecret: redis set: %w", err)
	}
	return nil
}
