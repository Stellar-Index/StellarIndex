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

// RedisAPIKeyValidator implements APIKeyValidator over `apikey:<sha256-hex>` records (JSON APIKeyRecord); plaintext keys are never stored, so a Redis dump leaks owners and tiers but not key material.
// Lookup errors: absent or revoked_at set is ErrUnauthorized; expires_at past is ErrTokenExpired; an undecodable record or Redis I/O failure is a wrapped non-sentinel (log signal / 503).
// Safe across goroutines: each Lookup is one GET and the account-status cache has its own mutex (accountStatusGate).
type RedisAPIKeyValidator struct {
	rdb redis.Cmdable
	now func() time.Time
	// status, when its reader is set, enables the account-level kill
	// switch: a record whose Identifier names a platform account
	// ([AccountIdentifier]) is rejected when that account is not
	// active. See [WithAccountStatus].
	status accountStatusGate

	// mirroredKeyIdleTTL is the sliding idle window re-applied to a
	// TTL-bearing record on every successful validated Lookup — the
	// read-path half of the [MirroredKeyIdleTTL] bound. Zero disables
	// refresh-on-use. See [Lookup] and [refreshIdleTTL].
	mirroredKeyIdleTTL time.Duration
}

// accountStatusGate is the account-level kill switch both validators
// apply to a record naming a platform account. See [accountStatusGate.check].
type accountStatusGate struct {
	// accounts, when nil, disables the gate.
	accounts AccountStatusReader

	// ttl / maxStale bound the status cache below.
	// See [DefaultAccountStatusCacheTTL].
	ttl      time.Duration
	maxStale time.Duration

	// cache memoises the last-read account status per slug so the kill-switch GetBySlug runs at most once per account per ttl window and a Postgres blip rides out on last-known status. Only populated when accounts != nil; guarded by mu.
	mu    sync.RWMutex
	cache map[string]cachedAccountStatus
}

// init sets the freshness window. Past it each request re-reads Postgres, and a cached status covers a GetBySlug *error* only up to a fixed multiple of it;
// a Postgres outage also blocks the suspension write itself, so riding out cannot mask a suspension an operator could have applied.
func (g *accountStatusGate) init(ttl time.Duration) {
	g.ttl = ttl
	g.maxStale = 10 * ttl
	g.cache = make(map[string]cachedAccountStatus)
}

// cachedAccountStatus is one memoised account read: the status the kill
// switch gates on and the overrides the budget cascade resolves.
type cachedAccountStatus struct {
	acct platform.Account
	at   time.Time
}

// DefaultAccountStatusCacheTTL is the freshness window of the Redis validators' account-status cache: at most one kill-switch query per account per window instead of per request.
const DefaultAccountStatusCacheTTL = 30 * time.Second

// MirroredKeyIdleTTL is the SLIDING idle window for register-mirrored credentials (CreateWithSecret, and Create for SelfService): every successful Lookup slides it, so a used key never expires while an abandoned one ages out,
// capping the keyspace growth open /v1/register would otherwise make unbounded. 90 days exceeds any plausible active gap; the TTL also makes the record eligible for volatile-lru eviction, whereas TTL-less records never are.
const MirroredKeyIdleTTL = 90 * 24 * time.Hour

// AccountStatusReader is the narrow slice of
// [platform.AccountStore] the Redis validator needs to honour an
// account-level suspension and the account's rate-limit / quota
// overrides. *postgresstore.AccountStore satisfies it.
type AccountStatusReader interface {
	GetBySlug(ctx context.Context, slug string) (platform.Account, error)
}

// APIKeyRecord is the JSON shape at `apikey:<hash>`, marshalled by the admin/seeding path and only unmarshalled by the validator. Time fields are RFC 3339; absent means no constraint (no expiry, not revoked).
type APIKeyRecord struct {
	// KeyID — public-safe identifier for this key. Distinct from
	// the secret hash (the Redis key) so it can appear in logs and
	// /v1/account/me responses without leaking the credential.
	// Generated at issuance time; stable for the key's lifetime.
	KeyID string `json:"key_id"`

	// Identifier — owner-account reference. Plain string; the
	// validator passes it through to [Subject.Identifier]. Used as
	// the rate-limit bucket key for the apikey tier.
	Identifier string `json:"identifier"`

	// Label — human-readable name set by the customer at creation
	// (`/v1/account/keys` POST body). Unused by the auth layer;
	// surfaced via /v1/account/me. Optional.
	Label string `json:"label,omitempty"`

	// KeyPrefix is the first characters of the plaintext (e.g. `sip_4f9c1d8b`), stored at creation, safe to log (not enough to authenticate) and shown in dashboard listings; empty on records minted without one.
	KeyPrefix string `json:"key_prefix,omitempty"`

	// Tier — the [Tier] this key authenticates as. Production
	// records carry [TierAPIKey]; an operator key may use
	// [TierOperator] to unlock admin endpoints.
	Tier Tier `json:"tier"`

	// Scopes is an optional capability list; empty or absent means no restriction, not no access.
	// These ARE enforced at runtime: middleware.KeyPolicy (checkScopes) rejects a route needing a scope the key lacks with `scope-denied`; verify names against keypolicy.go's route table before issuing.
	Scopes []string `json:"scopes,omitempty"`

	// RateLimitPerMin — overrides the per-tier default (zero means
	// "use the tier default"). Set to a non-zero value for paid
	// customers on a custom plan.
	RateLimitPerMin int `json:"rate_limit_per_min,omitempty"`

	// CreatedAt — when the key was issued. Required — the
	// /v1/account/me response surfaces it. The store sets it on
	// Create; the validator passes it through.
	CreatedAt time.Time `json:"created_at,omitempty"`

	// ExpiresAt — zero means never. A non-zero value in the past
	// triggers [ErrTokenExpired].
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// RevokedAt — zero means active. Any non-zero value (even in
	// the future, which would be a bug in the writer) triggers
	// [ErrUnauthorized].
	RevokedAt time.Time `json:"revoked_at,omitempty"`

	// IPAllowlist / RefererAllowlist / Permissions are the per-key policy fields mirrored from Postgres keys, so a cache-hit Subject is policy-identical to a miss; omitting them would silently bypass KeyPolicy until the TTL elapsed.
	// IPAllowlist is CIDR text on the wire (netip.Prefix is not JSON-friendly); empty or nil means no IP gate.
	IPAllowlist []string `json:"ip_allowlist,omitempty"`

	// RefererAllowlist is rendered as `[]string` of exact-host
	// values. Empty / nil = "no Referer gate".
	RefererAllowlist []string `json:"referer_allowlist,omitempty"`

	// PermissionsAll = true bypasses the per-endpoint allow/deny
	// check (operator-style "any endpoint" keys).
	PermissionsAll bool `json:"permissions_all,omitempty"`

	// AllowPermissions / DenyPermissions carry the per-endpoint
	// or prefix permission entries. Wire shape uses the same
	// (endpoint, endpoint_prefix) two-of pattern as
	// platform.KeyPermissionEntry.
	AllowPermissions []SubjectPermissionEntry `json:"allow_permissions,omitempty"`
	DenyPermissions  []SubjectPermissionEntry `json:"deny_permissions,omitempty"`

	// MonthlyQuota — when > 0, the per-key monthly request cap
	// the runtime quota middleware enforces. Zero (the default)
	// disables the check.
	MonthlyQuota int64 `json:"monthly_quota,omitempty"`

	// EmailVerifiedAt is when the customer proved ownership of their
	// signup email. Zero = never verified. The `RequireEmailVerified`
	// middleware, mounted by default, answers an unverified key whose
	// identifier starts with `signup-` with 403.
	EmailVerifiedAt time.Time `json:"email_verified_at,omitempty"`
}

// RedisOption configures a [RedisAPIKeyValidator] at construction.
type RedisOption func(*RedisAPIKeyValidator)

// WithClock overrides the time source used for expiry comparison.
// Production uses time.Now; tests inject a fixed clock.
func WithClock(now func() time.Time) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.now = now }
}

// WithAccountStatus wires the account-level kill switch. The Postgres validator and dashboard session middleware enforce suspension themselves, but this is the DEFAULT backend (auth_backend=redis): without a reader, suspending an account does not stop its keys here.
// With one, records whose Identifier is AccountIdentifier(slug) are rejected unless the account is active; `signup-<emailhash>` records have no account and are killed by per-key revoke. Nil skips the check and the per-request Postgres read.
func WithAccountStatus(accounts AccountStatusReader) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.status.accounts = accounts }
}

// WithMirroredKeyIdleTTL overrides the sliding idle window re-applied on
// each successful Lookup (default [MirroredKeyIdleTTL]). A non-positive
// value disables refresh-on-use. Tests inject a short window to exercise
// the slide deterministically.
func WithMirroredKeyIdleTTL(ttl time.Duration) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.mirroredKeyIdleTTL = ttl }
}

// NewRedisAPIKeyValidator constructs a validator that reads records
// from rdb. rdb MUST be non-nil — callers wire this only after
// confirming Redis is available; the auth middleware fails-loud at
// 503 if the validator field is left as the Noop stub.
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

// Lookup implements [APIKeyValidator]. One Redis GET per call;
// translates the record's expiry/revocation into the correct
// sentinel error.
func (v *RedisAPIKeyValidator) Lookup(ctx context.Context, key string) (Subject, error) {
	if key == "" {
		// Unreachable from the middleware (which short-circuits on
		// empty), but the validator is the trust boundary — an
		// admin tool that calls this directly must not be able to
		// authenticate as "the empty-string subject".
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
		// Malformed record is operator-side corruption, not caller fault: wrap a non-sentinel so the middleware's default branch returns 401 without detail; the admin-side log line catches it.
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

	// A malformed allowlist entry fails closed (401), identically on
	// both validators — see [subjectFromRecord].
	sub, err := subjectFromRecord(rec)
	if err != nil {
		return Subject{}, fmt.Errorf("auth: apikey ip_allowlist decode: %w", err)
	}
	if isAccount {
		// The operator's account overrides apply on this backend exactly
		// as the Postgres validator applies them: a rate-limit floor and a
		// monthly-quota ceiling over the per-key values.
		sub.RateLimitPerMin = acct.ResolveKeyRateLimitPerMin(sub.RateLimitPerMin)
		sub.MonthlyQuota = acct.ResolveKeyMonthlyQuota(sub.MonthlyQuota)
	}

	// Refresh-on-use: a fully-validated TTL-bearing key (the register mirror) has its idle window slid forward. Placed after every rejection gate so a suspended account's key is NOT kept alive.
	// Best-effort EXPIRE ... XX: never touches persistent records, and a failed refresh never rejects auth.
	v.refreshIdleTTL(ctx, hash)
	return sub, nil
}

// subjectFromRecord is the single APIKeyRecord-to-Subject mapping (recordFromSubject is its inverse) used by both validators, so no path can drop a field. Rejection gates stay with the caller;
// an unparseable allowlist entry is an error so the caller fails closed.
func subjectFromRecord(rec APIKeyRecord) (Subject, error) {
	ipAllowlist, err := decodeIPAllowlist(rec.IPAllowlist)
	if err != nil {
		return Subject{}, err
	}
	tier := rec.Tier
	if tier == "" {
		// Records seeded without an explicit tier default to the apikey
		// tier; an operator key must set tier=operator explicitly.
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

// recordFromSubject is the inverse of [subjectFromRecord]: the record the
// Postgres validator persists into its read-through cache.
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

// refreshIdleTTL slides a TTL-bearing record's idle window to MirroredKeyIdleTTL with EXPIRE ... XX, a no-op on persistent (operator-seeded) records. Best-effort: errors are swallowed, since the worst case is an idle key expiring on its existing TTL.
func (v *RedisAPIKeyValidator) refreshIdleTTL(ctx context.Context, hash string) {
	if v.mirroredKeyIdleTTL <= 0 {
		return
	}
	_ = v.rdb.ExpireXX(ctx, cachekeys.APIKey(hash).String(), v.mirroredKeyIdleTTL).Err()
}

// check enforces the account kill switch for a record naming a platform account and returns the account read; isAccount is false (err nil) when it does not apply. A short-TTL cache fronts GetBySlug, so suspension and override changes propagate within ttl.
// ErrNotFound (closed account) is ErrUnauthorized. A transport error WITH a cached status within maxStale rides out on it (a blip does not 401 customers, a suspended account is still rejected);
// with NO usable cache it is ErrAccountStatusUnavailable (retryable 503, not a misleading 401), still failing closed.
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
		// Transport/degradation error. Ride out on a last-known status
		// within the staleness bound rather than degrading auth for
		// every active customer on a transient Postgres blip.
		if cached, ok := g.cachedStatus(slug); ok && now.Sub(cached.at) <= g.maxStale {
			return cached.acct, true, statusToError(cached.acct.Status)
		}
		// Truly unknown: no usable cached status. Fail closed, but as a
		// retryable 503 ("auth layer degraded"), not a 401 credential
		// rejection.
		return platform.Account{}, false,
			fmt.Errorf("auth: apikey account status %q: %w: %w", slug, ErrAccountStatusUnavailable, err)
	}

	g.storeStatus(slug, acct, now)
	return acct, true, statusToError(acct.Status)
}

// statusToError maps a resolved account status to the auth outcome:
// active authenticates, everything else (suspended/closed) is
// [ErrUnauthorized].
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

// evictStaleStatusLocked drops every cache entry older than maxStale.
// Past that bound check never reads an entry again (the ride-out gate
// refuses it), so retaining it only grows the map for the lifetime of
// the process with no cache-hit benefit. Called with mu held.
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
