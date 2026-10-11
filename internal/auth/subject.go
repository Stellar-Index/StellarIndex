package auth

import (
	"context"
	"net/netip"
	"time"
)

// Tier identifies the rate-limit + feature-access bucket a caller
// belongs to. Stable string values — appear in metrics labels and
// in the rate-limit middleware key, so changing them is a wire
// break.
type Tier string

const (
	// TierAnonymous — no credential presented (or auth_mode=none).
	// Lowest rate-limit budget. Default.
	TierAnonymous Tier = "anonymous"

	// TierAPIKey — caller presented a valid API key.
	TierAPIKey Tier = "apikey"

	// TierSEP10 — caller presented a valid SEP-10 JWT.
	TierSEP10 Tier = "sep10"

	// TierOperator — internal operator credential. Reserved for
	// admin endpoints (`/v1/account/*` self-service + future
	// /v1/admin/*); never granted to public callers.
	TierOperator Tier = "operator"
)

// String makes Tier usable as a Prometheus label value without a
// type assertion.
func (t Tier) String() string { return string(t) }

// Subject is the authenticated identity on a request, set before the rate-limit middleware so
// per-tier limits work; for anonymous requests Identifier is a RemoteIP + User-Agent hash.
type Subject struct {
	// Identifier is the stable identity (G-strkey for SEP-10, owner-account reference for API
	// keys) or, for anonymous, a per-IP+UA hash that is not stable across deploys.
	Identifier string

	// Tier — see above. Drives rate-limit budget + feature gating.
	Tier Tier

	// Scopes is an optional platform.KeyScope* capability list; EMPTY means full access, non-empty
	// confines the key to the listed route families (enforced via RequiredScope + HasScope).
	Scopes []string

	// KeyID is the public-safe credential identifier (APIKeyRecord.KeyID, distinct from the secret
	// hash); empty for anonymous and SEP-10 subjects.
	KeyID string

	// RateLimitPerMin — per-tier budget the rate-limit middleware
	// applies. Zero means "use the deployment default for this
	// tier"; a non-zero value overrides at the per-key level
	// (paid customers on a custom plan).
	RateLimitPerMin int

	// CreatedAt — when the credential was issued. Zero for
	// anonymous. Surfaced via /v1/account/me; not load-bearing
	// elsewhere.
	CreatedAt time.Time

	// Label is the customer-supplied credential name shown via /v1/account/me; never consulted by
	// auth or rate-limit logic.
	Label string

	// KeyPrefix — first 12 chars of the plaintext key (e.g.
	// `sip_4f9c1d8b`). Empty on records minted without a prefix and
	// on anonymous subjects. Customers see this in dashboard listings to
	// identify which key matches a row in their secret manager.
	KeyPrefix string

	// IPAllowlist — when non-empty, the request's resolved client
	// IP must fall in at least one prefix or the request is
	// rejected with 403. Populated by both API-key validators from
	// the key record.
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

	// ExpiresAt — when the presented API key stops authenticating. Zero
	// means the credential never expires (and for anonymous / SEP-10
	// subjects, which have no key record). [ChildKeyRequest] copies it so
	// a time-boxed key cannot mint itself a permanent successor.
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

// Anonymous returns the subject the middleware attaches when no
// credential is presented (or auth_mode=none). The identifier is
// caller-supplied so the rate-limit middleware can key on it
// (typically RemoteIP + UA hash).
func Anonymous(identifier string) Subject {
	return Subject{Identifier: identifier, Tier: TierAnonymous}
}

// ─── Context helpers ──────────────────────────────────────────────

// subjectKeyType is unexported so callers can't accidentally read
// or write a Subject under the wrong key type.
type subjectKeyType struct{}

var subjectKey subjectKeyType

// WithSubject returns a new context carrying the Subject. The auth
// middleware calls this on every authenticated request; downstream
// handlers + middleware (rate-limit, request logger) read it back
// via [SubjectFrom].
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, subjectKey, s)
}

// SubjectFrom extracts the Subject from ctx. The boolean is `true`
// when a subject was attached (always the case after the auth
// middleware runs). Tests that bypass middleware should expect
// `false` and treat that as anonymous-with-empty-id.
func SubjectFrom(ctx context.Context) (Subject, bool) {
	v := ctx.Value(subjectKey)
	if v == nil {
		return Subject{}, false
	}
	s, ok := v.(Subject)
	return s, ok
}
