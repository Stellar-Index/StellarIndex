package dispatcher

import (
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// validatingFakeDecoder matches on topic like fakeDecoder, but also
// implements Validator so tests can prove Recognize distinguishes
// "topic shape matches" from "this sample would actually decode".
type validatingFakeDecoder struct {
	fakeDecoder
	validateErr error
}

func (d *validatingFakeDecoder) Validate(ev events.Event) error { return d.validateErr }

// TestRecognize verifies Recognize reports a match (with decoder name)
// for handled topics, false for unhandled, and — importantly — has no
// side effects on the dispatcher's stats counters (so a recognition
// audit doesn't pollute the live unmatched/seen tallies).
func TestRecognize(t *testing.T) {
	disp := New(
		&fakeDecoder{name: "alpha", topic0: "swap"},
		&fakeDecoder{name: "beta", topic0: "sync"},
	)

	if name, ok := disp.Recognize(events.Event{Topic: []string{"swap"}}); !ok || name != "alpha" {
		t.Errorf("Recognize(swap) = (%q,%v), want (alpha,true)", name, ok)
	}
	if name, ok := disp.Recognize(events.Event{Topic: []string{"sync"}}); !ok || name != "beta" {
		t.Errorf("Recognize(sync) = (%q,%v), want (beta,true)", name, ok)
	}

	before := disp.unmatchedHits
	if _, ok := disp.Recognize(events.Event{Topic: []string{"mystery_topic"}}); ok {
		t.Error("Recognize(mystery_topic) = true, want false (recognition gap)")
	}
	if disp.unmatchedHits != before {
		t.Errorf("Recognize mutated unmatchedHits (%d → %d); must be side-effect-free", before, disp.unmatchedHits)
	}
}

// TestRecognize_ValidateFailureIsRecognitionGap proves RLT-137: a
// decoder whose Matches() reports the topic shape as owned, but whose
// sample would not actually decode, must surface as a recognition gap
// (ok == false) rather than a false "recognized". A decoder that does
// not implement Validator (fakeDecoder) keeps today's shape-only
// behavior unchanged.
func TestRecognize_ValidateFailureIsRecognitionGap(t *testing.T) {
	bad := &validatingFakeDecoder{
		fakeDecoder: fakeDecoder{name: "gamma", topic0: "unstable"},
		validateErr: errors.New("scval: unexpected arity"),
	}
	good := &validatingFakeDecoder{
		fakeDecoder: fakeDecoder{name: "delta", topic0: "stable"},
		validateErr: nil,
	}
	disp := New(bad, good)

	if name, ok := disp.Recognize(events.Event{Topic: []string{"unstable"}}); ok {
		t.Errorf("Recognize(unstable) = (%q,%v), want ok=false (Validate failed, so this shape is a recognition gap, not recognized)", name, ok)
	}
	if name, ok := disp.Recognize(events.Event{Topic: []string{"stable"}}); !ok || name != "delta" {
		t.Errorf("Recognize(stable) = (%q,%v), want (delta,true)", name, ok)
	}
}
