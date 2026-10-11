package v1

import (
	"context"
	"encoding/hex"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SelfServiceKeyManager is the v1 boundary onto the Redis-backed self-service key store (keys
// minted through POST /v1/account/keys), implemented by [auth.RedisAPIKeyStore].
type SelfServiceKeyManager interface {
	ListKeysForIdentifier(ctx context.Context, identifier string) ([]auth.APIKeyRecord, error)
	UpdateRateLimit(ctx context.Context, keyID string, newRateLimitPerMin int) (auth.APIKeyRecord, error)
}

// KeyMirror writes a caller-minted credential into the validator's own store.
type KeyMirror interface {
	CreateWithSecret(ctx context.Context, k auth.MirroredKey) error
	// RevokeKeyByID removes a mirrored credential by KeyID, scoped to its owner identifier. Register
	// uses it to roll back a mirror whose management row failed to commit, so no credential
	// outlives its management record.
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
}

// KeyCacheInvalidator evicts one API-key record from the auth read-through cache by its SHA-256
// hex hash.
//
// It is meaningful ONLY under auth_backend=postgres. Under the default redis backend
// `apikey:<hash>` is the canonical credential, not a cache, so an eviction there deletes the
// customer's key for good; [NewAPIKeyBudgetStores] leaves the field nil in that mode.
type KeyCacheInvalidator interface {
	InvalidateCachedKey(ctx context.Context, hexHash string) error
}

// APIKeyBudgetStores groups the credential stores a TIER CHANGE has to clamp, so every path that
// lowers a tier lowers the budget on every credential the old tier allowed.
//
// Postgres dashboard keys and Redis self-service keys are independent, and a missed store is a
// live throughput leak: both validators read the budget off the key record, so lowering
// accounts.tier alone changes nothing.
type APIKeyBudgetStores struct {
	// Platform is the Postgres dashboard key store; nil skips that half.
	Platform platform.APIKeyStore
	// Redis is the self-service key store, reached by [auth.AccountIdentifier](account.Slug); nil skips it.
	Redis SelfServiceKeyManager
	// RedisMirror writes an already-minted credential into the Redis validator store; non-nil only
	// when Redis is wired. POST /v1/register uses it so the returned key authenticates against the
	// Redis validator (a Postgres-only key 401s).
	RedisMirror KeyMirror
	// CacheInvalidator evicts each lowered Postgres key from the auth cache so the new budget
	// applies on the next request, not after the ~1h TTL. Nil is REQUIRED under auth_backend=redis
	// (see [KeyCacheInvalidator]); build via [NewAPIKeyBudgetStores].
	CacheInvalidator KeyCacheInvalidator
	// OnError, when non-nil, receives the failing step (list_keys | key_update | key_cache_invalidate).
	OnError func(step string)
}

// NewAPIKeyBudgetStores assembles the credential stores for one deployment; it is the single place
// that decides, from `[api].auth_backend`, whether a key-cache invalidator exists. platformKeys
// and rdb are nil without Postgres and Redis respectively.
//
// The invalidator is wired ONLY under auth_backend=postgres. Under redis it would DEL the canonical
// credential and an admin PATCH would permanently destroy every register-minted key (only a hash is
// kept). Nothing needs evicting there: the clamp rewrites the record in place.
func NewAPIKeyBudgetStores(platformKeys platform.APIKeyStore, rdb redis.Cmdable, authBackend string) APIKeyBudgetStores {
	var st APIKeyBudgetStores
	if platformKeys != nil {
		st.Platform = platformKeys
	}
	if rdb == nil {
		return st
	}
	redisKeys := auth.NewRedisAPIKeyStore(rdb)
	st.Redis = redisKeys
	st.RedisMirror = redisKeys
	// Assign only a non-nil invalidator: a typed-nil in the interface would defeat the `== nil`
	// guards and count evictions that never happened.
	if inv := auth.NewKeyCacheInvalidatorForBackend(authBackend, rdb); inv != nil {
		st.CacheInvalidator = inv
	}
	return st
}

// note reports a step failure through the caller's own observability, if any.
func (st APIKeyBudgetStores) note(step string) {
	if st.OnError != nil {
		st.OnError(step)
	}
}

// clampKeyBudgetsToTier lowers every credential `account` can still authenticate with down to
// `ceiling`, across BOTH key stores. It returns the lowered and failed counts so the caller can
// record the outcome durably. `cause` is stamped on every log line.
//
// Best-effort and idempotent: keys at or below the ceiling and revoked keys are skipped, and one
// key's failure never stops the others, since leaving over-tier throughput live is worse.
func (s *Server) clampKeyBudgetsToTier(
	ctx context.Context,
	cause string,
	st APIKeyBudgetStores,
	account platform.Account,
	ceiling int,
) (lowered, failed int) {
	pl, pf := s.downgradePlatformAPIKeys(ctx, cause, st, account, ceiling)
	rl, rf := s.downgradeRedisAPIKeys(ctx, cause, st, account, ceiling)
	lowered, failed = pl+rl, pf+rf
	// A clamp silently cuts throughput (the key starts 429-ing sooner); this is the one chokepoint
	// that sees every clamp, and `failed` signals over-tier throughput left live.
	if lowered > 0 {
		obs.AdminKeyBudgetClampsTotal.WithLabelValues("lowered").Add(float64(lowered))
	}
	if failed > 0 {
		obs.AdminKeyBudgetClampsTotal.WithLabelValues("failed").Add(float64(failed))
	}
	return lowered, failed
}

// downgradePlatformAPIKeys lowers every active Postgres dashboard key for `account` whose
// per-minute budget exceeds `ceiling`, then evicts it from the auth cache. Idempotent; revoked
// keys are left alone.
func (s *Server) downgradePlatformAPIKeys(
	ctx context.Context,
	cause string,
	st APIKeyBudgetStores,
	account platform.Account,
	ceiling int,
) (lowered, failed int) {
	if st.Platform == nil {
		return 0, 0
	}
	keys, err := st.Platform.ListActiveForAccount(ctx, account.ID)
	if err != nil {
		st.note("list_keys")
		s.logger.Error("tier clamp: ListActiveForAccount failed; platform-backed keys keep the OLD budget",
			"cause", cause, "account_id", account.ID, "err", err)
		return 0, 1
	}
	for i := range keys {
		k := keys[i]
		if !k.RevokedAt.IsZero() || k.RateLimitPerMin <= ceiling {
			continue
		}
		k.RateLimitPerMin = ceiling
		if err := st.Platform.Update(ctx, account.ID, k); err != nil {
			st.note("key_update")
			s.logger.Error("tier clamp: platform-key downgrade Update failed",
				"cause", cause, "account_id", account.ID,
				"key_id", k.ID, "err", err)
			failed++
			continue
		}
		lowered++
		s.invalidatePlatformKeyCache(ctx, cause, st, account, k)
	}
	if lowered > 0 {
		s.logger.Info("tier clamp: lowered platform-backed dashboard keys to the new tier budget",
			"cause", cause, "account_id", account.ID,
			"tier", string(account.Tier), "rate_limit_per_min", ceiling,
			"keys_lowered", lowered)
	}
	return lowered, failed
}

// downgradeRedisAPIKeys lowers every Redis key the account minted through POST /v1/account/keys
// down to `ceiling`. Those records carry [auth.AccountIdentifier](slug) and inherit the caller's
// budget at mint time. No-op without a Redis store or a slug.
func (s *Server) downgradeRedisAPIKeys(
	ctx context.Context,
	cause string,
	st APIKeyBudgetStores,
	account platform.Account,
	ceiling int,
) (lowered, failed int) {
	if st.Redis == nil || account.Slug == "" {
		return 0, 0
	}
	identifier := auth.AccountIdentifier(account.Slug)
	keys, err := st.Redis.ListKeysForIdentifier(ctx, identifier)
	if err != nil {
		st.note("list_keys")
		s.logger.Error("tier clamp: ListKeysForIdentifier failed; redis-backed keys keep the OLD budget",
			"cause", cause, "identifier", identifier, "err", err)
		return 0, 1
	}
	for _, k := range keys {
		if !k.RevokedAt.IsZero() || k.RateLimitPerMin <= ceiling {
			continue
		}
		if _, err := st.Redis.UpdateRateLimit(ctx, k.KeyID, ceiling); err != nil {
			st.note("key_update")
			s.logger.Error("tier clamp: redis-key downgrade failed",
				"cause", cause, "identifier", identifier,
				"key_id", k.KeyID, "err", err)
			failed++
			continue
		}
		lowered++
	}
	if lowered > 0 {
		s.logger.Info("tier clamp: lowered redis-backed self-service keys to the new tier budget",
			"cause", cause, "identifier", identifier,
			"rate_limit_per_min", ceiling, "keys_lowered", lowered)
	}
	return lowered, failed
}

// invalidatePlatformKeyCache evicts a just-updated Postgres key from the auth read-through cache.
// No-op when no invalidator is wired or the row has no hash. A failure is logged and counted
// under `key_cache_invalidate` but not surfaced: the write succeeded, so the worst case is the
// old budget until the cache TTL elapses.
func (s *Server) invalidatePlatformKeyCache(ctx context.Context, cause string, st APIKeyBudgetStores, account platform.Account, k platform.APIKey) {
	if st.CacheInvalidator == nil || len(k.KeyHash) == 0 {
		return
	}
	if err := st.CacheInvalidator.InvalidateCachedKey(ctx, hex.EncodeToString(k.KeyHash)); err != nil {
		st.note("key_cache_invalidate")
		s.logger.Warn("tier clamp: auth cache invalidate after key budget change failed",
			"cause", cause, "account_id", account.ID,
			"key_id", k.ID, "err", err)
	}
}
