package platform

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
)

// AlertCondition is the direction a [PriceAlert] fires in, mirroring the `price_alerts.condition` CHECK constraint.
type AlertCondition string

const (
	// AlertAbove fires when the observed price is at or above the
	// threshold (observed >= threshold).
	AlertAbove AlertCondition = "above"
	// AlertBelow fires when the observed price is at or below the
	// threshold (observed <= threshold).
	AlertBelow AlertCondition = "below"
)

// ValidAlertCondition reports whether s is a known condition, so the CRUD handler rejects a bad one before the INSERT.
func ValidAlertCondition(s string) bool {
	switch AlertCondition(s) {
	case AlertAbove, AlertBelow:
		return true
	default:
		return false
	}
}

// MinAlertCooldownSeconds floors the re-fire interval: without it a price oscillating around the threshold re-enqueues every subscribed webhook each tick and outruns the delivery worker's drain.
const MinAlertCooldownSeconds = 300

// MaxAlertCooldownSeconds is the largest cooldown the int4 `price_alerts.cooldown_seconds` column can hold.
const MaxAlertCooldownSeconds = math.MaxInt32

// ValidAlertCooldown reports whether n is an accepted cooldown, so an out-of-range value is a 400 rather than a firehose alert or a 500.
func ValidAlertCooldown(n int) bool {
	return n >= MinAlertCooldownSeconds && n <= MaxAlertCooldownSeconds
}

// PriceAlert is a customer price-threshold rule evaluated against the latest closed 1-minute VWAP; owner-scoped by AccountID so one account's alerts never reach another's webhooks.
type PriceAlert struct {
	ID        uuid.UUID
	AccountID uuid.UUID

	// BaseAsset / QuoteAsset are canonical wire-form asset ids (`native`, `CODE-ISSUER`, `C…`,
	// `fiat:USD`), parsed with canonical.ParseAsset. The price is of BaseAsset in QuoteAsset.
	BaseAsset  string
	QuoteAsset string

	Condition AlertCondition

	// Threshold is a decimal STRING, never a float (ADR-0003). Stored NUMERIC and compared
	// against the observed VWAP with big.Rat so precision is never lost.
	Threshold string

	// CooldownSeconds is the minimum wall-clock gap between two fires of
	// the same alert, at least [MinAlertCooldownSeconds].
	CooldownSeconds int

	Enabled bool

	// LastFiredAt is zero when never fired; re-fires are gated on now-LastFiredAt >= CooldownSeconds.
	LastFiredAt time.Time

	// Disarmed is set by a claimed fire and cleared once a fresh price shows
	// the condition no longer holds, so an alert fires once per crossing
	// rather than every cooldown while the condition holds.
	Disarmed bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// PriceAlertStore is the persistence boundary for [PriceAlert]: CRUD for the dashboard, ListEnabledPriceAlerts/ClaimPriceAlertFire for the aggregator worker.
type PriceAlertStore interface {
	// CreatePriceAlert inserts an alert, enforcing the per-account cap atomically; [ErrPriceAlertQuotaExceeded] at the cap.
	CreatePriceAlert(ctx context.Context, a PriceAlert, maxPerAccount int) (PriceAlert, error)

	// GetPriceAlert returns one alert by ID. [ErrNotFound] when absent.
	GetPriceAlert(ctx context.Context, id uuid.UUID) (PriceAlert, error)

	// ListPriceAlertsForAccount returns every alert for the account,
	// newest first. Powers the dashboard list view.
	ListPriceAlertsForAccount(ctx context.Context, accountID uuid.UUID) ([]PriceAlert, error)

	// ListEnabledPriceAlerts returns every enabled alert across all
	// accounts. The evaluator sweeps this set each tick.
	ListEnabledPriceAlerts(ctx context.Context) ([]PriceAlert, error)

	// UpdatePriceAlert persists the mutable fields (AccountID and ID immutable) and re-arms when the pair, condition or threshold changes or a disabled alert is enabled; [ErrNotFound] if gone.
	UpdatePriceAlert(ctx context.Context, a PriceAlert) error

	// DeletePriceAlert removes the row. Idempotent (deleting an absent
	// id is not an error).
	DeletePriceAlert(ctx context.Context, id uuid.UUID) error

	// ClaimPriceAlertFire atomically claims a crossing (stamps last_fired_at, disarms) only if the
	// row is armed, past its cooldown, and still matches snapshot a's pair, condition and
	// threshold. The condition lives in the UPDATE because the evaluator's cooldown check reads a
	// snapshot, so overlapping evaluators would otherwise send two webhooks per crossing.
	// claimed=false means not yours to deliver: skip the fan-out.
	ClaimPriceAlertFire(ctx context.Context, a PriceAlert, firedAt time.Time) (claimed bool, err error)

	// RearmPriceAlert clears Disarmed once the condition stops holding, but only while
	// last_fired_at still equals lastFiredAt, so a newer fire claimed by another evaluator stays
	// disarmed. rearmed=false is not an error.
	RearmPriceAlert(ctx context.Context, id uuid.UUID, lastFiredAt time.Time) (rearmed bool, err error)
}
