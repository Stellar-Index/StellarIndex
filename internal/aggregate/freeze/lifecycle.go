package freeze

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ─── ADR-0019 freeze lifecycle ────────────────────────────────────
//
// ADR-0019 §"Freeze duration" specifies a freeze as a HOLD with an
// extension ladder, not as a per-bucket decision:
//
//	Initial: 30 minutes
//	Re-evaluation at expiry: if the freeze condition still holds,
//	  extend by 30 min, up to 4 extensions (2 hours total)
//	After 4 extensions: escalate to operator review (P1 alert);
//	  freeze stays active until manual unfreeze
//	Auto-unfreeze trigger: confidence rises above 0.30 AND z_score
//	  falls below 3.0 for two consecutive buckets
//
// Until this file landed, none of that existed. The shipped freeze
// was "write a Redis marker with a 5-minute TTL on every bucket the
// 3-signal AND fires for, and stop writing when it stops firing", so
// the RELEASE condition was the negation of the FIRE condition —
// evaluated on a SINGLE bucket, with the release band strictly wider
// than the fire band (release at z ≤ 5 rather than the ADR's z < 3,
// release on confidence ≥ 0.45 rather than the ADR's > 0.30, and
// release the moment a second source appears at all). One clean-ish
// bucket therefore published the manipulated price the freeze had
// just refused, and an attacker who could produce a single trade on a
// second venue — or nudge z from 5.1 to 4.9 for one bucket — cleared
// the freeze while the manipulation was still in force.
//
// The state machine below is deliberately a pure function of
// (previous state, this bucket's signal): no clock reads, no IO, no
// package state. The orchestrator owns persistence and side effects;
// this file owns the policy, so the policy is exhaustively testable
// at table speed.
//
// Two properties are load-bearing and easy to lose in a refactor:
//
//   - The initial hold is a MINIMUM. Auto-unfreeze cannot fire inside
//     it however healthy the last two buckets looked; from the end of
//     it the ADR's trigger is evaluated on every bucket. Releasing
//     inside the minimum on "the current bucket looks fine" is the
//     evasion above: the freeze fires at z > 5 and the streak needs
//     only z < 3, so on a wide-MAD asset a price still well away from
//     the last-known-good reads healthy two buckets running.
//   - Release requires POSITIVE evidence of health (the ADR's
//     two-consecutive-bucket condition), never merely the absence of
//     the fire condition. A bucket the scorer could not score at all
//     (no baseline, no previous VWAP — the state after an aggregator
//     restart) resets the streak rather than crediting it.

// Freeze-lifecycle defaults, per ADR-0019 §"Freeze duration" and
// §"Auto-unfreeze trigger". Operators tune via `[anomaly.phase2]`;
// unset fields fall back to these.
const (
	// DefaultInitialHold — ADR-0019's flat "Initial: 30 minutes",
	// applied to any freeze that fired on a pair with a corroborating
	// lens available this bucket (see [Signal.Corroborated]).
	DefaultInitialHold = 30 * time.Minute

	// DefaultUncorroboratedInitialHold is a documented DEVIATION from
	// ADR-0019's flat 30 minutes, for freezes on pairs where no
	// corroborating lens produced a reading at all: no configured
	// triangulation chain compared a composite against the direct
	// price, and no cross-oracle reference set met the trust floor.
	//
	// Why the durations must not be uniform. A freeze serves the
	// last-known-good price for its whole duration, so a FALSE freeze
	// is its own money bug (the MNY-22 class: a stale leg laundered
	// into a derived pair). The false-freeze rate is not uniform
	// across the index — it concentrates entirely on thin books, where
	// a single venue's own history is the only reference the 3-signal
	// AND has, and where a $19/hour book can produce a z > 5 bucket
	// from one ordinary trade. Charging that population the full
	// 30-minute stale-price bill for a decision taken on one lens is
	// the wrong trade; charging it 10 minutes is not.
	//
	// 10 minutes, not 5: it must comfortably outlast both the longest
	// default aggregation window that can carry the spike (5m) and
	// several 30s ticks, so a one-bucket spike cannot be waited out.
	// Extensions are NOT scaled — an uncorroborated pair whose anomaly
	// persists climbs the same 30-minute ladder to the same 2-hour
	// escalation, so the shortened hold only shortens the FIRST
	// decision, which is where the false-positive risk sits.
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
)

// DefaultMarkerGrace is how long the Redis marker outlives the hold
// it encodes. It exists so a missed aggregator tick — or a restart —
// cannot blink `flags.frozen` off mid-hold on the serving path.
//
// Deliberately [cachekeys.FreezeTTL]: that constant used to BE the
// freeze duration, and after this file it means "how long a freeze
// survives aggregator silence", which is the only job a TTL was ever
// suited for. A freeze's DURATION is now state, not an expiry.
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
}

// WithDefaults returns a copy with every unset (zero-valued) field
// replaced by its ADR-0019 default.
//
// Zero is the "unset" sentinel for every field here. A zero-length hold
// is "don't freeze", which is achieved by disabling the Phase 2 gate
// rather than by a zero duration.
//
// Corrected 2026-08-04. This block used to justify the sentinel for
// UnfreezeConfidenceMin by claiming a zero bound "would make the
// auto-unfreeze condition unreachable, since confidence is clamped to
// [0, 1] and the comparison is strict". That is backwards: the test is
// `sig.Confidence > p.UnfreezeConfidenceMin`, so a zero bound makes
// that leg MAXIMALLY reachable — satisfied by any positive confidence.
// The sentinel is still the right behaviour (an operator who leaves the
// field unset gets the ADR-0019 default rather than an unbounded
// release gate), but the stated reason was the opposite of the truth,
// and an operator who deliberately wants z to be the only release gate
// silently gets 0.30 instead of the 0 they asked for.
//
// The same block advertised "set MaxExtensions negative" to escalate
// immediately. That escape hatch is unreachable through the supported
// path: config validation rejects max_extensions < 0.
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
	if p.MarkerGrace <= 0 {
		p.MarkerGrace = DefaultMarkerGrace
	}
	return p
}

// State is one pair's freeze-lifecycle state. It is the authority for
// the freeze's LIFECYCLE (when it fired, how much of the ladder it has
// climbed, whether it has escalated); the Redis marker remains the
// authority for the SERVING path's `flags.frozen`, and carries a copy
// of this state so a marker dump is self-describing and so the state
// survives an aggregator restart.
//
// The zero value means "not frozen" — see [State.Active].
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

	// Corroborated records whether a corroborating lens had produced a
	// reading for this pair at fire time — the input that chose
	// between the two initial-hold durations. Kept so an operator
	// reading a marker can tell WHY this freeze's first hold was 10
	// minutes rather than 30.
	Corroborated bool `json:"corroborated,omitempty"`
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

	// Scored reports whether Confidence and ZScore below are real
	// measurements for this bucket. False means the bucket could not
	// be scored (no baseline row, no previous VWAP to derive a return
	// from, Phase 1 firing before Phase 2 ran) — which is the ABSENCE
	// of evidence, never evidence of health, so it resets the
	// auto-unfreeze streak.
	Scored bool

	// Confidence / ZScore are this bucket's multi-factor confidence
	// score and its OBSERVATION-based z-score — the two values
	// ADR-0019's auto-unfreeze condition reads. Both are already
	// computed by the orchestrator's refresh loop; this policy never
	// recomputes them.
	//
	// ZScore is deliberately the observation-based score and not the
	// wider sustained-drift statistic: the drift signal latches for up
	// to 30 days and, wired into a publication decision, cannot
	// self-clear (see orchestrator/confidence.go's header). It reaches
	// this decision through Confidence instead, which is graded and
	// self-correcting — the drift statistic already feeds
	// confidence.Compute's z input.
	Confidence float64
	ZScore     float64

	// Corroborated reports whether a corroborating lens produced a
	// reading for this pair in this bucket: a configured triangulation
	// chain compared a composite against the direct price, or a
	// cross-oracle reference set met the trust floor. Read only when
	// the freeze FIRES (it selects the initial hold).
	//
	// This is "was a second lens consulted", NOT "did the second lens
	// agree". A lens that actively DISAGREES is the strongest evidence
	// the freeze is true, and must not buy the shorter hold — see the
	// ADR-0019 amendment for why the agreement-only reading was
	// rejected.
	Corroborated bool
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
	// TransitionHeldUnscored — the hold expired on a bucket that could
	// not be scored (post-restart confidence bootstrap, scoring
	// outage): the hold slides WITHOUT consuming an extension, because
	// an unscored bucket asked nothing about recovery. The ladder
	// resumes when scoring does.
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
		return p.frozen(State{
			FiredAt:      sig.Now,
			HoldUntil:    sig.Now.Add(p.initialHold(sig.Corroborated)),
			Corroborated: sig.Corroborated,
		}, TransitionFired, sig.Now)
	}

	st := prev
	st.UnfreezeStreak = p.streak(prev.UnfreezeStreak, sig)

	switch {
	case st.Escalated:
		// ADR-0019: an escalated freeze "stays active until manual
		// unfreeze". Auto-unfreeze is suppressed on purpose — the
		// ladder already spent two hours asking whether this pair had
		// recovered, and a human has been paged. The hold keeps
		// sliding so the marker never lapses under a live aggregator.
		st.HoldUntil = sig.Now.Add(p.Extension)
		return p.frozen(st, TransitionHeld, sig.Now)

	case st.UnfreezeStreak >= p.UnfreezeBuckets && p.minimumServed(st, sig.Now):
		// Earned release. ADR-0019 phrases auto-unfreeze as a TRIGGER,
		// so it is evaluated on every bucket once the INITIAL hold has
		// been served — not only at a ladder expiry. Gating it on
		// expiry instead would make a pair that recovered one bucket
		// after an extension was granted wait out the full 30-minute
		// extension serving a stale last-known-good price, which is
		// the money bug (MNY-22 class) the freeze itself trades
		// against; there is no security argument for it, because the
		// streak is what proves recovery and the streak is not easier
		// to satisfy at an expiry instant than between two.
		//
		// The initial hold IS still a hard minimum. It is load-bearing
		// on volatile pairs: the freeze fires at z > 5 and the streak
		// only needs z < 3, so on a wide-MAD asset a price still well
		// away from the last-known-good can read "healthy" two buckets
		// running. The minimum hold is what stops those two buckets —
		// possibly 60 seconds after the freeze — from ending it.
		return Outcome{Transition: TransitionReleased}

	case sig.Now.Before(st.HoldUntil):
		// Inside the current hold segment. Nothing to decide yet; the
		// streak keeps accumulating so the checks above and the expiry
		// below read a CURRENT answer rather than a historical one.
		return p.frozen(st, TransitionHeld, sig.Now)

	case !sig.Scored:
		// The hold expired on a bucket that could NOT be scored — the
		// ~30-minute post-restart confidence bootstrap, or a mid-freeze
		// scoring outage. An extension is supposed to mean "we asked
		// whether this pair recovered and it had not"; an unscored
		// bucket asked NOTHING, so it must not climb the ladder.
		// Pre-fix this was the restart-rehydration trap the runbook
		// documents: a freeze rehydrated across a deploy could never
		// accumulate an unfreeze streak (unscored ⇒ streak=0) while
		// its expiries still burned extensions, so it marched
		// deterministically to ESCALATED — operator-only — without a
		// single scored evaluation (live occurrence: crypto:XLM/
		// fiat:GBP 24h, 2026-08-05, escalated entirely inside the
		// v0.25.0 restart's bootstrap window). Slide the hold and wait
		// for scoring to return; the ladder resumes exactly where it
		// was. ADR-0019's 2-hour escalation budget thereby counts two
		// hours of SCORED asking, which is what it always meant.
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

// initialHold picks between the two initial-hold durations.
func (p Policy) initialHold(corroborated bool) time.Duration {
	if corroborated {
		return p.InitialHold
	}
	return p.UncorroboratedInitialHold
}

// minimumServed reports whether the freeze has served its INITIAL
// hold — the floor below which no auto-unfreeze may fire, however
// healthy the last two buckets looked.
//
// Derived from FiredAt + the initial hold rather than stored, so it
// stays a property of the freeze rather than of the ladder: HoldUntil
// slides forward on every extension, and comparing against it would
// silently turn the floor into "the current segment's end".
func (p Policy) minimumServed(st State, now time.Time) bool {
	return !now.Before(st.FiredAt.Add(p.initialHold(st.Corroborated)))
}

// streak advances (or resets) the consecutive-healthy-bucket counter
// that ADR-0019's auto-unfreeze condition requires.
//
// Fail-closed in three places, each of which has been a real bug
// class somewhere in this pipeline: an unscored bucket earns nothing,
// a bucket that still FIRES earns nothing even if an operator has
// configured overlapping fire/unfreeze bands, and the counter
// saturates rather than growing without bound.
func (p Policy) streak(prev int, sig Signal) int {
	if !sig.Scored || sig.Fires {
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
