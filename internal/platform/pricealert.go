package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AlertCondition is the direction a [PriceAlert] fires in.
//
// Matches the `price_alerts.condition` CHECK constraint in migration
// 0080.
type AlertCondition string

const (
	// AlertAbove fires when the observed price is at or above the
	// threshold (observed >= threshold).
	AlertAbove AlertCondition = "above"
	// AlertBelow fires when the observed price is at or below the
	// threshold (observed <= threshold).
	AlertBelow AlertCondition = "below"
)

// ValidAlertCondition reports whether s is a recognised condition
// string. Used by the CRUD handler to reject a bad `condition` before
// the INSERT would hit the CHECK constraint.
func ValidAlertCondition(s string) bool {
	switch AlertCondition(s) {
	case AlertAbove, AlertBelow:
		return true
	default:
		return false
	}
}

// PriceAlert is one customer-registered price-threshold rule: "notify
// this account when <BaseAsset>/<QuoteAsset> goes <Condition>
// <Threshold>". Backs the `price_alerts` table (migration 0080).
//
// The aggregator's evaluator (internal/pricealerts) reads enabled rows
// every tick, compares each against the latest closed 1-minute VWAP for
// the pair, and — respecting Cooldown + LastFiredAt — enqueues a
// `price.alert` delivery into the customer-webhook queue for the owning
// account's subscribed webhooks. Owner-scoped by AccountID so one
// account's alerts never reach another's webhooks.
type PriceAlert struct {
	ID        uuid.UUID
	AccountID uuid.UUID

	// BaseAsset / QuoteAsset are canonical wire-form asset ids
	// (`native`, `CODE-ISSUER`, `C…`, `fiat:USD`). The evaluator parses
	// them with canonical.ParseAsset; the pair is read in the stored
	// orientation (price of BaseAsset expressed in QuoteAsset).
	BaseAsset  string
	QuoteAsset string

	Condition AlertCondition

	// Threshold is the price boundary as an arbitrary-precision decimal
	// STRING (ADR-0003 — a price is an i128-derived amount ratio, never
	// a float). Stored NUMERIC; compared against the observed VWAP with
	// big.Rat so precision is never lost.
	Threshold string

	// CooldownSeconds is the minimum wall-clock gap between two fires of
	// the same alert. 0 = re-fire every tick the condition holds.
	CooldownSeconds int

	Enabled bool

	// LastFiredAt is when the alert last enqueued a delivery; zero when
	// it has never fired. The evaluator gates re-fires on
	// now - LastFiredAt >= CooldownSeconds.
	LastFiredAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// PriceAlertStore is the persistence boundary for [PriceAlert].
//
// Implementation: postgresstore.PriceAlertStore (migration 0080). The
// CRUD half (Create/Get/List-for-account/Update/Delete) is called by
// the dashboard handlers in the API binary; the evaluator half
// (ListEnabledPriceAlerts/MarkPriceAlertFired) is called by the
// aggregator's price-alert worker.
type PriceAlertStore interface {
	// CreatePriceAlert inserts a new alert, enforcing the per-account
	// `maxPerAccount` cap atomically (same advisory-lock + CTE-gated
	// INSERT shape as CreateWebhook). Returns
	// [ErrPriceAlertQuotaExceeded] when the account is already at the
	// cap.
	CreatePriceAlert(ctx context.Context, a PriceAlert, maxPerAccount int) (PriceAlert, error)

	// GetPriceAlert returns one alert by ID. [ErrNotFound] when absent.
	GetPriceAlert(ctx context.Context, id uuid.UUID) (PriceAlert, error)

	// ListPriceAlertsForAccount returns every alert for the account,
	// newest first. Powers the dashboard list view.
	ListPriceAlertsForAccount(ctx context.Context, accountID uuid.UUID) ([]PriceAlert, error)

	// ListEnabledPriceAlerts returns every enabled alert across all
	// accounts. The evaluator sweeps this set each tick.
	ListEnabledPriceAlerts(ctx context.Context) ([]PriceAlert, error)

	// UpdatePriceAlert persists the mutable fields (base/quote asset,
	// condition, threshold, cooldown, enabled). AccountID + ID are
	// immutable. [ErrNotFound] when the row is gone.
	UpdatePriceAlert(ctx context.Context, a PriceAlert) error

	// DeletePriceAlert removes the row. Idempotent (deleting an absent
	// id is not an error).
	DeletePriceAlert(ctx context.Context, id uuid.UUID) error

	// MarkPriceAlertFired stamps last_fired_at (and bumps updated_at).
	// Called by the evaluator after it enqueues a delivery so the
	// cooldown clock starts.
	MarkPriceAlertFired(ctx context.Context, id uuid.UUID, firedAt time.Time) error
}
