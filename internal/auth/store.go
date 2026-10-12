package auth

import (
	"context"
	"crypto/rand"
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

// APIKeyStore is the writer side of API-key persistence (APIKeyValidator is the read path, kept separate so the hot path does not expose the mutating surface). Implementations must be safe for concurrent use.
type APIKeyStore interface {
	// Create issues a fresh key from req (the store fills KeyID and CreatedAt and generates the plaintext).
	// The plaintext is returned ONCE and is unrecoverable; callers must not log or persist it beyond the one-shot response.
	Create(ctx context.Context, req CreateAPIKeyRequest) (rec APIKeyRecord, plaintext string, err error)
}

// CreateAPIKeyRequest is the payload [APIKeyStore.Create] consumes; the store ignores fields it computes (KeyID, CreatedAt).
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

	// MonthlyQuota is the billable-request ceiling middleware.MonthlyQuota enforces; zero means inherit the identifier's existing ceiling (0 only if it has none), never an invented cap.
	// Self-service rotation passes the caller's EFFECTIVE quota: a stored record is read back without the Postgres account-override cascade, so copying a per-key 0 would ship the child unmetered.
	MonthlyQuota int64

	// ExpiresAt — zero means never. The self-service rotation path sets
	// it to the caller's own expiry via [ChildKeyRequest].
	ExpiresAt time.Time

	// EmailVerifiedAt zero means the owner never proved their signup email. Rotation copies the parent's stamp: verification belongs to the identifier's owner, and no path verifies a non-signup KeyID later.
	EmailVerifiedAt time.Time

	// MintedBy is the authenticated credential issuing this key. When
	// set, Create refuses a request exceeding it ([ClampToMinter]). Nil
	// only for mints with no HTTP subject: signup and the operator CLI.
	MintedBy *Subject

	// SelfService writes the record with the sliding [MirroredKeyIdleTTL]
	// instead of no expiry: a caller-minted child or signup key must age
	// out once abandoned, while operator-issued keys stay persistent.
	SelfService bool
}

// ErrMintExceedsCaller is [ClampToMinter]'s refusal: the request asked for more authority than the minting credential holds.
var ErrMintExceedsCaller = errors.New("auth: mint exceeds the minting credential's own authority")

// ClampToMinter ensures a credential mints no child more privileged than itself (scopes must be a HasScope subset and are refused, not dropped; a scoped minter cannot exceed its own rate limit), returning the scopes to persist or an error wrapping ErrMintExceedsCaller.
func ClampToMinter(minter Subject, scopes []string, rateLimitPerMin int) ([]string, error) {
	if rateLimitPerMin > 0 {
		if minter.RateLimitPerMin > 0 && rateLimitPerMin > minter.RateLimitPerMin {
			return nil, fmt.Errorf("%w: rate_limit_per_min %d exceeds this key's own %d",
				ErrMintExceedsCaller, rateLimitPerMin, minter.RateLimitPerMin)
		}
		if minter.RateLimitPerMin == 0 && len(minter.Scopes) > 0 {
			return nil, fmt.Errorf("%w: a scoped key on the deployment-default rate limit may only mint keys on that default (rate_limit_per_min 0)",
				ErrMintExceedsCaller)
		}
	}
	if len(minter.Scopes) == 0 {
		return scopes, nil
	}
	if len(scopes) == 0 {
		return append([]string(nil), minter.Scopes...), nil
	}
	for _, s := range scopes {
		if !minter.HasScope(s) {
			return nil, fmt.Errorf("%w: scope %q exceeds this key's own scopes (%s) — a key may only mint a child with a subset of its scopes",
				ErrMintExceedsCaller, s, strings.Join(minter.Scopes, ", "))
		}
	}
	return scopes, nil
}

// ChildKeyRequest builds the mint request for a key parent delegates to itself, the one place every delegated dimension is copied; the store re-checks scopes via MintedBy (ClampToMinter).
func ChildKeyRequest(parent Subject, label string, scopes []string) CreateAPIKeyRequest {
	return CreateAPIKeyRequest{
		MintedBy:        &parent,
		Identifier:      parent.Identifier,
		Label:           label,
		Tier:            parent.Tier,
		Scopes:          scopes,
		RateLimitPerMin: parent.RateLimitPerMin,
		MonthlyQuota:    parent.MonthlyQuota,
		ExpiresAt:       parent.ExpiresAt,
		EmailVerifiedAt: parent.EmailVerifiedAt,
		SelfService:     true,
	}
}

// RedisAPIKeyStore implements APIKeyStore over the `apikey:<sha256-hex>` records the validator reads; it generates the plaintext (`sip_` + 32 crypto/rand bytes hex) and a KeyID (`kid_`) that is distinct from the secret hash and safe to log.
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

// WithStoreClock overrides the time source for CreatedAt stamping, for tests.
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *RedisAPIKeyStore) { s.now = now }
}

// withRandRead overrides the entropy source. Internal — tests use
// it to pin generated bytes; production has no reason to override
// crypto/rand.
func withRandRead(fn func([]byte) (int, error)) StoreOption {
	return func(s *RedisAPIKeyStore) { s.randRead = fn }
}

// NewRedisAPIKeyStore constructs a store; rdb must be non-nil. It is wired only when Redis is reachable (otherwise the Noop validator 503s and issuance is not mounted).
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

// MaxKeyRateLimitPerMin is the highest per-key budget any surface may set: the Partner plan ceiling.
const MaxKeyRateLimitPerMin = 100_000

// ValidateKeyBounds is the store-side floor every mint and re-budget path inherits, whichever surface called it.
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

// Create implements APIKeyStore: requires an Identifier, generates KeyID and plaintext, and SETs apikey:<sha256(plaintext)> (with MirroredKeyIdleTTL when req.SelfService); on Redis failure the plaintext is not surfaced.
func (s *RedisAPIKeyStore) Create(ctx context.Context, req CreateAPIKeyRequest) (APIKeyRecord, string, error) {
	if req.Identifier == "" {
		return APIKeyRecord{}, "", errors.New("auth: Create: Identifier is required")
	}
	if err := ValidateKeyBounds(req.RateLimitPerMin, req.Scopes); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: %w", err)
	}
	tier := req.Tier
	if tier == "" {
		tier = TierAPIKey
	}
	if !mintableTier(tier) {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: tier %q cannot hold a key (want %s, %s or %s)", tier, TierAPIKey, TierSEP10, TierOperator)
	}
	if req.MintedBy != nil {
		scopes, err := ClampToMinter(*req.MintedBy, req.Scopes, req.RateLimitPerMin)
		if err != nil {
			return APIKeyRecord{}, "", fmt.Errorf("auth: Create: %w", err)
		}
		req.Scopes = scopes
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
	// human-facing namespace label.
	plaintext, err := generateID(s.randRead, "sip_", 32)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: generate plaintext: %w", err)
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
		// Operator-minted keys are full-access by default (Permissions.All=true): with no allow entries and PermissionsAll=false the permission middleware's closed posture 403s every request from the new key.
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
	// Expiry and revocation live in the JSON record; the Redis TTL only bounds how long an unused self-service key occupies the keyspace, and the validator slides it on each Lookup.
	// The record and its lookup-index entries land as one atomic write (writeRecord).
	ttl := cachekeys.APIKeyTTL
	if req.SelfService {
		ttl = MirroredKeyIdleTTL
	}
	if err := s.writeRecord(ctx, hash, rec, body, ttl); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: Create: redis set: %w", err)
	}
	return rec, plaintext, nil
}

// mintableTier reports whether a minted record may carry t. Anonymous is
// not a credential tier, and an unrecognised one has no rate-limit policy.
func mintableTier(t Tier) bool {
	switch t {
	case TierAPIKey, TierSEP10, TierOperator:
		return true
	case TierAnonymous:
		return false
	default:
		return false
	}
}

// KeyQuotaExceededError is [RedisAPIKeyStore.CreateCapped]'s refusal: the identifier already holds Active un-revoked keys against a ceiling of Max.
type KeyQuotaExceededError struct{ Active, Max int }

func (e *KeyQuotaExceededError) Error() string {
	return fmt.Sprintf("auth: identifier holds %d active API keys (max %d)", e.Active, e.Max)
}

// ErrKeyQuotaUnavailable wraps every CreateCapped failure that left the ceiling unverified (lock or count), as distinct from a failed write.
var ErrKeyQuotaUnavailable = errors.New("auth: key quota could not be verified")

// ErrKeyMintBusy means another capped mint for the same identifier held the mint lock for the whole wait; retryable.
var ErrKeyMintBusy = errors.New("auth: another key mint for this identifier is in progress")

const (
	// keyMintLockTTL bounds how long a crashed minter blocks its
	// identifier; a mint is a few round trips.
	keyMintLockTTL = 10 * time.Second
	// keyMintLockWait is how long a mint queues behind another before
	// giving up with ErrKeyMintBusy.
	keyMintLockWait = 3 * time.Second
	keyMintLockPoll = 25 * time.Millisecond
)

// releaseMintLockScript deletes the lock only while it still holds this
// minter's token, so a mint that outlived the TTL cannot free a lock a
// later minter now owns.
var releaseMintLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

// CreateCapped is Create with the identifier's active-key ceiling enforced under a per-identifier lock shared across API instances; a lock or count failure refuses the mint.
func (s *RedisAPIKeyStore) CreateCapped(ctx context.Context, req CreateAPIKeyRequest, maxActive int) (APIKeyRecord, string, error) {
	if req.Identifier == "" {
		return APIKeyRecord{}, "", errors.New("auth: CreateCapped: Identifier is required")
	}
	release, err := s.lockMint(ctx, req.Identifier)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: CreateCapped: %w: %w", ErrKeyQuotaUnavailable, err)
	}
	defer release()

	existing, err := s.ListKeysForIdentifier(ctx, req.Identifier)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: CreateCapped: %w: count keys: %w", ErrKeyQuotaUnavailable, err)
	}
	active := 0
	for _, rec := range existing {
		if rec.RevokedAt.IsZero() {
			active++
		}
	}
	if active >= maxActive {
		return APIKeyRecord{}, "", &KeyQuotaExceededError{Active: active, Max: maxActive}
	}
	return s.Create(ctx, req)
}

// lockMint takes the identifier's mint lock, polling up to
// keyMintLockWait, and returns its release.
func (s *RedisAPIKeyStore) lockMint(ctx context.Context, identifier string) (func(), error) {
	key := cachekeys.APIKeyMintLock(identifier).String()
	token, err := generateID(s.randRead, "", 16)
	if err != nil {
		return nil, fmt.Errorf("mint lock token: %w", err)
	}
	wait := time.NewTimer(keyMintLockWait)
	defer wait.Stop()
	for {
		won, err := s.rdb.SetNX(ctx, key, token, keyMintLockTTL).Result()
		if err != nil {
			return nil, fmt.Errorf("mint lock: %w", err)
		}
		if won {
			return func() {
				// Detached: a caller that hung up must still free the lock.
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cancel()
				_ = releaseMintLockScript.Run(rctx, s.rdb, []string{key}, token).Err()
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait.C:
			return nil, ErrKeyMintBusy
		case <-time.After(keyMintLockPoll):
		}
	}
}

// inheritedMonthlyQuota is the ceiling a key minted without one takes: the most generous LIVE credential's (a live unmetered one means 0), else the most generous lapsed one,
// so a mint can neither lift nor tighten the plan and letting every key lapse cannot reset a metered identifier to unmetered.
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

// KeyPrefix returns the first 12 chars of a plaintext key (`sip_` plus 8 hex), safe to log (32 bits is not authentication material) and shown in dashboard listings; "" for shorter input.
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
