package accounterasure

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// ExportStore is the Postgres half of an export; *postgresstore.AccountStore
// satisfies it.
type ExportStore interface {
	ExportAccount(
		ctx context.Context, accountID, requester uuid.UUID, now time.Time, extra postgresstore.ExtraKeysFunc,
	) (platform.AccountExport, error)
}

// Exporter builds an account's data export: the Postgres document plus
// the self-service keys only the Redis validator store holds.
type Exporter struct {
	Store ExportStore
	Redis redis.Cmdable
}

// Export returns the document for requester, an owner of accountID.
func (x *Exporter) Export(ctx context.Context, accountID, requester uuid.UUID, now time.Time) (platform.AccountExport, error) {
	var extra postgresstore.ExtraKeysFunc
	if x.Redis != nil {
		extra = x.redisKeys
	}
	return x.Store.ExportAccount(ctx, accountID, requester, now, extra)
}

// redisKeys lists the self-service keys the Redis validator store holds
// for slug.
func (x *Exporter) redisKeys(ctx context.Context, slug string) ([]platform.ExportAPIKey, error) {
	recs, err := auth.NewRedisAPIKeyStore(x.Redis).ListKeysForIdentifier(ctx, auth.AccountIdentifier(slug))
	if err != nil {
		return nil, fmt.Errorf("account export: redis keys: %w", err)
	}
	out := make([]platform.ExportAPIKey, 0, len(recs))
	for _, r := range recs {
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
		out = append(out, k)
	}
	return out, nil
}
