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
	// This does NOT set `flags.divergence_warning`. That flag
	// belongs to the cross-reference divergence service and is
	// meaningful only alongside `flags.divergence_checked`; an anomaly
	// warn runs no cross-reference check, so setting it would publish
	// warning=true / checked=false, a state consumers cannot interpret.
	// Surfacing this on the wire needs its own flag, which is an
	// API-shape decision.
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

// Publishes reports whether the bucket should be published.
//
// EXHAUSTIVE by construction, and that is the point. The publish choke
// point in the orchestrator tests `!decision.IsFrozen()`, so a fourth
// Action added later would be treated as "publish" silently — no
// compiler pressure, no lint, no test. Routing the decision through a
// switch with an explicit default makes a new variant fail CLOSED
// (refuse to publish) and, in tests, fail loudly.
func (d Decision) Publishes() bool {
	switch d.Action {
	case ActionAllow, ActionWarn:
		return true
	case ActionFreeze:
		return false
	default:
		// Unknown variant: refuse to publish. Publishing an
		// unrecognised decision is the unrecoverable direction —
		// a wrong price goes out and is cached; refusing merely
		// serves the last-known-good.
		return false
	}
}
