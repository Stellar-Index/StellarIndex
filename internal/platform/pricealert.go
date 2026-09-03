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
// (ListEnabledPriceAlerts/ClaimPriceAlertFire) is called by the
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

	// ClaimPriceAlertFire atomically claims this crossing's cooldown
	// window for the caller: it stamps last_fired_at (and bumps
	// updated_at) ONLY when the row's own cooldown has elapsed, and
	// reports whether it won.
	//
	// The claim has to be conditional in the UPDATE itself because the
	// evaluator's cooldown check reads a SNAPSHOT taken by
	// ListEnabledPriceAlerts at the top of the sweep. Two evaluators —
	// an operator running a second aggregator, an R2/R3 standby, or a
	// deploy in which the old and new process overlap — both pass that
	// check on the same crossing, and an unconditional stamp then let
	// BOTH fan out, so the customer got two webhooks per crossing and
	// the once-per-cooldown-window guarantee was only ever true for a
	// single instance (#368 M10). Postgres serialises the concurrent
	// UPDATEs on the row lock, so the loser re-evaluates the predicate
	// against the winner's committed row and matches nothing.
	//
	// claimed=false means "not yours to deliver": either another
	// evaluator claimed this window, or the alert was deleted mid-sweep.
	// Both call for the same thing — skip the fan-out — so they are
	// deliberately not distinguished.
	ClaimPriceAlertFire(ctx context.Context, id uuid.UUID, firedAt time.Time) (claimed bool, err error)
}
