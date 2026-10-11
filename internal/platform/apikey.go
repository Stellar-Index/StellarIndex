package platform

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// APIKey is a dashboard-managed API key; the plaintext is NEVER stored, KeyHash is its sha256 and KeyPrefix the first 12 chars (safe to display).
type APIKey struct {
	ID                     string // "kid_<hex>"
	AccountID              uuid.UUID
	CreatedByUserID        uuid.UUID // zero when system-minted (e.g. /v1/signup pre-dashboard)
	Name                   string
	Description            string
	KeyHash                []byte
	KeyPrefix              string
	Tier                   APIKeyTier
	RateLimitPerMin        int
	MonthlyQuota           int64 // 0 = inherit from plan
	Permissions            KeyPermissions
	Scopes                 []string // capability scopes (see KeyScope*); empty = full access (legacy posture)
	IPAllowlist            []netip.Prefix
	RefererAllowlist       []string
	ExpiresAt              time.Time // zero = no expiry
	RevokedAt              time.Time
	RevokedByUserID        uuid.UUID
	RevokedReason          string
	LastUsedAt             time.Time
	LastUsedIP             net.IP
	LastUsedUserAgent      string
	UsageAlertThresholdPct int // 0 = no alert; valid range 1..100
	CreatedAt              time.Time
}

// APIKeyTier is the tier the key authenticates as. Distinct
// from [Tier] (the account's billing tier) — a key can be
// pinned to a specific runtime tier independent of the account
// plan, e.g. for operator credentials or partner integrations.
type APIKeyTier string

const (
	APIKeyTierAPIKey   APIKeyTier = "apikey"   // standard customer key
	APIKeyTierPartner  APIKeyTier = "partner"  // higher rate limit, custom contract
	APIKeyTierOperator APIKeyTier = "operator" // staff-issued; unlocks admin endpoints
)

// Key capability scopes: coarse route-family grants (read, account, dashboard, admin). An EMPTY scope list retains full access; a key WITH scopes is confined to the listed families (enforced by the KeyPolicy middleware).
const (
	KeyScopeRead      = "read"      // public data surfaces (price, history, chart, assets, explorer, …)
	KeyScopeAccount   = "account"   // /v1/account/* self-service
	KeyScopeDashboard = "dashboard" // /v1/dashboard/* management surface
	KeyScopeAdmin     = "admin"     // /v1/admin/* operator surface
)

// KnownKeyScopes lists the accepted scope vocabulary, for
// validation errors and docs.
func KnownKeyScopes() []string {
	return []string{KeyScopeRead, KeyScopeAccount, KeyScopeDashboard, KeyScopeAdmin}
}

// ValidKeyScope reports whether s is one of the known capability
// scopes. The wildcard "*" is NOT accepted at mint time — an empty
// list already means full access, so persisting "*" would just be
// a second spelling of the same thing.
func ValidKeyScope(s string) bool {
	switch s {
	case KeyScopeRead, KeyScopeAccount, KeyScopeDashboard, KeyScopeAdmin:
		return true
	default:
		return false
	}
}

// KeyPermissions is the per-key capability set, stored as JSONB; the default {All: true} enforces nothing, and Allow/Deny restrict a key per endpoint via middleware KeyPolicy.
type KeyPermissions struct {
	All   bool                 `json:"all"`
	Allow []KeyPermissionEntry `json:"allow,omitempty"`
	Deny  []KeyPermissionEntry `json:"deny,omitempty"`
}

// KeyPermissionEntry matches one allow/deny rule. Either Endpoint
// (exact pattern, e.g. "GET /v1/price") or EndpointPrefix
// (e.g. "/v1/account/") — never both.
type KeyPermissionEntry struct {
	Endpoint       string `json:"endpoint,omitempty"`
	EndpointPrefix string `json:"endpoint_prefix,omitempty"`
}

// IsActive reports whether the key is currently valid: not
// revoked, not expired (or expiry zero), and assumes the parent
// account is active (caller is responsible for checking that).
func (k APIKey) IsActive(now time.Time) bool {
	if !k.RevokedAt.IsZero() {
		return false
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return false
	}
	return true
}

// APIKeyStore persists [APIKey] in Postgres. The runtime auth
// validator will consult this store via a Redis-cached read-
// through wrapper once the Phase 1 Week 4 cutover ships.
type APIKeyStore interface {
	// Create inserts a pre-hashed key; ErrConflict on hash/ID collision, [ErrAPIKeyQuotaExceeded] when maxActiveKeysPerAccount is met (zero or negative disables the cap).
	Create(ctx context.Context, k APIKey, maxActiveKeysPerAccount int) (APIKey, error)

	// Get by key ID; ErrNotFound if absent.
	Get(ctx context.Context, id string) (APIKey, error)

	// GetByHash is the hot path used by the auth validator.
	// ErrNotFound if absent; caller checks IsActive on the
	// returned record.
	GetByHash(ctx context.Context, keyHash []byte) (APIKey, error)

	// CountActiveForAccount returns the number of non-revoked keys
	// the account holds — the figure the active-key cap gates on.
	CountActiveForAccount(ctx context.Context, accountID uuid.UUID) (int, error)

	// ListActiveForAccount returns every non-revoked key, sorted
	// CreatedAt asc. Bounded by the active-key cap, so it is safe on
	// the tier-clamp and kill-switch paths; revoked history is never read.
	ListActiveForAccount(ctx context.Context, accountID uuid.UUID) ([]APIKey, error)

	// ListForAccount returns every active key plus at most revokedLimit recent revoked keys (moreRevoked reports omitted ones); revoked rows are kept forever, so never read the whole history.
	ListForAccount(ctx context.Context, accountID uuid.UUID, revokedLimit int) (keys []APIKey, moreRevoked bool, err error)

	// Update writes the editable fields; a key not owned by accountID is ErrNotFound, like an absent one.
	Update(ctx context.Context, accountID uuid.UUID, k APIKey) error

	// Revoke soft-deletes by setting revoked_at + reason. Only a key
	// owned by accountID is revoked; any other key is ErrNotFound.
	// Idempotent: revoking an already-revoked key is a no-op.
	Revoke(ctx context.Context, accountID uuid.UUID, id string, byUserID uuid.UUID, reason string) error

	// TouchUsage updates LastUsedAt + LastUsedIP + LastUsedUserAgent.
	// id must be the authenticated credential's own key ID, never a
	// request-supplied one: a key only ever touches itself. Debounced
	// caller-side, by default to once per auth.DefaultTouchDebounceTTL.
	TouchUsage(ctx context.Context, id string, ip net.IP, userAgent string) error
}
