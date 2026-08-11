package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Tier is the capability bucket an account sits in. The platform is
// free (operator decision 2026-08-10 — no payments), so the model is
// exactly three levels:
//
//   - anon:    unauthenticated callers. Not an account tier at all —
//     the per-IP baseline enforced by the rate-limit middleware
//     (`[api].anon_rate_limit_per_min`, default 60/min). Declared
//     here so the ladder methods can document the full model in one
//     place; it is never persisted to `accounts.tier`.
//   - free:    every registered account's default. Higher limits than
//     anon (anchored to the old Starter plan's numbers — the lowest
//     paid tier before billing was removed).
//   - partner: staff-admitted accounts with per-account limits set by
//     an operator via PATCH /v1/admin/accounts/{id} (the
//     rate_limit_per_min / monthly_request_quota overrides). The
//     ladder values below are the partner CEILINGS when no override
//     is set (the old Enterprise numbers); the override, when set, is
//     the real limit.
//
// The `accounts.tier` CHECK constraint (migration 0027, deliberately
// untouched) still speaks the legacy five-string vocabulary
// (free/starter/pro/business/enterprise). The mapping is code-level:
// [Tier.Canonical] folds legacy strings into the three-level model on
// every read, and [Tier.StorageValue] renders a canonical tier back
// into a CHECK-legal string on every write.
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

// Legacy pre-free-platform tier strings, still present in
// `accounts.tier` rows (and accepted by the admin PATCH for
// back-compat). Canonical() folds them into the three-level model.
//
// Deprecated: use TierFree / TierPartner; these exist only for the
// legacy-string mapping and will be removed once the stored rows are
// rewritten.
const (
	TierStarter    Tier = "starter"    // → free (identical numbers)
	TierPro        Tier = "pro"        // → partner
	TierBusiness   Tier = "business"   // → partner
	TierEnterprise Tier = "enterprise" // → partner
)

// KnownTier reports whether s is an accepted account-tier string:
// canonical (free / partner) or one of the legacy paid-tier names
// that [Tier.Canonical] folds (starter / pro / business /
// enterprise). anon is deliberately NOT accepted — it is the
// unauthenticated baseline, never an account tier. This is the
// single definition of the PATCH /v1/admin/accounts/{id} tier
// vocabulary.
func KnownTier(s string) bool {
	switch Tier(s) {
	case TierFree, TierPartner,
		TierStarter, TierPro, TierBusiness, TierEnterprise:
		return true
	default:
		return false
	}
}

// Canonical maps any tier string — canonical, legacy, or corrupt —
// into the three-level model. Legacy paid plans above Starter had
// individually negotiated / elevated limits, which is exactly what
// partner now means, so pro/business/enterprise fold to partner;
// starter folds to free (the free ladder IS the old Starter ladder).
// Unknown values fold to free, matching the historical "a corrupt row
// should not unlock elevated budgets" posture.
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

// StorageValue renders the tier as a string the migration-0027 CHECK
// constraint accepts (the constraint is deliberately untouched — the
// mapping lives in code). partner persists as "enterprise" (the
// legacy string that canonicalises back to partner); everything else
// persists as "free". Round-trip property: for any t,
// Tier(t.StorageValue()).Canonical() == t.Canonical() (with anon
// defensively folding to free, since anon is never an account tier).
func (t Tier) StorageValue() string {
	if t.Canonical() == TierPartner {
		return string(TierEnterprise)
	}
	return string(TierFree)
}

// MaxRateLimitPerMin returns the per-tier ceiling for the customer-
// supplied `rate_limit_per_min` field on dashboard-minted keys.
//
// Without this ceiling, the dashboard key-creation flow accepted any
// positive value up to 100_000 regardless of the account's tier.
// F-1212 (codex audit-2026-05-12).
//
// Ladder (free-platform model, 2026-08-11):
//
//   - anon:    60/min (the `[api].anon_rate_limit_per_min` default —
//     documented here, enforced by the middleware)
//   - free:    1000/min (the old Starter budget; also what
//     POST /v1/signup and POST /v1/register mint at)
//   - partner: 100_000/min ceiling; the staff-set per-account
//     override is the real limit
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

// MaxActiveKeys returns the per-tier ceiling on concurrently active
// (non-revoked) API keys an account can hold. Replaces the flat
// 25-key cap the dashboard shipped with ("tier-aware quotas can
// replace this once billing is wired — Phase 2"). Deployments can
// override per tier via the dashboard handler config
// (dashboardkeys.Config.KeyQuotas); this method is the
// config-absent default ladder.
//
// An unknown tier value is treated as free (defensive — a corrupt
// row should not unlock elevated quotas), matching
// [Tier.MaxRateLimitPerMin].
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
// An unknown tier value is treated as free (defensive — a corrupt row
// should not unlock elevated quotas), matching the other tier ladders.
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

// MaxPriceAlerts returns the per-tier ceiling on registered price
// alerts an account can hold (BACKLOG #60). Same tier-aware ladder
// shape as [Tier.MaxWebhooks]; the override seam is
// dashboardpricealerts.Config.AlertQuotas. Unknown tiers are treated
// as free, matching the other tier ladders.
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

// Account is the top-level org primitive — every API key,
// webhook, audit-log entry hangs off Account.ID.
//
// v1 is one user per account; v2 multi-org migrates by moving
// (account_id, role) from User to a new memberships join table
// without changing this type.
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

	// Update writes mutable fields (name, billing_email, tier,
	// status, suspension bookkeeping, overrides). Immutable fields
	// (id, slug, created_at) are ignored.
	Update(ctx context.Context, a Account) error

	// Suspend sets status=suspended with a reason. Idempotent.
	Suspend(ctx context.Context, id uuid.UUID, reason string) error

	// Unsuspend clears the suspension. Idempotent.
	Unsuspend(ctx context.Context, id uuid.UUID) error
}
