package auth

import (
	"context"
	"net/netip"
	"time"
)

// Tier identifies the rate-limit + feature-access bucket a caller belongs to. The string values
// appear in metrics labels and the rate-limit key, so changing them is a wire break.
type Tier string

const (
	// TierAnonymous: no credential presented (or auth_mode=none). Lowest budget; the default.
	TierAnonymous Tier = "anonymous"

	// TierAPIKey: caller presented a valid API key.
	TierAPIKey Tier = "apikey"

	// TierSEP10: caller presented a valid SEP-10 JWT.
	TierSEP10 Tier = "sep10"

	// TierOperator: internal operator credential for admin endpoints; never granted to public callers.
	TierOperator Tier = "operator"
)

// String makes Tier usable as a Prometheus label value.
func (t Tier) String() string { return string(t) }

// Subject is the authenticated identity on a request, set before the rate-limit middleware so
// per-tier limits work; for anonymous requests Identifier is a RemoteIP + User-Agent hash.
type Subject struct {
	// Identifier is the stable identity (G-strkey for SEP-10, owner-account reference for API
	// keys) or, for anonymous, a per-IP+UA hash that is not stable across deploys.
	Identifier string

	// Tier drives rate-limit budget and feature gating.
	Tier Tier

	// Scopes is an optional platform.KeyScope* capability list; EMPTY means full access, non-empty
	// confines the key to the listed route families (enforced via RequiredScope + HasScope).
	Scopes []string

	// KeyID is the public-safe credential identifier (APIKeyRecord.KeyID, distinct from the secret
	// hash); empty for anonymous and SEP-10 subjects.
	KeyID string

	// RateLimitPerMin: zero means the deployment default for the tier; non-zero overrides per key.
	RateLimitPerMin int

	// CreatedAt is when the credential was issued; zero for anonymous.
	CreatedAt time.Time

	// Label is the customer-supplied credential name shown via /v1/account/me; never consulted by
	// auth or rate-limit logic.
	Label string

	// KeyPrefix is the first 12 chars of the plaintext key (e.g. `sip_4f9c1d8b`), shown in dashboard
	// listings to identify a key. Empty on records minted without one and on anonymous subjects.
	KeyPrefix string

	// IPAllowlist: when non-empty, the resolved client IP must fall in at least one prefix or the
	// request is rejected with 403.
	IPAllowlist []netip.Prefix

	// RefererAllowlist: when non-empty, the Referer host must exactly match an entry or the request
	// gets 403; empty disables it. NOT a theft control: Referer is client-settable, so it only
	// scopes BROWSERS embedding the key. IPAllowlist is the real control (trusted-proxy address).
	RefererAllowlist []string

	// AllowAllPermissions skips the per-endpoint permission check; when false the Allow/Deny
	// lists below decide.
	AllowAllPermissions bool

	// AllowPermissions holds exact (METHOD PATH) or path-prefix grants; non-empty with
	// AllowAllPermissions false enables allow-list mode.
	AllowPermissions []SubjectPermissionEntry

	// DenyPermissions deny even when the allow-list would admit; always consulted, even when
	// AllowAllPermissions is true.
	DenyPermissions []SubjectPermissionEntry

	// MonthlyQuota: when > 0, middleware.MonthlyQuota 429s the Subject once the calendar-month
	// count reaches it; zero disables. Both validators populate it, resolved against the account
	// override.
	MonthlyQuota int64

	// EmailVerifiedAt is when the customer proved their signup email (zero = never);
	// RequireEmailVerified answers an unverified `signup-` Subject with 403
	// signup-verify-required, and the field is informational without that middleware.
	EmailVerifiedAt time.Time

	// ExpiresAt is when the presented API key stops authenticating; zero means never (also for
	// anonymous and SEP-10 subjects). [ChildKeyRequest] copies it so a time-boxed key cannot mint
	// itself a permanent successor.
	ExpiresAt time.Time
}

// SubjectPermissionEntry mirrors platform.KeyPermissionEntry so auth need not import platform:
// either Endpoint (exact `<METHOD> <PATH>`) or EndpointPrefix, never both. The JSON tags are
// required: the Redis cache round-trips this type, and dropping them would silently bypass
// KeyPolicy on cache hits.
type SubjectPermissionEntry struct {
	Endpoint       string `json:"endpoint,omitempty"`
	EndpointPrefix string `json:"endpoint_prefix,omitempty"`
}

// Anonymous returns the subject attached when no credential is presented (or auth_mode=none); the
// identifier is typically a RemoteIP + UA hash, which the rate limiter keys on.
func Anonymous(identifier string) Subject {
	return Subject{Identifier: identifier, Tier: TierAnonymous}
}

// ─── Context helpers ──────────────────────────────────────────────

// subjectKeyType is unexported so callers cannot read or write a Subject under the wrong key.
type subjectKeyType struct{}

var subjectKey subjectKeyType

// WithSubject returns a context carrying the Subject; read it back via [SubjectFrom].
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, subjectKey, s)
}

// SubjectFrom extracts the Subject from ctx; false means none was attached (always attached after
// the auth middleware), to be treated as anonymous-with-empty-id.
func SubjectFrom(ctx context.Context) (Subject, bool) {
	v := ctx.Value(subjectKey)
	if v == nil {
		return Subject{}, false
	}
	s, ok := v.(Subject)
	return s, ok
}
