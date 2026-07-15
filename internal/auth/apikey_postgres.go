package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// PostgresAPIKeyValidator reads keys from the Postgres
// `api_keys` table (the dashboard's source of truth) with an
// optional Redis read-through cache.
//
// This is the cutover validator: keys minted by the dashboard
// (`internal/api/v1/dashboardkeys`) authenticate without a
// separate mirror-write step. Existing Redis-only keys (minted
// by `/v1/signup` before the cutover) still work — the cache is
// consulted first and falls back to Postgres only on miss, so
// the validator transparently serves both populations until
// every legacy record has been rotated through dashboard mint.
//
// On a Postgres-served lookup the result is written back into
// the Redis cache so subsequent requests for the same key
// short-circuit at the cache. The dashboard's Revoke handler is
// responsible for DEL'ing the cache entry; we don't TTL-out
// records aggressively because revocations are a cold path.
//
// On Redis I/O failures we degrade-not-fail: a transient cache
// outage still authenticates calls via Postgres directly. The
// inverse is not true — Postgres being down means nobody
// authenticates (the keys table is the source of truth).
type PostgresAPIKeyValidator struct {
	keys     platform.APIKeyStore
	accounts platform.AccountStore
	cache    redis.Cmdable
	now      func() time.Time
	// cacheTTL bounds the cache row lifetime — if a revoke DEL
	// fails (rare), the row eventually rolls off rather than
	// authenticating a revoked key indefinitely.
	cacheTTL time.Duration
}

// PostgresValidatorOptions configures a [PostgresAPIKeyValidator].
type PostgresValidatorOptions struct {
	Keys     platform.APIKeyStore
	Accounts platform.AccountStore
	// Cache, when non-nil, enables the Redis read-through path.
	// Recommended in production; nil falls through to per-request
	// Postgres lookups (workable but ~10x slower than the cache hit).
	Cache    redis.Cmdable
	Now      func() time.Time
	CacheTTL time.Duration
}

// NewPostgresAPIKeyValidator constructs the cutover validator.
// Returns an error if the required platform stores aren't wired
// — the validator is useless without them.
func NewPostgresAPIKeyValidator(opts PostgresValidatorOptions) (*PostgresAPIKeyValidator, error) {
	if opts.Keys == nil {
		return nil, errors.New("auth: PostgresAPIKeyValidator: Keys store is required")
	}
	if opts.Accounts == nil {
		return nil, errors.New("auth: PostgresAPIKeyValidator: Accounts store is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.CacheTTL
	if ttl == 0 {
		ttl = 1 * time.Hour
	}
	return &PostgresAPIKeyValidator{
		keys:     opts.Keys,
		accounts: opts.Accounts,
		cache:    opts.Cache,
		now:      now,
		cacheTTL: ttl,
	}, nil
}

// Lookup implements [APIKeyValidator]. Cache → Postgres → 401.
func (v *PostgresAPIKeyValidator) Lookup(ctx context.Context, key string) (Subject, error) {
	if key == "" {
		return Subject{}, ErrUnauthorized
	}
	hexHash := hashAPIKey(key)

	// 1. Cache.
	if v.cache != nil {
		if sub, ok, err := v.cacheLookup(ctx, hexHash); ok {
			return sub, err
		}
	}

	// 2. Postgres canonical lookup. Hash is the key column.
	rawHash, err := decodeHexHash(hexHash)
	if err != nil {
		// Unreachable — hashAPIKey produces a valid hex string.
		return Subject{}, fmt.Errorf("auth: hash decode: %w", err)
	}
	pgKey, err := v.keys.GetByHash(ctx, rawHash)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			return Subject{}, ErrUnauthorized
		}
		return Subject{}, fmt.Errorf("auth: postgres lookup: %w", err)
	}
	if !pgKey.RevokedAt.IsZero() {
		return Subject{}, ErrUnauthorized
	}
	if !pgKey.ExpiresAt.IsZero() && !v.now().Before(pgKey.ExpiresAt) {
		return Subject{}, ErrTokenExpired
	}

	acct, err := v.accounts.Get(ctx, pgKey.AccountID)
	if err != nil {
		// Orphaned key (FK should prevent this, but defensive). Treat
		// as unauthorised — the operator log captures the why.
		return Subject{}, ErrUnauthorized
	}
	if acct.Status != platform.AccountActive {
		return Subject{}, ErrUnauthorized
	}

	// Monthly-quota cascade. The account-level override
	// (accounts.monthly_request_quota_override) is the operator's
	// hard CEILING — the customer can only ever LOWER their cap
	// beneath it, never raise it. This is the deliberate
	// symmetric-opposite of the rate-limit override below (which is a
	// FLOOR that only raises).
	//
	//   - per-key 0 (default/inherit): fall back to the account
	//     override. If that's also 0 the middleware's
	//     `MonthlyQuota <= 0` short-circuit leaves the request
	//     unmetered (the current default when nothing is set).
	//   - per-key > 0 AND override > 0: use min(per-key, override) so
	//     an override always clamps a per-key value budgeted above it.
	//     This retroactively closes the hole for any key minted before
	//     the dashboard's mint-time clamp shipped (audit-2026-07
	//     MEDIUM): even a persisted `monthly_quota: 9_000_000_000` can
	//     never exceed the operator's account cap at enforcement time.
	//   - per-key > 0 AND override 0: honour the per-key value (the
	//     dashboard already clamped it to the tier ceiling at mint).
	//
	// F-1226 (codex audit-2026-05-12) introduced the fallback leg;
	// audit-2026-07 hardened it into a ceiling.
	monthlyQuota := pgKey.MonthlyQuota
	switch {
	case monthlyQuota == 0:
		monthlyQuota = acct.MonthlyRequestQuotaOverride
	case acct.MonthlyRequestQuotaOverride > 0 && acct.MonthlyRequestQuotaOverride < monthlyQuota:
		monthlyQuota = acct.MonthlyRequestQuotaOverride
	}

	// Account-level rate-limit override (platform-spec accounts.
	// rate_limit_per_min_override, "when set, replaces the tier
	// default"). Symmetric with the monthly-quota cascade above, but
	// the per-key `rate_limit_per_min` column is NOT NULL CHECK (> 0)
	// — it's never the zero sentinel that the quota cascade keys on —
	// so the override can't be a "fall back when per-key is unset"
	// rule here. Instead it acts as an account-wide FLOOR: the staff
	// override raises every key that would otherwise be below it (an
	// operator comps an enterprise account to 100k/min without
	// touching each key), while a key explicitly budgeted ABOVE the
	// override keeps its higher value. Monotonic by construction — an
	// override only ever raises the effective budget, so it can never
	// silently shrink a customer's paid-for limit. Effective
	// immediately for cache-miss lookups; cache hits inherit the
	// resolved value on the next Postgres read after the cache TTL
	// (same staleness window the monthly-quota override already has).
	rateLimit := pgKey.RateLimitPerMin
	if acct.RateLimitPerMinOverride > rateLimit {
		rateLimit = acct.RateLimitPerMinOverride
	}
	sub := Subject{
		Identifier:          "acct:" + acct.Slug,
		Tier:                pgTierToAuthTier(pgKey.Tier),
		KeyID:               pgKey.ID,
		RateLimitPerMin:     rateLimit,
		CreatedAt:           pgKey.CreatedAt,
		Label:               pgKey.Name,
		KeyPrefix:           pgKey.KeyPrefix,
		Scopes:              pgKey.Scopes,
		IPAllowlist:         pgKey.IPAllowlist,
		RefererAllowlist:    pgKey.RefererAllowlist,
		AllowAllPermissions: pgKey.Permissions.All,
		AllowPermissions:    convertPermissionEntries(pgKey.Permissions.Allow),
		DenyPermissions:     convertPermissionEntries(pgKey.Permissions.Deny),
		MonthlyQuota:        monthlyQuota,
	}

	// 3. Cache write-back. Best-effort; a write failure doesn't
	// affect this request (the caller already has the Subject).
	if v.cache != nil {
		v.cacheStore(ctx, hexHash, sub, pgKey)
	}
	return sub, nil
}

// cacheLookup returns (Subject, hit, sentinelErr). hit=false
// means "miss or transient cache failure — fall through to
// Postgres". When hit=true the second return is the sentinel the
// caller should propagate (or nil for a successful auth).
func (v *PostgresAPIKeyValidator) cacheLookup(ctx context.Context, hexHash string) (Subject, bool, error) {
	raw, err := v.cache.Get(ctx, cachekeys.APIKey(hexHash).String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return Subject{}, false, nil
	}
	if err != nil {
		// Cache I/O error — degrade-not-fail. Fall through to
		// Postgres so a transient Redis blip doesn't take auth
		// down. The error is intentionally swallowed; an operator
		// log on the parent .Get path catches the cache state.
		return Subject{}, false, nil //nolint:nilerr // deliberate degrade-not-fail
	}
	var rec APIKeyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		// Corrupt cache entry — same degrade-not-fail rationale
		// as above. Postgres lookup will rebuild the cache row.
		return Subject{}, false, nil //nolint:nilerr // deliberate degrade-not-fail
	}
	if !rec.RevokedAt.IsZero() {
		return Subject{}, true, ErrUnauthorized
	}
	if !rec.ExpiresAt.IsZero() && !v.now().Before(rec.ExpiresAt) {
		return Subject{}, true, ErrTokenExpired
	}
	tier := rec.Tier
	if tier == "" {
		tier = TierAPIKey
	}
	// F-1226 (codex audit-2026-05-12): hydrate policy fields from
	// the cache row so cache-hit Subjects carry the same gates as
	// cache-miss Subjects. Pre-fix these fields were missing, so
	// KeyPolicy enforcement silently turned into a no-op for any
	// request that hit the cache TTL window.
	ipAllow, err := decodeIPAllowlist(rec.IPAllowlist)
	if err != nil {
		// Corrupt cache entry — same degrade-not-fail rationale as
		// Unmarshal failures upstream.
		return Subject{}, false, nil //nolint:nilerr // deliberate degrade-not-fail
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
		IPAllowlist:         ipAllow,
		RefererAllowlist:    rec.RefererAllowlist,
		AllowAllPermissions: rec.PermissionsAll,
		AllowPermissions:    rec.AllowPermissions,
		DenyPermissions:     rec.DenyPermissions,
		MonthlyQuota:        rec.MonthlyQuota,
	}, true, nil
}

// decodeIPAllowlist parses the on-the-wire CIDR-text slice back
// into netip.Prefix values. Empty input yields nil. Returns an
// error on the first un-parseable entry so the caller can treat
// the cache row as corrupt.
func decodeIPAllowlist(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("auth: decode IP allowlist entry %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// encodeIPAllowlist renders the in-memory netip.Prefix slice
// into the on-the-wire CIDR-text slice. Empty input yields nil.
func encodeIPAllowlist(prefixes []netip.Prefix) []string {
	if len(prefixes) == 0 {
		return nil
	}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, p.String())
	}
	return out
}

// cacheStore writes the Postgres-derived Subject into Redis with
// the configured TTL. Mirrors the legacy APIKeyRecord JSON shape
// so the cache and the legacy /v1/signup writers share one
// schema.
func (v *PostgresAPIKeyValidator) cacheStore(ctx context.Context, hexHash string, sub Subject, pgKey platform.APIKey) {
	rec := APIKeyRecord{
		KeyID:           sub.KeyID,
		Identifier:      sub.Identifier,
		Label:           sub.Label,
		KeyPrefix:       sub.KeyPrefix,
		Tier:            sub.Tier,
		Scopes:          sub.Scopes,
		RateLimitPerMin: sub.RateLimitPerMin,
		CreatedAt:       sub.CreatedAt,
		ExpiresAt:       pgKey.ExpiresAt,
		// F-1226 (codex audit-2026-05-12): persist policy fields so
		// cache-hit reads can reconstruct an enforcement-complete
		// Subject. Without these the dashboard's IP/Referer/
		// permission gates would be silently bypassed on cache hits.
		IPAllowlist:      encodeIPAllowlist(sub.IPAllowlist),
		RefererAllowlist: sub.RefererAllowlist,
		PermissionsAll:   sub.AllowAllPermissions,
		AllowPermissions: sub.AllowPermissions,
		DenyPermissions:  sub.DenyPermissions,
		MonthlyQuota:     sub.MonthlyQuota,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = v.cache.Set(ctx, cachekeys.APIKey(hexHash).String(), body, v.cacheTTL).Err()
}

// convertPermissionEntries maps platform.KeyPermissionEntry into
// auth.SubjectPermissionEntry so the middleware can enforce
// per-endpoint allow/deny without importing the platform package.
// F-1226 (codex audit-2026-05-12).
func convertPermissionEntries(entries []platform.KeyPermissionEntry) []SubjectPermissionEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]SubjectPermissionEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, SubjectPermissionEntry{
			Endpoint:       e.Endpoint,
			EndpointPrefix: e.EndpointPrefix,
		})
	}
	return out
}

// pgTierToAuthTier maps the platform-side enum to the auth-side
// Tier. Distinct enums because the platform table also covers
// future tier values (e.g. partner) the auth middleware doesn't
// yet have rate-limit buckets for.
func pgTierToAuthTier(t platform.APIKeyTier) Tier {
	switch t {
	case platform.APIKeyTierOperator:
		return TierOperator
	case platform.APIKeyTierPartner, platform.APIKeyTierAPIKey:
		fallthrough
	default:
		return TierAPIKey
	}
}

// decodeHexHash converts a hex string back into its raw byte
// form for the Postgres bytea column lookup.
func decodeHexHash(hexHash string) ([]byte, error) {
	out := make([]byte, len(hexHash)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexNibble(hexHash[i*2])
		if err != nil {
			return nil, err
		}
		lo, err := hexNibble(hexHash[i*2+1])
		if err != nil {
			return nil, err
		}
		out[i] = (hi << 4) | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("auth: invalid hex byte %q", c)
}

// InvalidateCachedKey removes a key's cached record from Redis.
// Called by the dashboard's Revoke handler so a revoked key
// stops authenticating immediately rather than waiting for the
// cache TTL to roll it off.
//
// hexHash is the SHA-256 of the plaintext, hex-encoded. Callers
// who only have a key_id need to look up the hash from
// platform.APIKeyStore.Get first — we don't add a Get-by-id
// here because the dashboard handler already has the row.
func (v *PostgresAPIKeyValidator) InvalidateCachedKey(ctx context.Context, hexHash string) error {
	if v.cache == nil {
		return nil
	}
	return v.cache.Del(ctx, cachekeys.APIKey(hexHash).String()).Err()
}

// Compile-time check.
var _ APIKeyValidator = (*PostgresAPIKeyValidator)(nil)
