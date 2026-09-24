package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// APIKeyStore is the WRITER side of the API-key persistence
// interface. [APIKeyValidator] is the read path; the two are
// deliberately separated so the validator (hot path on every
// request) can be wired without exposing the mutating surface to
// non-admin callers.
//
// Implementations must be safe for concurrent use.
type APIKeyStore interface {
	// Create issues a fresh API key for the supplied request data.
	// The plaintext key is returned ONCE and is unrecoverable
	// thereafter — callers must echo it to the requester before
	// dropping the response.
	//
	// req carries the operator-supplied fields (Identifier, Label,
	// Tier, Scopes, RateLimitPerMin, MonthlyQuota, ExpiresAt). The store fills
	// in KeyID + CreatedAt and generates the plaintext.
	//
	// Returns the public-safe APIKeyRecord (without the plaintext)
	// alongside the plaintext. Callers must not log or persist
	// the plaintext beyond the one-shot response body.
	Create(ctx context.Context, req CreateAPIKeyRequest) (rec APIKeyRecord, plaintext string, err error)
}

// CreateAPIKeyRequest is the payload [APIKeyStore.Create] consumes.
// The store ignores fields it computes itself (KeyID, CreatedAt).
type CreateAPIKeyRequest struct {
	// Identifier — owner-account reference. The store stamps this
	// onto APIKeyRecord.Identifier verbatim. Required.
	Identifier string

	// Label — customer-supplied human-readable name. Surfaced via
	// /v1/account/me. Optional.
	Label string

	// Tier — defaults to [TierAPIKey] when zero.
	Tier Tier

	// Scopes — optional capability list. Reserved for future
	// per-endpoint scope checks.
	Scopes []string

	// RateLimitPerMin — non-zero overrides the per-tier default.
	// Zero means "use the tier default".
	RateLimitPerMin int

	// MonthlyQuota — non-zero is the calendar-month billable-request
	// ceiling `middleware.MonthlyQuota` enforces against the key.
	// Zero means "inherit": the store copies the ceiling the
	// identifier's existing credentials already carry (see
	// [RedisAPIKeyStore.inheritedMonthlyQuota]), and persists 0 — no
	// ceiling — only when the identifier has none. The cap is opt-in
	// by contract; the store never invents one.
	//
	// The self-service rotation path (POST /v1/account/keys) sets it to
	// the caller's EFFECTIVE quota rather than its stored per-key value:
	// a record this store writes is read back without the Postgres
	// validator's account-override cascade, so copying a per-key 0
	// would ship the child unmetered.
	MonthlyQuota int64

	// ExpiresAt — zero means never. The self-service rotation path sets
	// it to the caller's own expiry via [ChildKeyRequest].
	ExpiresAt time.Time

	// EmailVerifiedAt — zero means the key has not (yet) passed the
	// /v1/signup/verify email-link flow. Set by the self-service
	// rotation path (POST /v1/account/keys) to the caller's own
	// stamp so a child key inherits its parent's verification:
	// the verification is a property of the identifier's owner,
	// not of one record, and there is no path that can verify a
	// non-signup KeyID after the fact. Signup leaves it zero.
	EmailVerifiedAt time.Time
}

// ChildKeyRequest builds the mint request for a key that parent delegates
// to itself (POST /v1/account/keys). It is the one place every delegated
// dimension is copied, so a field added to [CreateAPIKeyRequest] is pinned
// by TestChildKeyRequest_InheritsEveryField instead of silently minting
// as zero — zero means "unlimited" for quota and "never" for expiry.
// scopes must already be clamped to parent's own (middleware.ClampMintScopes).
func ChildKeyRequest(parent Subject, label string, scopes []string) CreateAPIKeyRequest {
	return CreateAPIKeyRequest{
		Identifier:      parent.Identifier,
		Label:           label,
		Tier:            parent.Tier,
		Scopes:          scopes,
		RateLimitPerMin: parent.RateLimitPerMin,
		MonthlyQuota:    parent.MonthlyQuota,
		ExpiresAt:       parent.ExpiresAt,
		EmailVerifiedAt: parent.EmailVerifiedAt,
	}
}

// RedisAPIKeyStore implements [APIKeyStore] against the same
// `apikey:<sha256-hex>` shape that [RedisAPIKeyValidator] reads.
//
// The store is the source of truth for issuance — it generates the
// plaintext (32 bytes from crypto/rand, hex-encoded with the `sip_`
// prefix) and the KeyID (8 bytes from crypto/rand, hex-encoded with
// the `kid_` prefix). The KeyID is distinct from the secret hash so
// it can appear in logs and customer-facing responses safely.
type RedisAPIKeyStore struct {
	rdb redis.Cmdable
	now func() time.Time
	// randRead is the entropy source — overridable in tests so the
	// test suite can pin generated key/KeyID bytes for the hash
	// stability assertions.
	randRead func([]byte) (int, error)
}

// StoreOption configures a [RedisAPIKeyStore] at construction.
type StoreOption func(*RedisAPIKeyStore)

// WithStoreClock overrides the time source for CreatedAt stamping.
// Production uses time.Now; tests inject a fixed clock.
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *RedisAPIKeyStore) { s.now = now }
}

// withRandRead overrides the entropy source. Internal — tests use
// it to pin generated bytes; production has no reason to override
// crypto/rand.
func withRandRead(fn func([]byte) (int, error)) StoreOption {
	return func(s *RedisAPIKeyStore) { s.randRead = fn }
}

// NewRedisAPIKeyStore constructs a store. rdb MUST be non-nil — the
// store is only wired when Redis is reachable; the API binary's
// auth layer falls back to the Noop validator (so requests 503)
// when Redis is missing, and the issuance endpoints simply 503 by
// not being mounted at all.
func NewRedisAPIKeyStore(rdb redis.Cmdable, opts ...StoreOption) *RedisAPIKeyStore {
	if rdb == nil {
		panic("auth: NewRedisAPIKeyStore: rdb must not be nil")
	}
	s := &RedisAPIKeyStore{
		rdb:      rdb,
		now:      time.Now,
		randRead: rand.Read,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// MaxKeyRateLimitPerMin is the highest per-key budget any surface may
// set: the Partner plan ceiling (platform.TierPartner.MaxRateLimitPerMin).
const MaxKeyRateLimitPerMin = 100_000

// ValidateKeyBounds is the store-side floor every mint and re-budget path
// inherits, whichever surface (HTTP, dashboard, ops CLI) called it.
func ValidateKeyBounds(rateLimitPerMin int, scopes []string) error {
	if rateLimitPerMin < 0 || rateLimitPerMin > MaxKeyRateLimitPerMin {
		return fmt.Errorf("rate limit %d must be in [0, %d] (0 = tier default)", rateLimitPerMin, MaxKeyRateLimitPerMin)
	}
	for _, sc := range scopes {
		if !platform.ValidKeyScope(sc) {
			return fmt.Errorf("unknown key scope %q (known: %v; an empty list is full access)", sc, platform.KnownKeyScopes())
		}
	}
	return nil
}

// GetByKeyID returns the record for keyID, or ErrKeyNotFound.
func (s *RedisAPIKeyStore) GetByKeyID(ctx context.Context, keyID string) (APIKeyRecord, error) {
	_, rec, found, err := s.findRecordByKeyID(ctx, keyID)
	if err != nil {
		return APIKeyRecord{}, fmt.Errorf("auth: GetByKeyID: %w", err)
	}
	if !found {
		return APIKeyRecord{}, ErrKeyNotFound
	}
	return rec, nil
}

// Create implements [APIKeyStore].
//
// Workflow:
//
//  1. Validate request (Identifier required); resolve an unset
//     MonthlyQuota from the identifier's existing credentials.
//  2. Generate KeyID (`kid_<16-hex>`) and plaintext (`sip_<64-hex>`).
//  3. Build APIKeyRecord with stamped CreatedAt and tier-defaulted Tier.
//  4. SET apikey:<sha256(plaintext)> JSON. SETNX semantics aren't
//     needed — the SHA-256 collision domain is astronomical and
//     the KeyID is independently unique.
//  5. Return (record, plaintext).
//
// On Redis I/O failure the plaintext is not surfaced (caller
// receives an empty string + the error). The record is also empty.
func (s *RedisAPIKeyStore) Create(ctx context.Context, req CreateAPIKeyRequest) (APIKeyRecord, string, error) {
	if req.Identifier == "" {
		return APIKeyRecord{}, "", errors.New("auth: Create: Identifier is required")
	}
	if err := ValidateKeyBounds(req.RateLimitPerMin, req.Scopes); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: %w", err)
	}
	monthlyQuota := req.MonthlyQuota
	if monthlyQuota <= 0 {
		// Resolved here, not in each handler, so no mint path (admin
		// mint, ops CLI, signup) can issue an unmetered credential to an
		// identifier whose plan is metered. Fail closed on a read error.
		inherited, err := s.inheritedMonthlyQuota(ctx, req.Identifier)
		if err != nil {
			return APIKeyRecord{}, "", fmt.Errorf("auth: Create: inherit monthly quota: %w", err)
		}
		monthlyQuota = inherited
	}

	keyID, err := generateID(s.randRead, "kid_", 8)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: generate key_id: %w", err)
	}
	// `sip_` namespace prefix (Stellar Index Pricing). Matches the
	// dashboard minter (dashboardkeys.generatePlaintext). Validation
	// is SHA-256 of the full plaintext, so the prefix is purely a
	// human-facing namespace label. (The last pre-rebrand-prefixed
	// key was deleted from the store 2026-07-03.)
	plaintext, err := generateID(s.randRead, "sip_", 32)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: generate plaintext: %w", err)
	}

	tier := req.Tier
	if tier == "" {
		tier = TierAPIKey
	}

	rec := APIKeyRecord{
		KeyID:           keyID,
		Identifier:      req.Identifier,
		Label:           req.Label,
		KeyPrefix:       KeyPrefix(plaintext),
		Tier:            tier,
		Scopes:          req.Scopes,
		RateLimitPerMin: req.RateLimitPerMin,
		MonthlyQuota:    monthlyQuota,
		CreatedAt:       s.now().UTC(),
		ExpiresAt:       req.ExpiresAt,
		EmailVerifiedAt: req.EmailVerifiedAt,
		// Operator-minted keys are full-access by default — matching the
		// dashboard issuance default (Permissions.All=true). Without this
		// the permission middleware's closed posture (no allow entries +
		// PermissionsAll=false) 403s EVERY request from a freshly minted
		// key ("this key has no permission entries") — caught 2026-06-12
		// when a mint-key'd load-test key failed 210k/210k requests.
		// Per-endpoint restriction stays a dashboard feature.
		PermissionsAll: true,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		// Should be unreachable — APIKeyRecord has no func/chan
		// fields. Wrap for diagnostic completeness.
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: marshal record: %w", err)
	}

	hash := hashAPIKey(plaintext)
	// No TTL: keys live until explicitly deleted. Expiry +
	// revocation are encoded in the JSON record so the validator
	// can return the right sentinel error.
	//
	// The record and its lookup-index entries land as one atomic write
	// ([RedisAPIKeyStore.writeRecord]); on failure nothing was written.
	if err := s.writeRecord(ctx, hash, rec, body, cachekeys.APIKeyTTL); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: redis set: %w", err)
	}
	return rec, plaintext, nil
}

// inheritedMonthlyQuota is the ceiling a key minted without one takes
// from the credentials its identifier already holds: the most generous
// LIVE one, so a mint can neither lift the identifier's plan nor tighten
// it. A live unmetered credential means the plan is unmetered (0). With
// no live credential the most generous lapsed one still binds, so letting
// every key lapse cannot reset a metered identifier to unmetered.
func (s *RedisAPIKeyStore) inheritedMonthlyQuota(ctx context.Context, identifier string) (int64, error) {
	recs, err := s.ListKeysForIdentifier(ctx, identifier)
	if err != nil {
		return 0, err
	}
	now := s.now()
	var live, lapsed int64
	anyLive := false
	for _, rec := range recs {
		if !rec.RevokedAt.IsZero() || (!rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt)) {
			lapsed = max(lapsed, rec.MonthlyQuota)
			continue
		}
		if rec.MonthlyQuota <= 0 {
			return 0, nil
		}
		anyLive = true
		live = max(live, rec.MonthlyQuota)
	}
	if anyLive {
		return live, nil
	}
	return lapsed, nil
}

// KeyPrefix returns the human-friendly identifier portion of a
// plaintext API key — the first 12 characters,
// covering the `sip_` namespace prefix plus 8 hex chars of
// entropy. Safe to log: 32 bits is far short of authentication-
// material; the secret tail is what makes the key. Customers
// see this in dashboard listings to identify which row maps to
// which key in their secret manager (mirrors AWS's `AKIA…`
// pattern).
//
// Returns "" for inputs shorter than 12 chars (defensive — never
// happens in practice; generateID always emits long enough).
func KeyPrefix(plaintext string) string {
	const prefixLen = 12
	if len(plaintext) < prefixLen {
		return ""
	}
	return plaintext[:prefixLen]
}

// generateID reads n bytes from rnd and returns prefix + hex(bytes).
func generateID(rnd func([]byte) (int, error), prefix string, n int) (string, error) {
	buf := make([]byte, n)
	read, err := rnd(buf)
	if err != nil {
		return "", err
	}
	if read != n {
		return "", fmt.Errorf("auth: short read from entropy source: got %d want %d", read, n)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// Compile-time check.
var _ APIKeyStore = (*RedisAPIKeyStore)(nil)
