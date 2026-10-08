package anomaly

// Action is the recommended publishing behaviour for a bucket.
// Stable string values appear in metric labels; renaming is a wire
// break.
type Action string

const (
	// ActionAllow — publish the bucket normally. No flags fire.
	ActionAllow Action = "allow"

	// ActionWarn publishes the bucket and records the warning operator-side only.
	// It does not set flags.divergence_warning: that flag is meaningful only beside
	// divergence_checked, and an anomaly warn runs no cross-reference check.
	ActionWarn Action = "warn"

	// ActionFreeze withholds the bucket; the caller serves its last-known-good
	// VWAP with flags.frozen and flags.single_source.
	ActionFreeze Action = "freeze"
)

// Decision is the result of [Checker.Evaluate].
type Decision struct {
	// Action — what the caller should do with the bucket.
	Action Action

	// Class — which [AssetClass] was used to look up thresholds.
	Class AssetClass

	// Thresholds — the per-class thresholds that were checked.
	// Useful for log lines + ops dashboards.
	Thresholds Thresholds

	// DeviationPct — the computed deviation of CurrVWAP from
	// PrevVWAP, as an absolute percentage.
	DeviationPct float64

	// Reason — short human-readable explanation. Used by
	// runbooks + log lines; not a wire-shape field.
	Reason string
}

// IsFrozen reports whether the decision says to freeze. Convenience
// for callers that only need the boolean.
func (d Decision) IsFrozen() bool { return d.Action == ActionFreeze }

// IsWarn reports whether the decision says to warn (publish but
// flag).
func (d Decision) IsWarn() bool { return d.Action == ActionWarn }

// Publishes reports whether the bucket should be published. The switch fails
// closed so a new Action variant refuses to publish rather than silently publishing.
func (d Decision) Publishes() bool {
	switch d.Action {
	case ActionAllow, ActionWarn:
		return true
	case ActionFreeze:
		return false
	default:
		// A wrong published price is cached; refusing only serves the last-known-good.
		return false
	}
}
