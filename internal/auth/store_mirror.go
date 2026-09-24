package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
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
//
// Record is the full validator record, not a caller-shaped subset: every
// gate the record can express (scopes, expiry, IP/referer allowlists,
// permission entries) is written as given, so a mirror can never widen a
// key. Build it from a management row with [APIKeyRecordFromPlatform].
type MirroredKey struct {
	// Plaintext is the caller-generated secret (`sip_<64hex>`). The
	// store writes only its SHA-256; the plaintext is never persisted.
	Plaintext string
	// Record is written verbatim except KeyPrefix (always derived from
	// Plaintext), a zero Tier (defaults to [TierAPIKey]) and a zero
	// CreatedAt (stamped now). KeyID must match the management row's id
	// so revocation and listings line up across the two stores.
	Record APIKeyRecord
}

// APIKeyRecordFromPlatform maps a Postgres management row onto the Redis
// validator record field-for-field, so the mirrored key enforces exactly
// the gates the management row carries. identifier is the owner reference
// ([AccountIdentifier] of the account slug).
//
// k.MonthlyQuota must already be resolved: on the management row 0 means
// "inherit from plan", but the Redis record reads 0 as UNMETERED (the
// quota middleware short-circuits at <= 0), so a zero is refused rather
// than silently shipping an uncapped key.
func APIKeyRecordFromPlatform(k platform.APIKey, identifier string) (APIKeyRecord, error) {
	if k.MonthlyQuota <= 0 {
		return APIKeyRecord{}, fmt.Errorf("auth: APIKeyRecordFromPlatform: key %q has unresolved monthly quota %d; resolve the plan cap before mirroring", k.ID, k.MonthlyQuota)
	}
	return APIKeyRecord{
		KeyID:            k.ID,
		Identifier:       identifier,
		Label:            k.Name,
		KeyPrefix:        k.KeyPrefix,
		Tier:             pgTierToAuthTier(k.Tier),
		Scopes:           k.Scopes,
		RateLimitPerMin:  k.RateLimitPerMin,
		CreatedAt:        k.CreatedAt,
		ExpiresAt:        k.ExpiresAt,
		RevokedAt:        k.RevokedAt,
		IPAllowlist:      encodeIPAllowlist(k.IPAllowlist),
		RefererAllowlist: k.RefererAllowlist,
		PermissionsAll:   k.Permissions.All,
		AllowPermissions: convertPermissionEntries(k.Permissions.Allow),
		DenyPermissions:  convertPermissionEntries(k.Permissions.Deny),
		MonthlyQuota:     k.MonthlyQuota,
	}, nil
}

// CreateWithSecret writes an already-minted credential into the Redis
// validator store, with the same key layout as [RedisAPIKeyStore.Create].
// Permissions are the record's own: PermissionsAll=false with no allow
// entries is the permission middleware's closed posture (403 everything).
func (s *RedisAPIKeyStore) CreateWithSecret(ctx context.Context, k MirroredKey) error {
	switch {
	case k.Plaintext == "":
		return errors.New("auth: CreateWithSecret: Plaintext is required")
	case k.Record.KeyID == "":
		return errors.New("auth: CreateWithSecret: KeyID is required")
	case k.Record.Identifier == "":
		return errors.New("auth: CreateWithSecret: Identifier is required")
	}

	rec := k.Record
	rec.KeyPrefix = KeyPrefix(k.Plaintext)
	if rec.Tier == "" {
		rec.Tier = TierAPIKey
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = s.now().UTC()
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
	//
	// Indexed in the same atomic write as the record, exactly as Create
	// does: POST /v1/register rolls a failed registration back through
	// RevokeKeyByID, which finds the record through that index.
	if err := s.writeRecord(ctx, hash, rec, body, MirroredKeyIdleTTL); err != nil {
		return fmt.Errorf("auth: CreateWithSecret: redis set: %w", err)
	}
	return nil
}
