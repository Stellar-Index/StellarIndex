package freeze

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ADR-0019 freeze lifecycle: a HOLD with an extension ladder (30 min, up to 4 × 30 min, then
// P1 and manual unfreeze only), released when confidence > 0.30 AND z < 3.0 for two consecutive
// buckets. Releasing on one bucket's negated fire condition lets one trade on a second venue, or
// nudging z from 5.1 to 4.9, publish the manipulated price. The state machine is a pure function
// of (previous state, signal); the orchestrator owns persistence. Load-bearing:
//   - the initial hold is a MINIMUM: on a wide-MAD asset a price far from last-known-good can
//     read z < 3 two buckets running;
//   - release needs POSITIVE evidence; an unscorable bucket resets the streak.

// Freeze-lifecycle defaults, per ADR-0019 §"Freeze duration" and
// §"Auto-unfreeze trigger". Operators tune via `[anomaly.phase2]`;
// unset fields fall back to these.
const (
	// DefaultInitialHold — ADR-0019's flat "Initial: 30 minutes",
	// applied to any freeze that fired on a pair with a corroborating
	// lens available this bucket (see [Signal.Corroborated]).
	DefaultInitialHold = 30 * time.Minute

	// DefaultUncorroboratedInitialHold deviates from ADR-0019 for pairs no corroborating lens read.
	// A false freeze serves a stale price (its own money bug) and false freezes concentrate on thin
	// books; 10 min still outlasts the 5m window and several ticks. Extensions are NOT scaled.
	DefaultUncorroboratedInitialHold = 10 * time.Minute

	// DefaultExtension — ADR-0019: "extend by 30 min".
	DefaultExtension = 30 * time.Minute

	// DefaultMaxExtensions — ADR-0019: "up to 4 extensions (2 hours
	// total)", after which the freeze escalates to operator review.
	DefaultMaxExtensions = 4

	// DefaultUnfreezeConfidenceMin — ADR-0019 §"Auto-unfreeze
	// trigger": "confidence rises above 0.30".
	DefaultUnfreezeConfidenceMin = 0.30

	// DefaultUnfreezeZScoreMax — ADR-0019 §"Auto-unfreeze trigger":
	// "z_score falls below 3.0".
	DefaultUnfreezeZScoreMax = 3.0

	// DefaultUnfreezeBuckets — ADR-0019 §"Auto-unfreeze trigger":
	// "for two consecutive buckets".
	DefaultUnfreezeBuckets = 2

	// DefaultOverrideMemory is how long a force-unfrozen ladder is remembered, so a pair re-firing
	// inside it resumes escalated instead of starting a fresh hold that will not page for two hours.
	DefaultOverrideMemory = 2 * time.Hour
)

// DefaultMarkerGrace is how long the Redis marker outlives its hold, so a missed tick or restart
// cannot blink `flags.frozen` off mid-hold. A freeze's DURATION is state, not a TTL.
var DefaultMarkerGrace = cachekeys.FreezeTTL

// Policy is the operator-tunable shape of the ADR-0019 freeze
// lifecycle. The zero value is valid: every field falls back to its
// documented `Default*` above via [Policy.WithDefaults].
type Policy struct {
	// InitialHold is the minimum freeze duration for a corroborated
	// freeze (ADR-0019: 30 minutes).
	InitialHold time.Duration

	// UncorroboratedInitialHold is the minimum freeze duration when no
	// corroborating lens produced a reading for the pair — see
	// [DefaultUncorroboratedInitialHold] for why it is shorter.
	UncorroboratedInitialHold time.Duration

	// Extension is added at each hold expiry the freeze has not earned
	// its release at (ADR-0019: 30 minutes).
	Extension time.Duration

	// MaxExtensions is how many extensions are granted before the
	// freeze escalates to operator review (ADR-0019: 4 → 2 hours).
	MaxExtensions int

	// UnfreezeConfidenceMin / UnfreezeZScoreMax / UnfreezeBuckets are
	// the ADR-0019 auto-unfreeze condition: confidence strictly above
	// UnfreezeConfidenceMin AND z strictly below UnfreezeZScoreMax,
	// for UnfreezeBuckets CONSECUTIVE buckets.
	UnfreezeConfidenceMin float64
	UnfreezeZScoreMax     float64
	UnfreezeBuckets       int

	// MarkerGrace is how far past the hold the Redis marker's TTL is
	// set — see [DefaultMarkerGrace].
	MarkerGrace time.Duration

	// OverrideMemory is how long a re-fire after an override resumes the
	// overridden ladder — see [DefaultOverrideMemory].
	OverrideMemory time.Duration
}

// WithDefaults replaces every zero field with its ADR-0019 default. A zero
// UnfreezeConfidenceMin would pass any positive confidence, so a deliberate 0 becomes 0.30;
// negative MaxExtensions is rejected by config validation.
func (p Policy) WithDefaults() Policy {
	if p.InitialHold <= 0 {
		p.InitialHold = DefaultInitialHold
	}
	if p.UncorroboratedInitialHold <= 0 {
		p.UncorroboratedInitialHold = DefaultUncorroboratedInitialHold
	}
	if p.Extension <= 0 {
		p.Extension = DefaultExtension
	}
	if p.MaxExtensions == 0 {
		p.MaxExtensions = DefaultMaxExtensions
	}
	if p.UnfreezeConfidenceMin == 0 {
		p.UnfreezeConfidenceMin = DefaultUnfreezeConfidenceMin
	}
	if p.UnfreezeZScoreMax == 0 {
		p.UnfreezeZScoreMax = DefaultUnfreezeZScoreMax
	}
	if p.UnfreezeBuckets == 0 {
		p.UnfreezeBuckets = DefaultUnfreezeBuckets
	}
	if p.OverrideMemory <= 0 {
		p.OverrideMemory = DefaultOverrideMemory
	}
	if p.MarkerGrace <= 0 {
		p.MarkerGrace = DefaultMarkerGrace
	}
	return p
}

// State is one pair's freeze-lifecycle authority; the Redis marker stays the serving authority
// and carries a copy so the state survives a restart. The zero value is "not frozen".
type State struct {
	// FiredAt is when the freeze first engaged. Preserved across
	// extensions, so `now - FiredAt` is the freeze's true age.
	FiredAt time.Time `json:"fired_at,omitempty"`

	// HoldUntil is when the current hold expires and the lifecycle
	// re-evaluates (release / extend / escalate). Slides forward on
	// every extension.
	HoldUntil time.Time `json:"hold_until,omitempty"`

	// ExtensionsUsed counts granted extensions. At
	// [Policy.MaxExtensions] the next expiry escalates instead.
	ExtensionsUsed int `json:"extensions_used,omitempty"`

	// Escalated records that the extension ladder ran out and the
	// freeze is now awaiting operator action. An escalated freeze does
	// NOT auto-unfreeze — ADR-0019: "freeze stays active until manual
	// unfreeze".
	Escalated bool `json:"escalated,omitempty"`

	// UnfreezeStreak counts CONSECUTIVE buckets that met the
	// auto-unfreeze condition. Reset to 0 by any bucket that does not,
	// including a bucket that could not be scored at all.
	UnfreezeStreak int `json:"unfreeze_streak,omitempty"`

	// Corroborated records whether a lens had read the pair at fire time, so an operator can tell
	// why the first hold was 10 minutes rather than 30.
	Corroborated bool `json:"corroborated,omitempty"`

	// OverriddenAt is when an out-of-band override last ended this pair's
	// ladder. On an inactive State it is the remembered ladder
	// ([DefaultOverrideMemory]); on an active one it marks a freeze that
	// resumed that ladder rather than starting from the bottom.
	OverriddenAt time.Time `json:"overridden_at,omitempty"`
}

// Overridden returns the inactive State an override leaves behind: the
// ladder it ended, remembered from `now`.
func (s State) Overridden(now time.Time) State {
	return State{ExtensionsUsed: s.ExtensionsUsed, Escalated: s.Escalated, OverriddenAt: now}
}

// Active reports whether the state describes a live freeze.
func (s State) Active() bool { return !s.FiredAt.IsZero() }

// Signal is what one closed bucket contributes to the lifecycle.
type Signal struct {
	// Now is the evaluation instant. Passed in rather than read from
	// the clock so the policy stays a pure function.
	Now time.Time

	// Fires is the ADR-0019 3-signal AND for THIS bucket (the Phase 2
	// `confidence < x AND z > y AND sources <= z` condition), or the
	// Phase 1 class-threshold freeze decision. It engages a freeze; it
	// does NOT keep one alive and does NOT extend one.
	Fires bool

	// Scored is false when the bucket could not be scored (no baseline, no previous VWAP): absence
	// of evidence, never health, so it resets the auto-unfreeze streak.
	Scored bool

	// Confidence / ZScore are the values the auto-unfreeze reads. ZScore is observation-based, not
	// sustained drift: drift latches for 30 days and cannot self-clear, so it arrives via Confidence.
	Confidence float64
	ZScore     float64

	// Corroborated is "was a second lens consulted", not "did it agree"; it selects the initial
	// hold. A disagreeing lens is the strongest evidence the freeze is true and must not shorten it.
	Corroborated bool

	// ReleaseCorroborated: a lens agrees with this bucket's fresh price (the one a release would
	// publish). Calm legs alone cannot release: a held manipulation reads calm too. False with no lens,
	// so those freezes end only by override or escalation.
	ReleaseCorroborated bool
}

// Transition names what the lifecycle did on one evaluation. Stable
// strings: they appear in logs and metric labels.
type Transition string

const (
	// TransitionNone — not frozen before, not firing now.
	TransitionNone Transition = "none"
	// TransitionFired — a fresh freeze engaged.
	TransitionFired Transition = "fired"
	// TransitionHeld — an existing freeze is inside its hold.
	TransitionHeld Transition = "held"
	// TransitionExtended — the hold expired unreleased; ladder +1.
	TransitionExtended Transition = "extended"
	// TransitionHeldUnscored — the hold expired on an unscored bucket; it slides without consuming
	// an extension, and the ladder resumes when scoring does.
	TransitionHeldUnscored Transition = "held_unscored"
	// TransitionEscalated — the ladder ran out; operator review (P1).
	TransitionEscalated Transition = "escalated"
	// TransitionReleased — the auto-unfreeze condition was met at
	// expiry; the freeze ends.
	TransitionReleased Transition = "released"
	// TransitionOverridden — the operator cleared the marker out of
	// band (ADR-0019: "Operator override always available: force
	// unfreeze"); the freeze ends immediately, ladder and all.
	TransitionOverridden Transition = "overridden"
)

// Outcome is one evaluation's result.
type Outcome struct {
	// Frozen is what the caller acts on: true means do not publish
	// this bucket and keep the marker alive.
	Frozen bool

	// Transition is what changed, for logs + metrics.
	Transition Transition

	// State is the state to persist. Zero (inactive) on release.
	State State

	// MarkerTTL is the TTL to write the Redis marker with — the
	// remaining hold plus [Policy.MarkerGrace]. Zero when not frozen.
	MarkerTTL time.Duration
}

// Evaluate advances the freeze lifecycle by one bucket.
//
// Pure: same (prev, sig) always yields the same Outcome. All
// persistence, marker IO, metrics and logging belong to the caller.
func (p Policy) Evaluate(prev State, sig Signal) Outcome {
	p = p.WithDefaults()

	if !prev.Active() {
		if !sig.Fires {
			return Outcome{Transition: TransitionNone}
		}
		return p.fire(prev, sig)
	}

	st := prev
	st.UnfreezeStreak = p.streak(prev.UnfreezeStreak, sig)

	switch {
	case st.Escalated:
		// Escalated: no auto-unfreeze, a human has been paged; keep sliding so the marker never lapses.
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionHeld, sig.Now)

	case st.UnfreezeStreak >= p.UnfreezeBuckets && p.minimumServed(st, sig.Now):
		// Release is a TRIGGER, checked every bucket once the initial hold is served; gating it on
		// expiry would serve a stale price for a whole extension. The initial hold stays a hard minimum
		// (z > 5 fires, z < 3 releases, so a wide-MAD pair can look healthy 60 s after firing).
		return Outcome{Transition: TransitionReleased}

	case sig.Now.Before(st.HoldUntil):
		// Inside the current hold segment. Nothing to decide yet; the
		// streak keeps accumulating so the checks above and the expiry
		// below read a CURRENT answer rather than a historical one.
		return p.frozen(st, TransitionHeld, sig.Now)

	case !sig.Scored:
		// Unscored expiry (post-restart bootstrap, scoring outage): slide without climbing the ladder,
		// else a rehydrated freeze could reach ESCALATED without one scored evaluation.
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionHeldUnscored, sig.Now)

	case st.ExtensionsUsed < p.MaxExtensions:
		st.ExtensionsUsed++
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionExtended, sig.Now)

	default:
		st.Escalated = true
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionEscalated, sig.Now)
	}
}

// fire opens a freeze. Inside OverrideMemory of an override it resumes that ladder: a pair still
// anomalous after a human's call is not a fresh first hold, and an escalated one pages again.
func (p Policy) fire(prev State, sig Signal) Outcome {
	st := State{
		FiredAt:      sig.Now,
		HoldUntil:    sig.Now.Add(p.initialHold(sig.Corroborated)),
		Corroborated: sig.Corroborated,
	}
	if prev.OverriddenAt.IsZero() || !sig.Now.Before(prev.OverriddenAt.Add(p.OverrideMemory)) {
		return p.frozen(st, TransitionFired, sig.Now)
	}
	st.ExtensionsUsed = prev.ExtensionsUsed
	st.OverriddenAt = prev.OverriddenAt
	if prev.Escalated {
		st.Escalated = true
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionEscalated, sig.Now)
	}
	return p.frozen(st, TransitionFired, sig.Now)
}

// initialHold picks between the two initial-hold durations.
func (p Policy) initialHold(corroborated bool) time.Duration {
	if corroborated {
		return p.InitialHold
	}
	return p.UncorroboratedInitialHold
}

// minimumServed reports whether the initial hold is served. Derived from FiredAt, not HoldUntil,
// which slides on every extension and would turn the floor into the current segment's end.
func (p Policy) minimumServed(st State, now time.Time) bool {
	return !now.Before(st.FiredAt.Add(p.initialHold(st.Corroborated)))
}

// streak advances the consecutive-healthy-bucket counter, fail-closed: unscored, still-firing
// (overlapping bands) or uncorroborated buckets earn nothing, and the counter saturates.
func (p Policy) streak(prev int, sig Signal) int {
	if !sig.Scored || sig.Fires {
		return 0
	}
	if !sig.ReleaseCorroborated {
		// Calm but uncorroborated: not evidence of health at the
		// candidate LEVEL (a held manipulation is calm too). Holds the
		// streak at zero so the ladder keeps walking — see
		// Signal.ReleaseCorroborated.
		return 0
	}
	if sig.Confidence > p.UnfreezeConfidenceMin && sig.ZScore < p.UnfreezeZScoreMax {
		if prev >= p.UnfreezeBuckets {
			return prev
		}
		return prev + 1
	}
	return 0
}

// frozen wraps a still-frozen state into an Outcome, computing the
// marker TTL from the remaining hold.
func (p Policy) frozen(st State, tr Transition, now time.Time) Outcome {
	remaining := st.HoldUntil.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	return Outcome{
		Frozen:     true,
		Transition: tr,
		State:      st,
		MarkerTTL:  remaining + p.MarkerGrace,
	}
}
