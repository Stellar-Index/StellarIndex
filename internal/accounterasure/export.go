package accounterasure

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// ExportStore is the Postgres half of an export; *postgresstore.AccountStore
// satisfies it.
type ExportStore interface {
	ExportAccount(ctx context.Context, accountID, requester uuid.UUID, now time.Time) (platform.AccountExport, error)
}

// Exporter builds an account's data export: the Postgres document plus
// the self-service keys only the Redis validator store holds.
type Exporter struct {
	Store ExportStore
	Redis redis.Cmdable
}

// Export returns the document for requester, an owner of accountID.
func (x *Exporter) Export(ctx context.Context, accountID, requester uuid.UUID, now time.Time) (platform.AccountExport, error) {
	doc, err := x.Store.ExportAccount(ctx, accountID, requester, now)
	if err != nil || x.Redis == nil {
		return doc, err
	}
	recs, err := auth.NewRedisAPIKeyStore(x.Redis).ListKeysForIdentifier(ctx, auth.AccountIdentifier(doc.Account.Slug))
	if err != nil {
		return platform.AccountExport{}, fmt.Errorf("account export: redis keys: %w", err)
	}
	seen := make(map[string]bool, len(doc.APIKeys))
	for _, k := range doc.APIKeys {
		seen[k.ID] = true
	}
	for _, r := range recs {
		if seen[r.KeyID] {
			continue // a Postgres key mirrored into Redis
		}
		k := platform.ExportAPIKey{
			ID: r.KeyID, Store: "redis", Prefix: r.KeyPrefix, Name: r.Label, Tier: string(r.Tier),
			Scopes: r.Scopes, RateLimitPerMin: r.RateLimitPerMin, MonthlyQuota: r.MonthlyQuota,
			IPAllowlist: r.IPAllowlist, RefererAllowlist: r.RefererAllowlist, CreatedAt: r.CreatedAt.UTC(),
		}
		if k.Scopes == nil {
			k.Scopes = []string{}
		}
		if !r.ExpiresAt.IsZero() {
			t := r.ExpiresAt.UTC()
			k.ExpiresAt = &t
		}
		if !r.RevokedAt.IsZero() {
			t := r.RevokedAt.UTC()
			k.RevokedAt = &t
		}
		doc.APIKeys = append(doc.APIKeys, k)
	}
	return doc, nil
}
