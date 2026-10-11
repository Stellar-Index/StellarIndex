package freeze

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ADR-0019 freeze lifecycle: a HOLD with an extension ladder (30 min, up to 4 x 30 min, then P1
// and manual unfreeze only), released when confidence > 0.30 AND z < 3.0 for two consecutive
// buckets, never on one bucket's negated fire condition. The initial hold is a MINIMUM and
// release needs POSITIVE evidence (an unscorable bucket resets the streak); the machine is pure,
// the orchestrator owns persistence.

// Freeze-lifecycle defaults per ADR-0019; operators tune via `[anomaly.phase2]`, and unset
// fields fall back to these.
const (
	// DefaultInitialHold is ADR-0019's 30-minute hold, for a pair a corroborating lens read this
	// bucket (see [Signal.Corroborated]).
	DefaultInitialHold = 30 * time.Minute

	// DefaultUncorroboratedInitialHold deviates from ADR-0019 for pairs no lens read: false
	// freezes serve a stale price and concentrate on thin books. Extensions are NOT scaled.
	DefaultUncorroboratedInitialHold = 10 * time.Minute

	// DefaultExtension is ADR-0019's 30-minute extension.
	DefaultExtension = 30 * time.Minute

	// DefaultMaxExtensions is ADR-0019's 4 extensions (2 hours) before escalation to an operator.
	DefaultMaxExtensions = 4

	// DefaultUnfreezeConfidenceMin is ADR-0019's auto-unfreeze confidence floor (above 0.30).
	DefaultUnfreezeConfidenceMin = 0.30

	// DefaultUnfreezeZScoreMax is ADR-0019's auto-unfreeze z-score ceiling (below 3.0).
	DefaultUnfreezeZScoreMax = 3.0

	// DefaultUnfreezeBuckets is ADR-0019's two consecutive buckets for auto-unfreeze.
	DefaultUnfreezeBuckets = 2

	// DefaultOverrideMemory is how long a force-unfrozen ladder is remembered, so a re-fire inside
	// it resumes escalated rather than a fresh hold.
	DefaultOverrideMemory = 2 * time.Hour
)

// DefaultMarkerGrace is how long the Redis marker outlives its hold so a missed tick cannot
// blink flags.frozen off; duration is state, not a TTL.
var DefaultMarkerGrace = cachekeys.FreezeTTL

// Policy is the operator-tunable ADR-0019 lifecycle; the zero value is valid via [Policy.WithDefaults].
type Policy struct {
	// InitialHold is the minimum freeze duration for a corroborated freeze.
	InitialHold time.Duration

	// UncorroboratedInitialHold is the minimum when no lens read the pair; see
	// [DefaultUncorroboratedInitialHold] for why it is shorter.
	UncorroboratedInitialHold time.Duration

	// Extension is added at each hold expiry the freeze has not earned its release at.
	Extension time.Duration

	// MaxExtensions is how many extensions are granted before escalation to operator review.
	MaxExtensions int

	// UnfreezeConfidenceMin / UnfreezeZScoreMax / UnfreezeBuckets are the auto-unfreeze condition:
	// confidence strictly above the min AND z strictly below the max, for UnfreezeBuckets
	// CONSECUTIVE buckets.
	UnfreezeConfidenceMin float64
	UnfreezeZScoreMax     float64
	UnfreezeBuckets       int

	// MarkerGrace is how far past the hold the Redis marker's TTL is set.
	MarkerGrace time.Duration

	// OverrideMemory is how long a re-fire after an override resumes the overridden ladder.
	OverrideMemory time.Duration
}

// WithDefaults replaces zero fields with ADR-0019 defaults; a zero UnfreezeConfidenceMin would
// pass any positive confidence, so 0 becomes 0.30.
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

// State is one pair's freeze-lifecycle authority (the marker carries a copy for restarts); the
// zero value is "not frozen".
type State struct {
	// FiredAt is when the freeze first engaged; preserved across extensions.
	FiredAt time.Time `json:"fired_at,omitempty"`

	// HoldUntil is when the current hold expires and the lifecycle re-evaluates; it slides
	// forward on every extension.
	HoldUntil time.Time `json:"hold_until,omitempty"`

	// ExtensionsUsed counts granted extensions; at [Policy.MaxExtensions] the next expiry escalates.
	ExtensionsUsed int `json:"extensions_used,omitempty"`

	// Escalated records that the ladder ran out and an operator must act. An escalated freeze
	// does NOT auto-unfreeze (ADR-0019: it stays active until manual unfreeze).
	Escalated bool `json:"escalated,omitempty"`

	// UnfreezeStreak counts CONSECUTIVE buckets meeting the auto-unfreeze condition; any bucket
	// that does not, including an unscorable one, resets it to 0.
	UnfreezeStreak int `json:"unfreeze_streak,omitempty"`

	// Corroborated records whether a lens had read the pair at fire time, so an operator can tell
	// why the first hold was 10 minutes rather than 30.
	Corroborated bool `json:"corroborated,omitempty"`

	// OverriddenAt is when an out-of-band override last ended this pair's ladder. On an inactive
	// State it is the remembered ladder ([DefaultOverrideMemory]); on an active one it marks a
	// freeze that resumed that ladder.
	OverriddenAt time.Time `json:"overridden_at,omitempty"`
}

// Overridden returns the inactive State an override leaves behind: the ended ladder,
// remembered from now.
func (s State) Overridden(now time.Time) State {
	return State{ExtensionsUsed: s.ExtensionsUsed, Escalated: s.Escalated, OverriddenAt: now}
}

// Active reports whether the state describes a live freeze.
func (s State) Active() bool { return !s.FiredAt.IsZero() }

// Signal is what one closed bucket contributes to the lifecycle.
type Signal struct {
	// Now is the evaluation instant, passed in so the policy stays a pure function.
	Now time.Time

	// Fires is the ADR-0019 3-signal AND for THIS bucket (or the Phase 1 class-threshold
	// decision). It engages a freeze; it does NOT keep one alive and does NOT extend one.
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
	// publish). Calm legs alone cannot release: a held manipulation reads calm too. False with no
	// lens, so those freezes end only by override or escalation.
	ReleaseCorroborated bool
}

// Transition names what the lifecycle did on one evaluation; the strings are stable (logs and
// metric labels).
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
	// TransitionHeldUnscored: the hold expired on an unscored bucket and slid without consuming an
	// extension.
	TransitionHeldUnscored Transition = "held_unscored"
	// TransitionEscalated — the ladder ran out; operator review (P1).
	TransitionEscalated Transition = "escalated"
	// TransitionReleased: the auto-unfreeze condition was met at expiry.
	TransitionReleased Transition = "released"
	// TransitionOverridden: the operator cleared the marker out of band, ending the freeze and
	// its ladder immediately.
	TransitionOverridden Transition = "overridden"
)

// Outcome is one evaluation's result.
type Outcome struct {
	// Frozen is what the caller acts on: true means do not publish this bucket.
	Frozen bool

	// Transition is what changed, for logs + metrics.
	Transition Transition

	// State is the state to persist. Zero (inactive) on release.
	State State

	// MarkerTTL is the Redis marker TTL: remaining hold plus [Policy.MarkerGrace]. Zero when not
	// frozen.
	MarkerTTL time.Duration
}

// Evaluate advances the lifecycle by one bucket; pure, so persistence, marker IO, metrics and
// logging are the caller's.
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
		// Escalated: no auto-unfreeze, a human is paged; keep sliding so the marker never lapses.
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionHeld, sig.Now)

	case st.UnfreezeStreak >= p.UnfreezeBuckets && p.minimumServed(st, sig.Now):
		// Release is a TRIGGER, checked every bucket once the initial hold is served; gating it
		// on expiry would serve a stale price for a whole extension. The initial hold stays a
		// hard minimum (z > 5 fires, z < 3 releases, so a wide-MAD pair can look healthy 60 s
		// after firing).
		return Outcome{Transition: TransitionReleased}

	case sig.Now.Before(st.HoldUntil):
		// Inside the hold: the streak keeps accumulating so the checks above and the expiry
		// below read a CURRENT answer.
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
		// Calm but uncorroborated is no evidence of health at the candidate LEVEL (a held
		// manipulation is calm too); see Signal.ReleaseCorroborated.
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

// frozen wraps a still-frozen state into an Outcome with the marker TTL.
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
