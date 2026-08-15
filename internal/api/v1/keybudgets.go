package v1

import (
	"context"
	"encoding/hex"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SelfServiceKeyManager is the v1 boundary onto the Redis-backed
// self-service key store (keys minted through POST /v1/account/keys).
// Implementation: [auth.RedisAPIKeyStore] (which provides both
// methods). Formerly named StripeKeyManager; the Stripe webhook that
// shared this seam was removed when the platform went free
// (2026-08-10) — the operator tier-clamp path is the remaining
// caller.
type SelfServiceKeyManager interface {
	ListKeysForIdentifier(ctx context.Context, identifier string) ([]auth.APIKeyRecord, error)
	UpdateRateLimit(ctx context.Context, keyID string, newRateLimitPerMin int) (auth.APIKeyRecord, error)
}

// KeyCacheInvalidator evicts one API-key record from the runtime
// auth read-through cache by its SHA-256 hex hash. Implemented by
// both auth.RedisKeyCacheInvalidator and
// auth.PostgresAPIKeyValidator. Declared here (rather than importing
// internal/auth) so the v1 package stays free of an auth dependency
// in its exported bridge type — the same narrowing pattern as
// dashboardkeys.CacheInvalidator.
// KeyMirror writes a caller-minted credential into the validator's
// own store. Implemented by [auth.RedisAPIKeyStore].
type KeyMirror interface {
	CreateWithSecret(ctx context.Context, k auth.MirroredKey) error
	// RevokeKeyByID removes a mirrored credential by KeyID, scoped to its
	// owner identifier. The register path uses it to roll back a mirror
	// that succeeded but whose durable management row then failed to
	// commit, so no credential ever outlives its management record
	// (NS-3). Implemented by [auth.RedisAPIKeyStore.RevokeKeyByID].
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
}

type KeyCacheInvalidator interface {
	InvalidateCachedKey(ctx context.Context, hexHash string) error
}

// APIKeyBudgetStores groups the credential stores a TIER CHANGE has to
// clamp, so every path that lowers an account's tier lowers the budget
// on every credential that tier used to allow.
//
// There are two independent key stores in production and a missed one
// is a live throughput leak: Postgres-backed dashboard keys
// (platform.APIKeyStore) and Redis-backed self-service keys minted
// through POST /v1/account/keys ([SelfServiceKeyManager]). The
// enforced budget is read straight off the key record by both
// validators (auth/apikey_postgres.go, auth/apikey_redis.go), so
// lowering accounts.tier alone changes nothing the key holder can
// feel.
type APIKeyBudgetStores struct {
	// Platform is the Postgres-backed dashboard key store. Nil skips
	// that half (deployments without Postgres).
	Platform platform.APIKeyStore
	// Redis is the Redis-backed self-service key store, reached by
	// [auth.AccountIdentifier](account.Slug). Nil skips that half.
	Redis SelfServiceKeyManager
	// RedisMirror writes an already-minted credential into the Redis
	// validator store. Non-nil only when the deployment wires Redis;
	// POST /v1/register uses it so the key it hands back authenticates
	// against the REDIS validator r1 actually runs (a Postgres-only
	// key 401s — v0.32.0 post-deploy finding).
	RedisMirror KeyMirror
	// CacheInvalidator evicts each lowered Postgres key from the auth
	// read-through cache so the new budget is enforced on the next
	// request rather than after the validator's ~1h TTL. Nil is safe.
	CacheInvalidator KeyCacheInvalidator
	// OnError, when non-nil, is called with the failing step name
	// (list_keys | key_update | key_cache_invalidate) so a caller can
	// attach its OWN observability.
	OnError func(step string)
}

// note reports a step failure through the caller's own observability, if any.
func (st APIKeyBudgetStores) note(step string) {
	if st.OnError != nil {
		st.OnError(step)
	}
}

// clampKeyBudgetsToTier lowers every credential `account` can still
// authenticate with down to `ceiling`, across BOTH key stores. Returns
// how many keys were lowered and how many failed to lower, so a caller
// can record the outcome durably (the admin path writes both into its
// audit row).
//
// Best-effort and idempotent: keys already at or below the ceiling are
// skipped, revoked keys are left alone, and a failure on one key never
// stops the others — leaving over-tier throughput live is the worse
// outcome. `cause` names what triggered the clamp and is stamped on
// every log line ("admin PATCH by key …").
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
	// A clamp quietly reduces throughput the customer may still believe they
	// have — their key keeps authenticating and starts 429-ing sooner. This
	// is the one chokepoint that sees every clamp. `failed` is the operator's
	// signal that over-tier throughput stayed live past a downgrade.
	if lowered > 0 {
		obs.AdminKeyBudgetClampsTotal.WithLabelValues("lowered").Add(float64(lowered))
	}
	if failed > 0 {
		obs.AdminKeyBudgetClampsTotal.WithLabelValues("failed").Add(float64(failed))
	}
	return lowered, failed
}

// downgradePlatformAPIKeys lowers every active Postgres-backed dashboard
// key for `account` whose per-minute budget exceeds `ceiling`, evicting
// each from the auth read-through cache afterwards. Keys already at or
// below the ceiling are skipped (idempotent), and revoked keys are left
// alone.
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
	keys, err := st.Platform.ListForAccount(ctx, account.ID)
	if err != nil {
		st.note("list_keys")
		s.logger.Error("tier clamp: ListForAccount failed; platform-backed keys keep the OLD budget",
			"cause", cause, "account_id", account.ID, "err", err)
		return 0, 1
	}
	for i := range keys {
		k := keys[i]
		if !k.RevokedAt.IsZero() || k.RateLimitPerMin <= ceiling {
			continue
		}
		k.RateLimitPerMin = ceiling
		if err := st.Platform.Update(ctx, k); err != nil {
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

// downgradeRedisAPIKeys lowers every Redis-backed key the account minted
// through POST /v1/account/keys (which copies the caller's Subject
// identifier, i.e. [auth.AccountIdentifier](slug), onto the record and
// inherits the caller's then-current budget) down to `ceiling`.
// No-op without a Redis store or a slug.
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

// invalidatePlatformKeyCache evicts a just-updated Postgres key from
// the runtime auth read-through cache. No-op when no invalidator is
// wired (Redis-less / auth_backend=redis) or the row has no hash.
// A failure is logged + counted under the
// `key_cache_invalidate` operation but never surfaced — the Postgres
// write already succeeded, so the worst case is the key serving
// the old budget until the cache TTL elapses.
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
