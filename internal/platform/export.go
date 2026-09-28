package platform

import (
	"encoding/json"
	"time"
)

// AccountExportSchemaVersion versions the [AccountExport] document.
const AccountExportSchemaVersion = 1

// AccountExport is the document GET /v1/dashboard/account/export serves:
// everything the service holds about an account, as the requesting owner
// may see it. Secrets (key hashes, webhook signing keys, session token
// hashes, passkey public keys, MFA material) are never included, and
// other members' sessions, passkeys, addresses and agents are left out:
// they are those members' data, not the requester's.
//
// jsonb columns are carried as json.RawMessage so no number in them is
// ever decoded into a float64; NUMERIC columns are decimal strings.
type AccountExport struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`

	Account     ExportAccount       `json:"account"`
	Users       []ExportUser        `json:"users"`
	Sessions    []ExportSession     `json:"sessions"`
	Passkeys    []ExportPasskey     `json:"passkeys"`
	APIKeys     []ExportAPIKey      `json:"api_keys"`
	Webhooks    []ExportWebhook     `json:"webhooks"`
	PriceAlerts []ExportPriceAlert  `json:"price_alerts"`
	Invites     []ExportInvite      `json:"invites"`
	Usage       []ExportUsageDay    `json:"usage"`
	AuditLog    []ExportAuditRecord `json:"audit_log"`
}

// ExportAccount is the account row.
type ExportAccount struct {
	ID                          string     `json:"id"`
	Name                        string     `json:"name"`
	Slug                        string     `json:"slug"`
	BillingEmail                string     `json:"billing_email"`
	Tier                        string     `json:"tier"`
	Status                      string     `json:"status"`
	CreatedAt                   time.Time  `json:"created_at"`
	SuspendedAt                 *time.Time `json:"suspended_at,omitempty"`
	SuspendedReason             string     `json:"suspended_reason,omitempty"`
	RateLimitPerMinOverride     *int64     `json:"rate_limit_per_min_override,omitempty"`
	MonthlyRequestQuotaOverride *int64     `json:"monthly_request_quota_override,omitempty"`
}

// ExportUser is one member. MFA secrets and recovery-code hashes are
// never exported.
type ExportUser struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"display_name,omitempty"`
	Role            string     `json:"role"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
	MFAEnabled      bool       `json:"mfa_enabled"`
	CreatedAt       time.Time  `json:"created_at"`
	IsRequester     bool       `json:"is_requester"`
}

// ExportSession is one of the requester's dashboard sessions.
type ExportSession struct {
	ID           string     `json:"id"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	IPFirstSeen  string     `json:"ip_first_seen"`
	IPLastSeen   string     `json:"ip_last_seen"`
	UserAgent    string     `json:"user_agent"`
	GeoFirstSeen string     `json:"geo_first_seen,omitempty"`
	GeoLastSeen  string     `json:"geo_last_seen,omitempty"`
}

// ExportPasskey is one of the requester's passkeys.
type ExportPasskey struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	AAGUID         string     `json:"aaguid,omitempty"`
	Transports     []string   `json:"transports"`
	BackupEligible bool       `json:"backup_eligible"`
	BackupState    bool       `json:"backup_state"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

// ExportAPIKey is one API key, from Postgres or the Redis validator store.
type ExportAPIKey struct {
	ID                string     `json:"id"`
	Store             string     `json:"store"`
	Prefix            string     `json:"prefix"`
	Name              string     `json:"name"`
	Tier              string     `json:"tier"`
	Scopes            []string   `json:"scopes"`
	RateLimitPerMin   int        `json:"rate_limit_per_min"`
	MonthlyQuota      int64      `json:"monthly_quota,omitempty"`
	IPAllowlist       []string   `json:"ip_allowlist,omitempty"`
	RefererAllowlist  []string   `json:"referer_allowlist,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	RevokedReason     string     `json:"revoked_reason,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP        string     `json:"last_used_ip,omitempty"`
	LastUsedUserAgent string     `json:"last_used_user_agent,omitempty"`
}

// ExportWebhook is one customer webhook with its retained deliveries.
type ExportWebhook struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	URL        string                  `json:"url"`
	Events     []string                `json:"events"`
	Enabled    bool                    `json:"enabled"`
	CreatedAt  time.Time               `json:"created_at"`
	UpdatedAt  time.Time               `json:"updated_at"`
	Deliveries []ExportWebhookDelivery `json:"deliveries"`
}

// ExportWebhookDelivery is one retained delivery attempt record.
type ExportWebhookDelivery struct {
	ID                 string          `json:"id"`
	EventType          string          `json:"event_type"`
	Payload            json.RawMessage `json:"payload"`
	AttemptCount       int             `json:"attempt_count"`
	DeliveredAt        *time.Time      `json:"delivered_at,omitempty"`
	LastError          string          `json:"last_error,omitempty"`
	LastResponseStatus int             `json:"last_response_status,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

// ExportPriceAlert is one price alert. Threshold is the NUMERIC column
// as a decimal string (ADR-0003).
type ExportPriceAlert struct {
	ID              string     `json:"id"`
	BaseAsset       string     `json:"base_asset"`
	QuoteAsset      string     `json:"quote_asset"`
	Condition       string     `json:"condition"`
	Threshold       string     `json:"threshold"`
	CooldownSeconds int        `json:"cooldown_seconds"`
	Enabled         bool       `json:"enabled"`
	LastFiredAt     *time.Time `json:"last_fired_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ExportInvite is one invite the account sent.
type ExportInvite struct {
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ExportUsageDay is one usage_daily row.
type ExportUsageDay struct {
	Day              string `json:"day"`
	Subject          string `json:"subject"`
	Endpoint         string `json:"endpoint"`
	OKCount          int64  `json:"ok_count"`
	ClientErrorCount int64  `json:"client_error_count"`
	ServerErrorCount int64  `json:"server_error_count"`
	ThrottledCount   int64  `json:"throttled_count"`
}

// ExportAuditRecord is one audit_log row about the account. IP and user
// agent are present only on the requester's own actions; staff identity
// (actor_email, session_id) is removed from metadata.
type ExportAuditRecord struct {
	Timestamp   time.Time       `json:"ts"`
	ActorKind   string          `json:"actor_kind"`
	ByRequester bool            `json:"by_requester"`
	Action      string          `json:"action"`
	TargetKind  string          `json:"target_kind,omitempty"`
	TargetID    string          `json:"target_id,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	IP          string          `json:"ip,omitempty"`
	UserAgent   string          `json:"user_agent,omitempty"`
}
