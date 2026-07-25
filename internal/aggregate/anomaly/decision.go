package anomaly

// Action is the recommended publishing behaviour for a bucket.
// Stable string values appear in metric labels; renaming is a wire
// break.
type Action string

const (
	// ActionAllow — publish the bucket normally. No flags fire.
	ActionAllow Action = "allow"

	// ActionWarn — publish the bucket, and record the warning on
	// the OPERATOR side (obs.AnomalyWarnTotal + a Warn log). The
	// deviation is large enough to call out but not extreme enough
	// (or has multi-source corroboration) to refuse to publish.
	//
	// This does NOT set `flags.divergence_warning`, despite what
	// this comment claimed until audit COR-09/AGT-06. That flag
	// belongs to the cross-reference divergence service and is
	// meaningful only alongside `flags.divergence_checked`
	// (CS-087); an anomaly warn runs no cross-reference check, so
	// setting it would publish warning=true / checked=false — the
	// state CS-087 declares un-interpretable. Surfacing this on
	// the wire needs its own flag, which is an API-shape decision.
	ActionWarn Action = "warn"

	// ActionFreeze — DO NOT publish this bucket. Serve the
	// previous bucket's last-known-good VWAP with
	// `flags.frozen: true` and `flags.single_source: true`.
	// Caller maintains the LKG slot; this package only signals
	// the recommended action.
	ActionFreeze Action = "freeze"
)

// Decision is the result of [Checker.Evaluate]. Carries the chosen
// [Action] plus enough context for the caller to populate response
// flags, log lines, and Prometheus labels.
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
