package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// MirroredKey is an already-minted credential written into the Redis validator store under a caller-generated plaintext; Record is written as given, so a mirror can never widen a key.
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

// APIKeyRecordFromPlatform maps a Postgres management row onto the Redis validator record field-for-field; k.MonthlyQuota must be resolved (0 means UNMETERED there), so a zero is refused rather than shipping an uncapped key.
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

// CreateWithSecret writes an already-minted credential into the Redis validator store with the key layout of [RedisAPIKeyStore.Create]; PermissionsAll=false with no allow entries is the closed posture (403 everything).
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
	// Write with the sliding idle TTL rather than 0: the register mirror is the open-registration growth vector, and the validator re-warms the TTL on each Lookup so only abandoned keys age out.
	// Indexed in the same atomic write as the record, as Create does: POST /v1/register rolls back a failed registration through RevokeKeyByID, which finds the record via the index.
	if err := s.writeRecord(ctx, hash, rec, body, MirroredKeyIdleTTL); err != nil {
		return fmt.Errorf("auth: CreateWithSecret: redis set: %w", err)
	}
	return nil
}
