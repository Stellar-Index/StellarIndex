package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Tier is the billing-plan / capability bucket an account sits in.
// Matches the `accounts.tier` CHECK constraint in migration 0027.
type Tier string

const (
	TierFree       Tier = "free"
	TierStarter    Tier = "starter"
	TierPro        Tier = "pro"
	TierBusiness   Tier = "business"
	TierEnterprise Tier = "enterprise"
)

// MaxRateLimitPerMin returns the per-tier ceiling for the customer-
// supplied `rate_limit_per_min` field on dashboard-minted keys.
//
// Without this ceiling, the dashboard key-creation flow accepted any
// positive value up to 100_000 regardless of the account's tier —
// meaning a Free account could self-mint a key budgeted for 100×
// the Starter default. F-1212 (codex audit-2026-05-12).
//
// Tier ladder mirrors `docs/architecture/billing-tiers.md`:
//
//   - Free:       60/min  (parity with anon-tier; keys for development only)
//   - Starter:    1000/min
//   - Pro:        10_000/min
//   - Business:   60_000/min
//   - Enterprise: 100_000/min  (operator-approved overrides go higher)
//
// An unknown tier value is treated as Free (defensive — a corrupt
// row should not unlock paid budgets).
func (t Tier) MaxRateLimitPerMin() int {
	switch t {
	case TierStarter:
		return 1000
	case TierPro:
		return 10_000
	case TierBusiness:
		return 60_000
	case TierEnterprise:
		return 100_000
	default: // TierFree + any unknown value
		return 60
	}
}

// MaxActiveKeys returns the per-tier ceiling on concurrently active
// (non-revoked) API keys an account can hold. Replaces the flat
// 25-key cap the dashboard shipped with ("tier-aware quotas can
// replace this once billing is wired — Phase 2"). Deployments can
// override per tier via the dashboard handler config
// (dashboardkeys.Config.KeyQuotas); this method is the
// config-absent default ladder.
//
// An unknown tier value is treated as Free (defensive — a corrupt
// row should not unlock paid quotas), matching
// [Tier.MaxRateLimitPerMin].
func (t Tier) MaxActiveKeys() int {
	switch t {
	case TierStarter:
		return 25
	case TierPro:
		return 50
	case TierBusiness:
		return 100
	case TierEnterprise:
		return 250
	default: // TierFree + any unknown value
		return 5
	}
}

// MaxMonthlyQuota returns the per-tier ceiling on a dashboard-minted
// key's customer-supplied `monthly_quota` (the monthly request-volume
// cap the runtime quota middleware enforces).
//
// This is the CEILING the account-level operator override
// ([Account.MonthlyRequestQuotaOverride]) falls back to when unset: the
// dashboard clamps a customer-supplied per-key quota to
// min(requested, override-if-set-else-this-ladder) so a metered
// customer can only LOWER their cap, never raise it above the plan.
// Without this ceiling the create handler honoured any int64 the POST
// body carried — a customer on a metered plan could self-mint a key
// with `monthly_quota: 9_000_000_000` and run effectively unmetered
// (audit-2026-07 MEDIUM).
//
// Symmetric-opposite of [Tier.MaxRateLimitPerMin]: rate limit is a
// burst ceiling clamped at mint AND raised by the account override as
// a FLOOR at auth time; monthly quota is a volume ceiling clamped at
// mint AND lowered by the account override as a CEILING at auth time.
//
// Values are round plan budgets (monotonic across the ladder), not a
// mechanical rate×minutes derivation. Operators grant a specific
// customer more by setting a higher [Account.MonthlyRequestQuotaOverride]
// — that override, not this default, is then the hard ceiling.
//
// An unknown tier value is treated as Free (defensive — a corrupt row
// should not unlock paid quotas), matching the other tier ladders.
func (t Tier) MaxMonthlyQuota() int64 {
	switch t {
	case TierStarter:
		return 1_000_000
	case TierPro:
		return 10_000_000
	case TierBusiness:
		return 100_000_000
	case TierEnterprise:
		return 1_000_000_000
	default: // TierFree + any unknown value
		return 100_000
	}
}

// MaxWebhooks returns the per-tier ceiling on registered webhook
// endpoints. Replaces the dashboard's flat 10-webhook cap; override
// seam is dashboardwebhooks.Config.WebhookQuotas. Unknown tiers are
// treated as Free, same posture as the other tier ladders.
func (t Tier) MaxWebhooks() int {
	switch t {
	case TierStarter:
		return 10
	case TierPro:
		return 25
	case TierBusiness:
		return 50
	case TierEnterprise:
		return 100
	default: // TierFree + any unknown value
		return 2
	}
}

// MaxPriceAlerts returns the per-tier ceiling on registered price
// alerts an account can hold (BACKLOG #60). Same tier-aware ladder
// shape as [Tier.MaxWebhooks]; the override seam is
// dashboardpricealerts.Config.AlertQuotas. Unknown tiers are treated
// as Free, matching the other tier ladders.
func (t Tier) MaxPriceAlerts() int {
	switch t {
	case TierStarter:
		return 25
	case TierPro:
		return 100
	case TierBusiness:
		return 250
	case TierEnterprise:
		return 1000
	default: // TierFree + any unknown value
		return 5
	}
}

// AccountStatus is the lifecycle state of an account.
type AccountStatus string

const (
	AccountActive    AccountStatus = "active"
	AccountSuspended AccountStatus = "suspended"
	AccountClosed    AccountStatus = "closed"
)

// Account is the top-level org primitive — every API key,
// subscription, webhook, audit-log entry hangs off Account.ID.
//
// v1 is one user per account; v2 multi-org migrates by moving
// (account_id, role) from User to a new memberships join table
// without changing this type.
type Account struct {
	ID                          uuid.UUID
	Name                        string
	Slug                        string
	BillingEmail                string
	StripeCustomerID            string // empty when no Stripe customer minted yet
	Tier                        Tier
	Status                      AccountStatus
	CreatedAt                   time.Time
	SuspendedAt                 time.Time // zero when not suspended
	SuspendedReason             string
	RateLimitPerMinOverride     int   // 0 = inherit tier default
	MonthlyRequestQuotaOverride int64 // 0 = inherit tier default
}

// AccountStore is the persistence boundary for [Account].
//
// Implementations: PostgresAccountStore (Week 1, this PR ships
// the interface; concrete impl lands when the auth flow needs
// it in Week 2).
type AccountStore interface {
	// Create inserts a new account. Returns the created row with
	// server-generated ID + CreatedAt populated.
	Create(ctx context.Context, a Account) (Account, error)

	// Get returns the account by ID.
	Get(ctx context.Context, id uuid.UUID) (Account, error)

	// GetBySlug returns the account by URL-safe handle.
	GetBySlug(ctx context.Context, slug string) (Account, error)

	// GetByStripeCustomerID maps Stripe customer back to our
	// account — used by webhook handlers.
	GetByStripeCustomerID(ctx context.Context, stripeCustomerID string) (Account, error)

	// Update writes mutable fields (name, billing_email, tier,
	// status, overrides). Immutable fields (id, slug, created_at)
	// are ignored.
	Update(ctx context.Context, a Account) error

	// Suspend sets status=suspended with a reason. Idempotent.
	Suspend(ctx context.Context, id uuid.UUID, reason string) error

	// Unsuspend clears the suspension. Idempotent.
	Unsuspend(ctx context.Context, id uuid.UUID) error
}
