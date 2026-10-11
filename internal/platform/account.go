package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Tier is the capability bucket an account sits in: anon (unauthenticated per-IP baseline, never persisted), free (registered default) or partner (staff-admitted); legacy `accounts.tier` strings fold via [Tier.Canonical] on read and [Tier.StorageValue] on write.
type Tier string

const (
	// TierAnon is the unauthenticated baseline. Documentation-level
	// only — never stored on an account row (StorageValue folds it to
	// "free" defensively).
	TierAnon Tier = "anon"
	// TierFree is the registered-account default.
	TierFree Tier = "free"
	// TierPartner marks a staff-admitted account whose real limits
	// live in the per-account overrides.
	TierPartner Tier = "partner"
)

// Legacy pre-free-platform tier strings still present in `accounts.tier` rows; Canonical() folds them into the three-level model.
const (
	TierStarter    Tier = "starter"    // → free (identical numbers)
	TierPro        Tier = "pro"        // → partner
	TierBusiness   Tier = "business"   // → partner
	TierEnterprise Tier = "enterprise" // → partner
)

// KnownTier reports whether s is an accepted account-tier string (canonical or legacy); anon is deliberately NOT accepted since it is never an account tier.
func KnownTier(s string) bool {
	switch Tier(s) {
	case TierFree, TierPartner,
		TierStarter, TierPro, TierBusiness, TierEnterprise:
		return true
	default:
		return false
	}
}

// Canonical maps any tier string into the three-level model: starter folds to free, pro/business/enterprise to partner, and unknown values to free so a corrupt row never unlocks elevated budgets.
func (t Tier) Canonical() Tier {
	switch t {
	case TierAnon:
		return TierAnon
	case TierPartner, TierPro, TierBusiness, TierEnterprise:
		return TierPartner
	default: // TierFree, TierStarter, unknown
		return TierFree
	}
}

// StorageValue renders the tier as a string the migration-0027 CHECK accepts (partner as "enterprise", everything else "free"), so Tier(t.StorageValue()).Canonical() == t.Canonical().
func (t Tier) StorageValue() string {
	if t.Canonical() == TierPartner {
		return string(TierEnterprise)
	}
	return string(TierFree)
}

// MaxRateLimitPerMin returns the per-tier ceiling (anon 60, free 1000, partner 100_000) on a dashboard-minted key's `rate_limit_per_min`; the staff-set account override is the real partner limit.
func (t Tier) MaxRateLimitPerMin() int {
	switch t.Canonical() {
	case TierAnon:
		return 60
	case TierPartner:
		return 100_000
	default: // TierFree + any unknown value
		return 1000
	}
}

// MaxActiveKeys returns the default per-tier ceiling on concurrently active API keys (dashboard config may override); unknown tiers are treated as free.
func (t Tier) MaxActiveKeys() int {
	switch t.Canonical() {
	case TierAnon:
		return 0 // anonymous callers hold no keys by definition
	case TierPartner:
		return 250
	default: // TierFree + any unknown value
		return 25
	}
}

// MaxMonthlyQuota returns the per-tier ceiling on a dashboard-minted key's `monthly_quota`; the dashboard clamps to min(requested, override-or-this-ladder) so a customer can only LOWER their cap. Unknown tiers are treated as free.
func (t Tier) MaxMonthlyQuota() int64 {
	switch t.Canonical() {
	case TierAnon:
		return 0 // anon traffic is per-minute limited, never monthly-metered
	case TierPartner:
		return 1_000_000_000
	default: // TierFree + any unknown value
		return 1_000_000
	}
}

// MaxWebhooks returns the per-tier ceiling on registered webhook
// endpoints. Replaces the dashboard's flat 10-webhook cap; override
// seam is dashboardwebhooks.Config.WebhookQuotas. Unknown tiers are
// treated as free, same posture as the other tier ladders.
func (t Tier) MaxWebhooks() int {
	switch t.Canonical() {
	case TierAnon:
		return 0
	case TierPartner:
		return 100
	default: // TierFree + any unknown value
		return 10
	}
}

// MaxPriceAlerts returns the per-tier ceiling on registered price alerts; unknown tiers are treated as free.
func (t Tier) MaxPriceAlerts() int {
	switch t.Canonical() {
	case TierAnon:
		return 0
	case TierPartner:
		return 1000
	default: // TierFree + any unknown value
		return 25
	}
}

// AccountStatus is the lifecycle state of an account.
type AccountStatus string

const (
	AccountActive    AccountStatus = "active"
	AccountSuspended AccountStatus = "suspended"
	AccountClosed    AccountStatus = "closed"
)

// Bounds the accounts CHECK constraints enforce (migration 0027):
// length(name) BETWEEN 1 AND 200 counts characters, and the slug
// pattern ^[a-z0-9][a-z0-9-]{0,62}$ admits at most 63 bytes. Code
// that derives either field from user input must stay inside them.
const (
	MaxAccountNameLen = 200
	MaxAccountSlugLen = 63
)

// Account is the top-level org primitive; every API key, webhook and audit-log entry hangs off Account.ID (v1 is one user per account).
type Account struct {
	ID                          uuid.UUID
	Name                        string
	Slug                        string
	BillingEmail                string
	Tier                        Tier
	Status                      AccountStatus
	CreatedAt                   time.Time
	SuspendedAt                 time.Time // zero when not suspended
	SuspendedReason             string
	RateLimitPerMinOverride     int   // 0 = inherit tier default
	MonthlyRequestQuotaOverride int64 // 0 = inherit tier default
}

// DashboardKeyDefaultRateLimitPerMin is the rate_limit_per_min a
// dashboard-minted key gets when the create request leaves it unset.
const DashboardKeyDefaultRateLimitPerMin = 1000

// ResolveKeyRateLimitPerMin is the per-minute limit auth enforces for
// one of this account's keys: the account override is a FLOOR over the
// key's own budget, so it only ever raises it.
func (a Account) ResolveKeyRateLimitPerMin(keyRate int) int {
	if a.RateLimitPerMinOverride > keyRate {
		return a.RateLimitPerMinOverride
	}
	return keyRate
}

// ResolveKeyMonthlyQuota is the monthly quota auth enforces for one of
// this account's keys (0 = unmetered): a key at 0 inherits the account
// override, and a set override is a CEILING over any other key value.
func (a Account) ResolveKeyMonthlyQuota(keyQuota int64) int64 {
	override := a.MonthlyRequestQuotaOverride
	if keyQuota == 0 || (override > 0 && override < keyQuota) {
		return override
	}
	return keyQuota
}

// EffectiveRateLimitPerMin is what auth enforces on a key minted at the
// dashboard defaults. Limits are per key, so a key minted with an
// explicit budget can differ; the tier ceiling is only a mint-time cap.
func (a Account) EffectiveRateLimitPerMin() int {
	minted := min(DashboardKeyDefaultRateLimitPerMin, a.Tier.MaxRateLimitPerMin())
	return a.ResolveKeyRateLimitPerMin(minted)
}

// EffectiveMonthlyQuota is what auth enforces on a key minted at the
// dashboard defaults: it stores 0 (inherit) under an override, else the
// tier ceiling.
func (a Account) EffectiveMonthlyQuota() int64 {
	var minted int64
	if a.MonthlyRequestQuotaOverride <= 0 {
		minted = a.Tier.MaxMonthlyQuota()
	}
	return a.ResolveKeyMonthlyQuota(minted)
}

// AccountStore is the persistence boundary for [Account].
type AccountStore interface {
	// Create inserts a new account. Returns the created row with
	// server-generated ID + CreatedAt populated.
	Create(ctx context.Context, a Account) (Account, error)

	// Get returns the account by ID.
	Get(ctx context.Context, id uuid.UUID) (Account, error)

	// GetBySlug returns the account by URL-safe handle.
	GetBySlug(ctx context.Context, slug string) (Account, error)

	// Update writes mutable fields (name, billing_email, tier,
	// status, suspension bookkeeping, overrides). Immutable fields
	// (id, slug, created_at) are ignored.
	Update(ctx context.Context, a Account) error

	// Suspend sets status=suspended with a reason. Idempotent.
	Suspend(ctx context.Context, id uuid.UUID, reason string) error

	// Unsuspend clears the suspension. Idempotent.
	Unsuspend(ctx context.Context, id uuid.UUID) error
}
