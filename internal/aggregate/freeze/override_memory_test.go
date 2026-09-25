package freeze_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
)

// TestPolicy_RefireAfterOverrideResumesTheLadder — an override ends a
// freeze, but a pair still anomalous after it is not a fresh first hold. An
// escalated ladder the operator force-unfroze used to come back 30 s later as
// a 10-minute hold with ExtensionsUsed=0, silent on the escalation counter for
// another two hours.
func TestPolicy_RefireAfterOverrideResumesTheLadder(t *testing.T) {
	p := freeze.Policy{}
	escalated := freeze.State{
		FiredAt:        t0.Add(-3 * time.Hour),
		HoldUntil:      t0.Add(20 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	remembered := escalated.Overridden(t0)
	if remembered.Active() {
		t.Fatalf("the override's memory must not be a live freeze: %+v", remembered)
	}

	at := t0.Add(30 * time.Second)
	out := p.Evaluate(remembered, firing(at))
	if !out.Frozen || out.Transition != freeze.TransitionEscalated {
		t.Fatalf("re-fire after override: Frozen=%v Transition=%q, want frozen and escalated", out.Frozen, out.Transition)
	}
	if !out.State.Escalated || out.State.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("re-fire restarted the ladder: %+v, want escalated with %d extensions used",
			out.State, freeze.DefaultMaxExtensions)
	}
	if out.State.FiredAt != at || out.State.OverriddenAt != t0 {
		t.Errorf("FiredAt=%v OverriddenAt=%v, want %v and %v", out.State.FiredAt, out.State.OverriddenAt, at, t0)
	}
	if got := out.State.HoldUntil.Sub(at); got != freeze.DefaultExtension {
		t.Errorf("escalated hold = %v, want the sliding %v", got, freeze.DefaultExtension)
	}

	mid := freeze.State{FiredAt: t0.Add(-time.Hour), HoldUntil: t0.Add(time.Minute), ExtensionsUsed: 2}
	out = p.Evaluate(mid.Overridden(t0), firing(at))
	if out.Transition != freeze.TransitionFired || out.State.ExtensionsUsed != 2 || out.State.Escalated {
		t.Errorf("mid-ladder re-fire: %q %+v, want fired with 2 extensions used", out.Transition, out.State)
	}

	late := t0.Add(freeze.DefaultOverrideMemory)
	out = p.Evaluate(remembered, firing(late))
	want := freeze.State{FiredAt: late, HoldUntil: late.Add(freeze.DefaultUncorroboratedInitialHold)}
	if out.Transition != freeze.TransitionFired || out.State != want {
		t.Errorf("re-fire after the memory: %q %+v, want a fresh %+v", out.Transition, out.State, want)
	}
}
