package dispatcher

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// validatingFakeDecoder matches on topic like fakeDecoder, but also
// implements Validator so tests can prove Recognize distinguishes
// "topic shape matches" from "this sample would actually decode".
type validatingFakeDecoder struct {
	fakeDecoder
	validateErr error
}

func (d *validatingFakeDecoder) Validate(ev events.Event) error { return d.validateErr }

// TestRecognize verifies Recognize reports a match with the decoder name for handled topics, false for unhandled,
// and leaves the dispatcher's stats counters untouched.
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

// TestRecognize_ValidateFailureIsRecognitionGap: a decoder whose Matches() claims the topic
// shape but whose sample would not decode must surface as a recognition gap (ok == false), not a
// false "recognized". A decoder without Validator (fakeDecoder) keeps shape-only behavior.
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

// TestRecognize_OverlappingMatchDoesNotFallThrough: when the first matching decoder's Validate() fails, Recognize
// must report a gap, not fall through, because dispatchOne commits to the first Matches()==true decoder.
func TestRecognize_OverlappingMatchDoesNotFallThrough(t *testing.T) {
	first := &validatingFakeDecoder{
		fakeDecoder: fakeDecoder{name: "first", topic0: "overlap"},
		validateErr: errors.New("scval: unexpected arity"),
	}
	second := &fakeDecoder{name: "second", topic0: "overlap"}
	disp := New(first, second)

	if name, ok := disp.Recognize(events.Event{Topic: []string{"overlap"}}); ok {
		t.Errorf("Recognize(overlap) = (%q,%v), want ok=false: first decoder's Validate failed, "+
			"so this is a recognition gap, not a match on %q", name, ok, "second")
	}
}

// TestRecognize_PanicResolvesToUnrecognised: Recognize runs from callers that do not recover, so one malformed row
// must not take down a verification run. A panic must resolve to not recognised so the recognition axis goes RED;
// naming the panicking decoder as owner would be a false green.
func TestRecognize_PanicResolvesToUnrecognised(t *testing.T) {
	dec := &panickyDecoder{name: "panic-recognise-src", contract: "CPOISON", panicInMatch: true}
	disp := New(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	name, ok := disp.Recognize(events.Event{ContractID: "CPOISON", Ledger: 9, TxHash: "def", OperationIndex: 1})

	if ok {
		t.Errorf("Recognize reported ok=true for a decoder that panicked — a shape nothing can decode must never be certified as owned")
	}
	if name != "" {
		t.Errorf("Recognize returned owner %q after a panic, want empty", name)
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name)) - before; got != 1 {
		t.Errorf("panic counter rose by %v, want 1 — an ops-path panic must be as visible as an ingest one", got)
	}

	// A healthy decoder still recognises normally after the guard.
	good := &panickyDecoder{name: "healthy-src", contract: "CGOOD"}
	if n, ok := New(good).Recognize(events.Event{ContractID: "CGOOD"}); !ok || n != "healthy-src" {
		t.Errorf("healthy decoder: got (%q, %v), want (healthy-src, true)", n, ok)
	}
}
