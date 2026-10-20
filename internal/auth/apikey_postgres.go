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

// PostgresAPIKeyValidator reads keys from Postgres `api_keys` (source of truth) with an optional Redis read-through cache in its own `apikey-cache:` namespace; Redis failures degrade to Postgres, Postgres down means nobody authenticates.
type PostgresAPIKeyValidator struct {
	keys     platform.APIKeyStore
	accounts platform.AccountStore
	cache    redis.Cmdable
	now      func() time.Time
	// cacheTTL bounds the cache row lifetime — if a revoke DEL
	// fails (rare), the row eventually rolls off rather than
	// authenticating a revoked key indefinitely.
	cacheTTL time.Duration
	// status re-checks account status on a cache hit. The cached row
	// cannot carry a later suspension, and a canonical `apikey:` record
	// may have no api_keys row for a PATCH eviction to find.
	status accountStatusGate
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

// NewPostgresAPIKeyValidator constructs the validator, returning an error if the required platform stores are not wired.
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
	v := &PostgresAPIKeyValidator{
		keys:     opts.Keys,
		accounts: opts.Accounts,
		cache:    opts.Cache,
		now:      now,
		cacheTTL: ttl,
	}
	v.status.init(DefaultAccountStatusCacheTTL)
	v.status.accounts = opts.Accounts
	return v, nil
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
	// platform.APIKey.IsActive is the single source of truth for "may this key authenticate"; re-implementing the revoked/expired predicate here would leave a stale copy on the path where being wrong authenticates a dead credential.
	// The field checks below only classify an already-made rejection into a sentinel.
	if !pgKey.IsActive(v.now()) {
		if !pgKey.RevokedAt.IsZero() {
			return Subject{}, ErrUnauthorized
		}
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

	// Monthly-quota cascade: the account override (accounts.monthly_request_quota_override) is a hard CEILING the customer can only lower beneath, the opposite of the rate-limit FLOOR below.
	// Per-key 0 falls back to the override (both 0 = unmetered); per-key > 0 with override > 0 uses min(); per-key > 0 with override 0 is honoured. The cascade lives on platform.Account so account views report what is enforced.
	monthlyQuota := acct.ResolveKeyMonthlyQuota(pgKey.MonthlyQuota)

	// Account rate-limit override (rate_limit_per_min_override): the per-key column is NOT NULL CHECK (> 0), so there is no unset sentinel and the override acts as an account-wide FLOOR that raises lower keys,
	// while a key budgeted above it keeps its value; it only ever raises the budget, never shrinking a paid limit. Cache hits pick it up after the cache TTL, like the quota override.
	rateLimit := acct.ResolveKeyRateLimitPerMin(pgKey.RateLimitPerMin)
	sub := Subject{
		Identifier:          AccountIdentifier(acct.Slug),
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
		ExpiresAt:           pgKey.ExpiresAt,
		// EmailVerifiedAt stays zero: neither api_keys nor accounts has a
		// verification column, and RequireEmailVerified only gates the
		// `signup-` identifiers this path never stamps.
	}

	// 3. Cache write-back. Best-effort; a write failure doesn't
	// affect this request (the caller already has the Subject).
	if v.cache != nil {
		v.cacheStore(ctx, hexHash, sub)
	}
	return sub, nil
}

// cacheLookup returns (Subject, hit, sentinelErr): hit=false means miss or transient failure (fall through to Postgres); on hit the error is the sentinel to propagate or nil.
// The canonical `apikey:` record is read before the `apikey-cache:` row because it can carry revoked_at, so a derived cache row never shadows it.
func (v *PostgresAPIKeyValidator) cacheLookup(ctx context.Context, hexHash string) (Subject, bool, error) {
	rec, ok := v.readRecord(ctx, cachekeys.APIKey(hexHash).String())
	if !ok {
		rec, ok = v.readRecord(ctx, cachekeys.APIKeyCache(hexHash).String())
	}
	if !ok {
		return Subject{}, false, nil
	}
	if !rec.RevokedAt.IsZero() {
		return Subject{}, true, ErrUnauthorized
	}
	if !rec.ExpiresAt.IsZero() && !v.now().Before(rec.ExpiresAt) {
		return Subject{}, true, ErrTokenExpired
	}
	if _, _, err := v.status.check(ctx, rec.Identifier, v.now()); err != nil {
		return Subject{}, true, err
	}
	sub, err := subjectFromRecord(rec)
	if err != nil {
		// Corrupt cache entry — same degrade-not-fail rationale as
		// Unmarshal failures upstream.
		return Subject{}, false, nil //nolint:nilerr // deliberate degrade-not-fail
	}
	return sub, true, nil
}

// readRecord GETs and decodes one record. Absent, unreadable and corrupt
// all report false — degrade-not-fail: a Redis blip or a bad row falls
// through to Postgres, which rebuilds the cache row.
func (v *PostgresAPIKeyValidator) readRecord(ctx context.Context, key string) (APIKeyRecord, bool) {
	raw, err := v.cache.Get(ctx, key).Bytes()
	if err != nil {
		return APIKeyRecord{}, false
	}
	var rec APIKeyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return APIKeyRecord{}, false
	}
	return rec, true
}

// decodeIPAllowlist parses the wire CIDR-text slice back into netip.Prefix values (empty yields nil); the first bad entry is an
// error so the caller can treat the cache row as corrupt.
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

// cacheStoreScript SETs the cache row (KEYS[1]) unless an eviction
// tombstone (KEYS[2]) is live. ARGV: body, TTL in ms.
var cacheStoreScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
	return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 1
`)

// cacheStore writes the Postgres-derived Subject to Redis with the configured TTL, skipped while an eviction tombstone lives so a revoke or suspension committed mid-lookup is not overwritten by an older read (see evictCachedKey).
func (v *PostgresAPIKeyValidator) cacheStore(ctx context.Context, hexHash string, sub Subject) {
	body, err := json.Marshal(recordFromSubject(sub))
	if err != nil {
		return
	}
	keys := []string{cachekeys.APIKeyCache(hexHash).String(), cachekeys.APIKeyCacheEvicted(hexHash).String()}
	_ = cacheStoreScript.Run(ctx, v.cache, keys, body, v.cacheTTL.Milliseconds()).Err()
}

// convertPermissionEntries maps platform.KeyPermissionEntry into
// auth.SubjectPermissionEntry so the middleware can enforce
// per-endpoint allow/deny without importing the platform package.
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

// pgTierToAuthTier maps the platform enum to the auth Tier. Distinct enums: the platform table also covers future tiers (e.g. partner)
// that auth has no rate-limit buckets for yet.
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

// InvalidateCachedKey removes a key's cache row and any canonical `apikey:` record so a revoked key stops authenticating without waiting for the TTL; hexHash is the hex SHA-256 of the plaintext.
func (v *PostgresAPIKeyValidator) InvalidateCachedKey(ctx context.Context, hexHash string) error {
	if v.cache == nil {
		return nil
	}
	return evictCachedKey(ctx, v.cache, hexHash)
}

// Compile-time check.
var _ APIKeyValidator = (*PostgresAPIKeyValidator)(nil)
