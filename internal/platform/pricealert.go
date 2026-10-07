package platform

import (
	"context"
	"math"
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

// MinAlertCooldownSeconds floors the re-fire interval. An alert re-arms each
// time the price moves back across its threshold, so without a floor a price
// oscillating around it re-enqueues every subscribed webhook on alternate
// 30 s ticks: at the free tier's 25 alerts x 10 webhooks that outruns the
// delivery worker's ~5/s drain. Migration 0181 raised stored values below it.
const MinAlertCooldownSeconds = 300

// MaxAlertCooldownSeconds is the largest cooldown the int4
// `price_alerts.cooldown_seconds` column can hold.
const MaxAlertCooldownSeconds = math.MaxInt32

// ValidAlertCooldown reports whether n is an accepted cooldown. Used by
// the CRUD handler so an out-of-range value is a 400, not a firehose
// alert or a driver encode error surfacing as a 500.
func ValidAlertCooldown(n int) bool {
	return n >= MinAlertCooldownSeconds && n <= MaxAlertCooldownSeconds
}

// PriceAlert is one customer-registered price-threshold rule: "notify
// this account when <BaseAsset>/<QuoteAsset> goes <Condition>
// <Threshold>". Backs the `price_alerts` table (migration 0080).
//
// The aggregator's evaluator (internal/pricealerts) reads enabled rows
// every tick, compares each against the latest closed 1-minute VWAP for
// the pair, and — when the condition holds on an armed alert whose cooldown
// has elapsed — enqueues one `price.alert` delivery per subscribed webhook
// of the owning account. Owner-scoped by AccountID so one account's alerts
// never reach another's webhooks.
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
	// the same alert, at least [MinAlertCooldownSeconds].
	CooldownSeconds int

	Enabled bool

	// LastFiredAt is when the alert last enqueued a delivery; zero when
	// it has never fired. The evaluator gates re-fires on
	// now - LastFiredAt >= CooldownSeconds.
	LastFiredAt time.Time

	// Disarmed is set by a claimed fire and cleared once a fresh price shows
	// the condition no longer holds, so an alert fires once per crossing
	// rather than every cooldown while the condition holds.
	Disarmed bool

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
	// immutable. It re-arms the alert when the pair, condition or threshold
	// changes or a disabled alert is enabled: either is a new rule to watch.
	// [ErrNotFound] when the row is gone.
	UpdatePriceAlert(ctx context.Context, a PriceAlert) error

	// DeletePriceAlert removes the row. Idempotent (deleting an absent
	// id is not an error).
	DeletePriceAlert(ctx context.Context, id uuid.UUID) error

	// ClaimPriceAlertFire atomically claims this crossing for the caller:
	// it stamps last_fired_at, disarms the alert (and bumps updated_at)
	// ONLY when the row is armed and its own cooldown has elapsed, and
	// reports whether it won.
	//
	// The claim has to be conditional in the UPDATE itself because the
	// evaluator's cooldown check reads a SNAPSHOT taken by
	// ListEnabledPriceAlerts at the top of the sweep. Two evaluators —
	// an operator running a second aggregator, an R2/R3 standby, or a
	// deploy in which the old and new process overlap — both pass that
	// check on the same crossing, and an unconditional stamp would let
	// BOTH fan out, so the customer would get two webhooks per crossing
	// and the once-per-cooldown-window guarantee would hold only for a
	// single instance. Postgres serialises the concurrent
	// UPDATEs on the row lock, so the loser re-evaluates the predicate
	// against the winner's committed row and matches nothing.
	//
	// a is the snapshot the caller evaluated: the claim also requires the
	// row to still be enabled with the same pair, condition and threshold.
	//
	// claimed=false means "not yours to deliver": another evaluator
	// claimed this window, or the alert was edited, disabled or deleted
	// mid-sweep. All call for the same thing — skip the fan-out — so they
	// are deliberately not distinguished.
	ClaimPriceAlertFire(ctx context.Context, a PriceAlert, firedAt time.Time) (claimed bool, err error)

	// RearmPriceAlert clears Disarmed after the evaluator saw the condition
	// stop holding, but only while last_fired_at still equals lastFiredAt,
	// the fire the caller observed: a newer fire claimed by another evaluator
	// stays disarmed. rearmed=false is not an error.
	RearmPriceAlert(ctx context.Context, id uuid.UUID, lastFiredAt time.Time) (rearmed bool, err error)
}
