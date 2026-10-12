package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RedisAPIKeyValidator implements APIKeyValidator over `apikey:<sha256-hex>` records (plaintext keys are never stored); absent or revoked is ErrUnauthorized, expired is ErrTokenExpired, decode or Redis failure is a wrapped non-sentinel error.
type RedisAPIKeyValidator struct {
	rdb redis.Cmdable
	now func() time.Time
	// status enables the account-level kill switch when its reader is set: a record whose Identifier
	// names a platform account ([AccountIdentifier]) is rejected unless that account is active.
	status accountStatusGate

	// mirroredKeyIdleTTL is the sliding idle window re-applied to a TTL-bearing record on every
	// validated Lookup (the read-path half of [MirroredKeyIdleTTL]); zero disables refresh-on-use.
	mirroredKeyIdleTTL time.Duration
}

// accountStatusGate is the account-level kill switch both validators apply to a record naming a
// platform account. See [accountStatusGate.check].
type accountStatusGate struct {
	// accounts, when nil, disables the gate.
	accounts AccountStatusReader

	// ttl / maxStale bound the status cache below.
	ttl      time.Duration
	maxStale time.Duration

	// cache memoises the last-read status per slug, so GetBySlug runs at most once per account per
	// ttl and a Postgres blip rides out on last-known status. Populated only when accounts != nil.
	mu    sync.RWMutex
	cache map[string]cachedAccountStatus
}

// init sets the freshness window. Past it each request re-reads Postgres, and a cached status covers a
// GetBySlug error only up to a fixed multiple of it: a Postgres outage also blocks the suspension
// write itself, so riding out cannot mask a suspension an operator could have applied.
func (g *accountStatusGate) init(ttl time.Duration) {
	g.ttl = ttl
	g.maxStale = 10 * ttl
	g.cache = make(map[string]cachedAccountStatus)
}

// cachedAccountStatus is one memoised account read: the kill-switch status and budget overrides.
type cachedAccountStatus struct {
	acct platform.Account
	at   time.Time
}

// DefaultAccountStatusCacheTTL is the freshness window of the account-status cache: at most one kill-switch query per account per window.
const DefaultAccountStatusCacheTTL = 30 * time.Second

// MirroredKeyIdleTTL is the sliding idle window for register-mirrored credentials, slid on every successful Lookup so abandoned keys age out and become volatile-lru evictable.
const MirroredKeyIdleTTL = 90 * 24 * time.Hour

// AccountStatusReader is the slice of [platform.AccountStore] the Redis validator needs for suspension and rate-limit / quota overrides.
type AccountStatusReader interface {
	GetBySlug(ctx context.Context, slug string) (platform.Account, error)
}

// APIKeyRecord is the JSON shape at `apikey:<hash>`, read-only to the validator; time fields are RFC 3339 and absent means no constraint.
type APIKeyRecord struct {
	// KeyID is a public-safe identifier, distinct from the secret hash (the Redis key), so it can
	// appear in logs and /v1/account/me without leaking the credential. Stable for the key's lifetime.
	KeyID string `json:"key_id"`

	// Identifier is the owner-account reference, passed to [Subject.Identifier] and used as the
	// rate-limit bucket key for the apikey tier.
	Identifier string `json:"identifier"`

	// Label is the customer-chosen name; unused by the auth layer.
	Label string `json:"label,omitempty"`

	// KeyPrefix is the first characters of the plaintext (e.g. `sip_4f9c1d8b`): safe to log (not
	// enough to authenticate) and shown in dashboard listings. Empty on records minted without one.
	KeyPrefix string `json:"key_prefix,omitempty"`

	// Tier is the [Tier] this key authenticates as: [TierAPIKey], or [TierOperator] to unlock admin endpoints.
	Tier Tier `json:"tier"`

	// Scopes is an optional capability list; empty or absent means no restriction, not no access.
	// middleware.KeyPolicy (checkScopes) enforces them at runtime and rejects a route needing a scope
	// the key lacks with `scope-denied`; verify names against keypolicy.go's route table before issuing.
	Scopes []string `json:"scopes,omitempty"`

	// RateLimitPerMin overrides the per-tier default; zero means use the default.
	RateLimitPerMin int `json:"rate_limit_per_min,omitempty"`

	// CreatedAt is when the key was issued; /v1/account/me surfaces it.
	CreatedAt time.Time `json:"created_at,omitempty"`

	// ExpiresAt zero means never; a non-zero value in the past triggers [ErrTokenExpired].
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// RevokedAt zero means active; any non-zero value, even a future one, triggers [ErrUnauthorized].
	RevokedAt time.Time `json:"revoked_at,omitempty"`

	// IPAllowlist / RefererAllowlist / Permissions are the per-key policy fields mirrored from Postgres
	// keys, so a cache-hit Subject is policy-identical to a miss; omitting them would silently bypass
	// KeyPolicy until the TTL elapsed. IPAllowlist is CIDR text on the wire (netip.Prefix is not
	// JSON-friendly); empty or nil means no IP gate.
	IPAllowlist []string `json:"ip_allowlist,omitempty"`

	// RefererAllowlist holds exact hosts; empty or nil means no Referer gate.
	RefererAllowlist []string `json:"referer_allowlist,omitempty"`

	// PermissionsAll bypasses the per-endpoint allow/deny check (operator-style keys).
	PermissionsAll bool `json:"permissions_all,omitempty"`

	// AllowPermissions / DenyPermissions use the same (endpoint, endpoint_prefix) either-or shape as
	// platform.KeyPermissionEntry.
	AllowPermissions []SubjectPermissionEntry `json:"allow_permissions,omitempty"`
	DenyPermissions  []SubjectPermissionEntry `json:"deny_permissions,omitempty"`

	// MonthlyQuota is the per-key monthly request cap the quota middleware enforces; zero disables it.
	MonthlyQuota int64 `json:"monthly_quota,omitempty"`

	// EmailVerifiedAt is when the customer proved ownership of their signup email; zero means never.
	// The default `RequireEmailVerified` middleware answers an unverified `signup-` key with 403.
	EmailVerifiedAt time.Time `json:"email_verified_at,omitempty"`
}

// RedisOption configures a [RedisAPIKeyValidator] at construction.
type RedisOption func(*RedisAPIKeyValidator)

// WithClock overrides the time source used for expiry comparison.
func WithClock(now func() time.Time) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.now = now }
}

// WithAccountStatus wires the account-level kill switch: records whose Identifier is AccountIdentifier(slug) are rejected unless the account is active, and nil skips the check (suspension would then not stop keys on this backend).
func WithAccountStatus(accounts AccountStatusReader) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.status.accounts = accounts }
}

// WithMirroredKeyIdleTTL overrides the sliding idle window re-applied on each successful Lookup (default [MirroredKeyIdleTTL]); non-positive disables refresh-on-use.
func WithMirroredKeyIdleTTL(ttl time.Duration) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.mirroredKeyIdleTTL = ttl }
}

// NewRedisAPIKeyValidator constructs a validator reading records from rdb, which MUST be non-nil.
func NewRedisAPIKeyValidator(rdb redis.Cmdable, opts ...RedisOption) *RedisAPIKeyValidator {
	if rdb == nil {
		panic("auth: NewRedisAPIKeyValidator: rdb must not be nil")
	}
	v := &RedisAPIKeyValidator{
		rdb:                rdb,
		now:                time.Now,
		mirroredKeyIdleTTL: MirroredKeyIdleTTL,
	}
	v.status.init(DefaultAccountStatusCacheTTL)
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Lookup implements [APIKeyValidator] with one Redis GET, mapping expiry and revocation to sentinels.
func (v *RedisAPIKeyValidator) Lookup(ctx context.Context, key string) (Subject, error) {
	if key == "" {
		// The middleware short-circuits on empty, but the validator is the trust boundary: a direct
		// caller must not authenticate as the empty-string subject.
		return Subject{}, ErrUnauthorized
	}
	hash := hashAPIKey(key)
	raw, err := v.rdb.Get(ctx, cachekeys.APIKey(hash).String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return Subject{}, ErrUnauthorized
	}
	if err != nil {
		return Subject{}, fmt.Errorf("auth: apikey redis get: %w", err)
	}

	var rec APIKeyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		// A malformed record is operator-side corruption, not caller fault: wrap a non-sentinel so the
		// middleware's default branch returns 401 without detail and the log line catches it.
		return Subject{}, fmt.Errorf("auth: apikey record decode: %w", err)
	}

	if !rec.RevokedAt.IsZero() {
		return Subject{}, ErrUnauthorized
	}
	if !rec.ExpiresAt.IsZero() && !v.now().Before(rec.ExpiresAt) {
		return Subject{}, ErrTokenExpired
	}
	acct, isAccount, err := v.status.check(ctx, rec.Identifier, v.now())
	if err != nil {
		return Subject{}, err
	}

	// A malformed allowlist entry fails closed (401) on both validators; see [subjectFromRecord].
	sub, err := subjectFromRecord(rec)
	if err != nil {
		return Subject{}, fmt.Errorf("auth: apikey ip_allowlist decode: %w", err)
	}
	if isAccount {
		// Account overrides apply as on the Postgres validator: a rate-limit floor and a monthly-quota
		// ceiling over the per-key values.
		sub.RateLimitPerMin = acct.ResolveKeyRateLimitPerMin(sub.RateLimitPerMin)
		sub.MonthlyQuota = acct.ResolveKeyMonthlyQuota(sub.MonthlyQuota)
	}

	// Refresh-on-use slides a validated TTL-bearing key's idle window. It sits after every rejection
	// gate so a suspended account's key is not kept alive. Best-effort EXPIRE ... XX: it never touches
	// persistent records, and a failed refresh never rejects auth.
	v.refreshIdleTTL(ctx, hash)
	return sub, nil
}

// subjectFromRecord is the single APIKeyRecord-to-Subject mapping (recordFromSubject is its inverse)
// used by both validators, so no path can drop a field. Rejection gates stay with the caller; an
// unparseable allowlist entry is an error so the caller fails closed.
func subjectFromRecord(rec APIKeyRecord) (Subject, error) {
	ipAllowlist, err := decodeIPAllowlist(rec.IPAllowlist)
	if err != nil {
		return Subject{}, err
	}
	tier := rec.Tier
	if tier == "" {
		// An unset tier defaults to apikey; an operator key must set tier=operator explicitly.
		tier = TierAPIKey
	}
	return Subject{
		Identifier:          rec.Identifier,
		Tier:                tier,
		Scopes:              rec.Scopes,
		KeyID:               rec.KeyID,
		RateLimitPerMin:     rec.RateLimitPerMin,
		CreatedAt:           rec.CreatedAt,
		Label:               rec.Label,
		KeyPrefix:           rec.KeyPrefix,
		IPAllowlist:         ipAllowlist,
		RefererAllowlist:    rec.RefererAllowlist,
		AllowAllPermissions: rec.PermissionsAll,
		AllowPermissions:    rec.AllowPermissions,
		DenyPermissions:     rec.DenyPermissions,
		MonthlyQuota:        rec.MonthlyQuota,
		EmailVerifiedAt:     rec.EmailVerifiedAt,
		ExpiresAt:           rec.ExpiresAt,
	}, nil
}

// recordFromSubject is the inverse of [subjectFromRecord], for the Postgres validator's cache.
func recordFromSubject(sub Subject) APIKeyRecord {
	return APIKeyRecord{
		KeyID:            sub.KeyID,
		Identifier:       sub.Identifier,
		Label:            sub.Label,
		KeyPrefix:        sub.KeyPrefix,
		Tier:             sub.Tier,
		Scopes:           sub.Scopes,
		RateLimitPerMin:  sub.RateLimitPerMin,
		CreatedAt:        sub.CreatedAt,
		ExpiresAt:        sub.ExpiresAt,
		IPAllowlist:      encodeIPAllowlist(sub.IPAllowlist),
		RefererAllowlist: sub.RefererAllowlist,
		PermissionsAll:   sub.AllowAllPermissions,
		AllowPermissions: sub.AllowPermissions,
		DenyPermissions:  sub.DenyPermissions,
		MonthlyQuota:     sub.MonthlyQuota,
		EmailVerifiedAt:  sub.EmailVerifiedAt,
	}
}

// refreshIdleTTL slides a TTL-bearing record's idle window to MirroredKeyIdleTTL with EXPIRE ... XX,
// a no-op on persistent (operator-seeded) records. Errors are swallowed: the worst case is an idle key
// expiring on its existing TTL.
func (v *RedisAPIKeyValidator) refreshIdleTTL(ctx context.Context, hash string) {
	if v.mirroredKeyIdleTTL <= 0 {
		return
	}
	_ = v.rdb.ExpireXX(ctx, cachekeys.APIKey(hash).String(), v.mirroredKeyIdleTTL).Err()
}

// check enforces the account kill switch for a record naming a platform account and returns the
// account read; isAccount is false (err nil) when it does not apply. A short-TTL cache fronts
// GetBySlug, so suspension and override changes propagate within ttl. ErrNotFound (closed account)
// is ErrUnauthorized. A transport error with a cached status within maxStale rides out on it (a blip
// does not 401 customers, and a suspended account is still rejected); with no usable cache it is
// ErrAccountStatusUnavailable (retryable 503, not a misleading 401), still failing closed.
func (g *accountStatusGate) check(
	ctx context.Context, identifier string, now time.Time,
) (acct platform.Account, isAccount bool, err error) {
	if g.accounts == nil {
		return platform.Account{}, false, nil
	}
	slug, ok := strings.CutPrefix(identifier, AccountIdentifierPrefix)
	if !ok || slug == "" {
		return platform.Account{}, false, nil // signup-<hash> record: no account to check
	}

	// Fresh cache hit: serve without a Postgres read.
	if cached, ok := g.cachedStatus(slug); ok && now.Sub(cached.at) <= g.ttl {
		return cached.acct, true, statusToError(cached.acct.Status)
	}

	acct, err = g.accounts.GetBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			return platform.Account{}, false, ErrUnauthorized
		}
		// Ride out on last-known status within the staleness bound so a Postgres blip does not
		// degrade auth for every active customer.
		if cached, ok := g.cachedStatus(slug); ok && now.Sub(cached.at) <= g.maxStale {
			return cached.acct, true, statusToError(cached.acct.Status)
		}
		// No usable cached status: fail closed, but as a retryable 503, not a 401 credential rejection.
		return platform.Account{}, false,
			fmt.Errorf("auth: apikey account status %q: %w: %w", slug, ErrAccountStatusUnavailable, err)
	}

	g.storeStatus(slug, acct, now)
	return acct, true, statusToError(acct.Status)
}

// statusToError maps an account status to the auth outcome: only active authenticates.
func statusToError(status platform.AccountStatus) error {
	if status != platform.AccountActive {
		return ErrUnauthorized
	}
	return nil
}

func (g *accountStatusGate) cachedStatus(slug string) (cachedAccountStatus, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cached, ok := g.cache[slug]
	return cached, ok
}

func (g *accountStatusGate) storeStatus(slug string, acct platform.Account, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Keep only what the gate and the cascade read; no PII in the cache.
	g.cache[slug] = cachedAccountStatus{acct: platform.Account{
		Status:                      acct.Status,
		RateLimitPerMinOverride:     acct.RateLimitPerMinOverride,
		MonthlyRequestQuotaOverride: acct.MonthlyRequestQuotaOverride,
	}, at: at}
	g.evictStaleStatusLocked(at)
}

// evictStaleStatusLocked drops cache entries older than maxStale, which check never reads again
// (the ride-out gate refuses them), so keeping them only grows the map. Called with mu held.
func (g *accountStatusGate) evictStaleStatusLocked(now time.Time) {
	for slug, cached := range g.cache {
		if now.Sub(cached.at) > g.maxStale {
			delete(g.cache, slug)
		}
	}
}

// HashAPIKey returns the hex SHA-256 of key, for admin tooling that seeds records under cachekeys.APIKey(hash); Lookup derives the same hash.
func HashAPIKey(key string) string { return hashAPIKey(key) }

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Compile-time check.
var _ APIKeyValidator = (*RedisAPIKeyValidator)(nil)
