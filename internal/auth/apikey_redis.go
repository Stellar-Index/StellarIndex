package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RedisAPIKeyValidator implements [APIKeyValidator] backed by Redis.
//
// Storage shape (one record per key):
//
//	KEY   apikey:<sha256-hex>
//	VALUE JSON [APIKeyRecord]
//
// Plaintext keys are never stored; the lookup hashes the
// caller-supplied bytes with SHA-256 and looks up by hash. A Redis
// dump leaks owner identifiers and tier mapping but never the key
// material itself.
//
// Lookup errors:
//
//   - record absent       → [ErrUnauthorized]
//   - revoked_at set      → [ErrUnauthorized]
//   - expires_at past     → [ErrTokenExpired]
//   - record undecodable  → wrapped non-sentinel (operator log signal)
//   - Redis I/O failure   → wrapped non-sentinel (middleware → 503)
//
// Concurrency: safe for use across goroutines — every Lookup is one
// GET. The validator carries no mutable state.
type RedisAPIKeyValidator struct {
	rdb redis.Cmdable
	now func() time.Time
	// accounts, when non-nil, enables the account-level kill switch:
	// a record whose Identifier names a platform account
	// ([AccountIdentifier]) is rejected when that account is not
	// active. See [WithAccountStatus].
	accounts AccountStatusReader
}

// AccountStatusReader is the narrow slice of
// [platform.AccountStore] the Redis validator needs to honour an
// account-level suspension. *postgresstore.AccountStore satisfies it.
type AccountStatusReader interface {
	GetBySlug(ctx context.Context, slug string) (platform.Account, error)
}

// APIKeyRecord is the JSON shape stored at `apikey:<hash>`. The
// admin/seeding path is responsible for marshalling this; the
// validator only unmarshals.
//
// All time fields are RFC 3339; absent fields decode to the zero
// time and are interpreted as "no constraint" (no expiry / not
// revoked).
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

	// KeyPrefix — first N characters of the plaintext key (e.g.
	// `sip_4f9c1d8b`).
	// Stored at creation time; safe to log
	// because it's not enough to authenticate (the secret tail is
	// what makes the key). Customers see this in dashboard
	// listings to identify which row corresponds to which key in
	// their secret manager.
	//
	// Empty for keys minted before this field shipped — the
	// dashboard renders "—" in that case. New keys always have it.
	KeyPrefix string `json:"key_prefix,omitempty"`

	// Tier — the [Tier] this key authenticates as. Production
	// records carry [TierAPIKey]; an operator key may use
	// [TierOperator] to unlock admin endpoints.
	Tier Tier `json:"tier"`

	// Scopes — optional capability list. Empty slice and absent are
	// equivalent ("no special scopes"), which means "no scope
	// restriction" rather than "no access".
	//
	// **These ARE enforced at runtime.** `middleware.KeyPolicy`
	// (internal/api/v1/middleware/keypolicy.go, `checkScopes`) rejects a
	// request whose route requires a scope the key doesn't carry, with a
	// `scope-denied` problem response. Minting a key with a narrow scope
	// set therefore narrows what it can call — verify the scope names
	// against keypolicy.go's route table before issuing.
	//
	// This paragraph used to say the opposite ("stored but NOT enforced
	// at any runtime endpoint … relying on them for access control is a
	// footgun"), a day-1 note that outlived the enforcement hook landing.
	// An operator reading it minted deliberately-scoped keys believing
	// them inert and got 403s (C3-088, audit-2026-07-23).
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

	// IPAllowlist / RefererAllowlist / Permissions are the
	// per-key policy fields the dashboard exposes for
	// Postgres-backed keys (the PostgresAPIKeyValidator cache).
	// F-1226 (codex audit-2026-05-12): without these the cache-
	// hit path constructed a Subject with empty policy fields,
	// silently bypassing KeyPolicy enforcement until the cache
	// entry TTL elapsed and the next request rebuilt from
	// Postgres. Now the cache mirrors what the Postgres rebuild
	// produces so the cache-hit Subject is policy-identical to
	// the cache-miss Subject.
	//
	// IPAllowlist is rendered as `[]string` of CIDR text on the
	// wire (e.g. ["10.0.0.0/8"]) — netip.Prefix isn't directly
	// JSON-friendly. Empty / nil = "no IP gate".
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
	// disables the check. F-1226 (codex audit-2026-05-12).
	MonthlyQuota int64 `json:"monthly_quota,omitempty"`

	// EmailVerifiedAt is the timestamp the customer clicked
	// the verification link in the post-signup email. Zero =
	// never verified. F-1218 wave 45 (codex audit-2026-05-12):
	// the optional `RequireEmailVerified` middleware uses this
	// flag to gate /v1/* access — keys minted via /v1/signup
	// stay usable until an operator opts in via config, then
	// unverified keys 403 until the customer clicks the link.
	EmailVerifiedAt time.Time `json:"email_verified_at,omitempty"`
}

// RedisOption configures a [RedisAPIKeyValidator] at construction.
type RedisOption func(*RedisAPIKeyValidator)

// WithClock overrides the time source used for expiry comparison.
// Production uses time.Now; tests inject a fixed clock.
func WithClock(now func() time.Time) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.now = now }
}

// WithAccountStatus wires the account-level kill switch (C3-010,
// audit-2026-07-23).
//
// The whole suspension machinery already existed and was already
// enforced by the Postgres validator (`acct.Status != AccountActive` →
// [ErrUnauthorized]) and by the dashboard session middleware — but this
// validator never read account status at all, and it is the DEFAULT
// backend (`auth_backend=redis`). So an operator suspending an account
// did not stop its keys from authenticating here: not through the admin
// API, not through the dashboard, not even through a manual
// `UPDATE accounts SET status='suspended'`.
//
// With a reader wired, a record whose Identifier is
// [AccountIdentifier](slug) — the shape the Postgres validator stamps
// and POST /v1/account/keys copies onto every self-service record — has
// its account resolved and is rejected unless the account is active.
// Legacy `signup-<emailhash>` records carry no account reference and are
// unaffected; the operator kill switch for those is the per-key revoke
// (DELETE /v1/admin/keys/{keyID}).
//
// Nil (the default) preserves the pre-fix behaviour exactly: no account
// lookup, no per-request Postgres read.
func WithAccountStatus(accounts AccountStatusReader) RedisOption {
	return func(v *RedisAPIKeyValidator) { v.accounts = accounts }
}

// NewRedisAPIKeyValidator constructs a validator that reads records
// from rdb. rdb MUST be non-nil — callers wire this only after
// confirming Redis is available; the auth middleware fails-loud at
// 503 if the validator field is left as the Noop stub.
func NewRedisAPIKeyValidator(rdb redis.Cmdable, opts ...RedisOption) *RedisAPIKeyValidator {
	if rdb == nil {
		panic("auth: NewRedisAPIKeyValidator: rdb must not be nil")
	}
	v := &RedisAPIKeyValidator{rdb: rdb, now: time.Now}
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
		// Malformed record is operator-side corruption, not caller
		// fault. Wrap a non-sentinel so the middleware's default
		// branch returns 401 (the safer of the two — we won't
		// surface details about why); a parallel log line on the
		// admin side will catch the corruption.
		return Subject{}, fmt.Errorf("auth: apikey record decode: %w", err)
	}

	if !rec.RevokedAt.IsZero() {
		return Subject{}, ErrUnauthorized
	}
	if !rec.ExpiresAt.IsZero() && !v.now().Before(rec.ExpiresAt) {
		return Subject{}, ErrTokenExpired
	}
	if err := v.accountActive(ctx, rec.Identifier); err != nil {
		return Subject{}, err
	}

	// Reuse the Postgres validator's CIDR decode so a malformed
	// allowlist entry fails closed (401) identically on both stores.
	ipAllowlist, err := decodeIPAllowlist(rec.IPAllowlist)
	if err != nil {
		return Subject{}, fmt.Errorf("auth: apikey ip_allowlist decode: %w", err)
	}

	tier := rec.Tier
	if tier == "" {
		// Records seeded without an explicit tier default to the
		// apikey tier — the most-common case. An operator key
		// must set tier=operator explicitly.
		tier = TierAPIKey
	}
	return Subject{
		Identifier:      rec.Identifier,
		Tier:            tier,
		Scopes:          rec.Scopes,
		KeyID:           rec.KeyID,
		RateLimitPerMin: rec.RateLimitPerMin,
		CreatedAt:       rec.CreatedAt,
		Label:           rec.Label,
		KeyPrefix:       rec.KeyPrefix,
		// F-1218 wave 45 (codex audit-2026-05-12): populate the
		// EmailVerifiedAt timestamp so the optional
		// `middleware.RequireEmailVerified` can gate /v1/* access
		// on whether the customer ever clicked the post-signup
		// verification link. Zero = never verified; the gate
		// fires when `RequireEmailVerified` is wired AND the
		// subject's identifier indicates a /v1/signup origin
		// (legacy operator-minted + dashboard-minted keys are
		// scoped out so the gate doesn't break them).
		EmailVerifiedAt: rec.EmailVerifiedAt,
		// Permission posture (F-1226 second half, found 2026-06-12): the
		// record has carried these fields since wave 45, but Lookup never
		// mapped them onto the Subject — so EVERY redis-minted key hit the
		// permission middleware's closed posture (AllowAllPermissions=false
		// + no entries) and 403'd on all endpoints ("this key has no
		// permission entries"). Mirror the Postgres validator's mapping.
		AllowAllPermissions: rec.PermissionsAll,
		AllowPermissions:    rec.AllowPermissions,
		DenyPermissions:     rec.DenyPermissions,
		IPAllowlist:         ipAllowlist,
		RefererAllowlist:    rec.RefererAllowlist,
	}, nil
}

// accountActive enforces the account-level kill switch for a record
// that names a platform account. Returns nil when the check does not
// apply (no reader wired, or a non-account identifier).
//
// Fails CLOSED on a lookup error, unlike the cache-degradation paths
// elsewhere in this package: this is the suspension gate, and "the
// account store blipped, so authenticate the suspended customer anyway"
// is the one degradation a kill switch must not make. A missing account
// row for an `acct:`-prefixed identifier is likewise a rejection — the
// record references an account that no longer exists, which is the
// closed-account case, not an unknown one.
func (v *RedisAPIKeyValidator) accountActive(ctx context.Context, identifier string) error {
	if v.accounts == nil {
		return nil
	}
	slug, ok := strings.CutPrefix(identifier, AccountIdentifierPrefix)
	if !ok || slug == "" {
		return nil // legacy signup-<hash> record: no account to check
	}
	acct, err := v.accounts.GetBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			return ErrUnauthorized
		}
		return fmt.Errorf("auth: apikey account status %q: %w", slug, err)
	}
	if acct.Status != platform.AccountActive {
		return ErrUnauthorized
	}
	return nil
}

// HashAPIKey returns the hex-encoded SHA-256 of key. Exposed for
// admin tooling that seeds records into Redis — the admin path
// computes the hash, builds the record, calls
// [cachekeys.APIKey](hash), and SET'S the JSON bytes. Lookup re-
// derives the same hash on read.
func HashAPIKey(key string) string { return hashAPIKey(key) }

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Compile-time check.
var _ APIKeyValidator = (*RedisAPIKeyValidator)(nil)
