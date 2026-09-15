package v1

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// `balanced: false` is a statement that something did not add up, with
// no way to find out what.
//
// Every reason was already being computed. Each census publishes a
// Check() returning the invariant it broke; each adjacent stage pair
// reports the arithmetic that failed. All of it was reduced to a
// boolean at the funnel boundary and the sentences went to a log — the
// same shape as a listing census computed for a WARN line and dropped,
// one surface over.

// An unbalanced funnel must name what did not close, and the name must
// be specific enough to act on: which check, and the numbers that
// disagreed.
func TestRWAFunnelImbalance_NamesWhatDidNotClose(t *testing.T) {
	// A classic census whose own arithmetic does not close. Check()
	// already has a sentence for this; it never reached the wire.
	m := rwaMembership{
		refusals: map[string]int{},
		census: timescale.Sep1BoundCensus{
			IssuersWithPayload: 10,
			IssuersDeclaring:   4,
			Entries:            9,
			EntriesBound:       9,
			EntriesKept:        9,
			// Deliberately inconsistent with the counts above.
			IssuersDeclaringNothing: 99,
		},
	}
	f := rwaFunnelOf(m, rwaCatalogueJoin{}, 0, 0, 0, nil)

	if f.Balanced {
		t.Fatal("the fixture must produce an unbalanced funnel for this to mean anything")
	}
	if f.Imbalance == "" {
		t.Fatal("balanced:false published no reason — a reader is told the accounting " +
			"does not close and given no way to find out what does not close")
	}
	if !strings.Contains(f.Imbalance, "classic census") {
		t.Errorf("imbalance = %q, want it to name the check that failed", f.Imbalance)
	}
	// The sentence the census already produced, carried rather than
	// restated — so the wire and the log can never give two accounts.
	if why := m.census.Check(); why != "" && !strings.Contains(f.Imbalance, why) {
		t.Errorf("imbalance = %q, want it to carry the census's own sentence %q", f.Imbalance, why)
	}
}

// The served-set disagreement is its own check and must name itself.
// It is the one that catches a row lost between the arms and the row
// list, which no per-arm census can see.
func TestRWAFunnelImbalance_NamesAServedSetDisagreement(t *testing.T) {
	m := rwaMembership{refusals: map[string]int{}}
	// Two rows listed, none counted by any arm.
	f := rwaFunnelOf(m, rwaCatalogueJoin{}, 0, 0, 0, []RWAAsset{{}, {}})

	if f.Balanced {
		t.Fatal("two rows listed against zero counted across the arms reported as balanced")
	}
	if !strings.Contains(f.Imbalance, "served set") {
		t.Errorf("imbalance = %q, want it to name the served-set check", f.Imbalance)
	}
	if !strings.Contains(f.Imbalance, "2 rows listed") {
		t.Errorf("imbalance = %q, want the numbers that disagreed", f.Imbalance)
	}
}

// balanced and imbalance are one statement in two forms and may never
// disagree: a reader branching on either must reach the same answer.
// Without this a later check could set the boolean and forget the
// sentence, which is how the field went missing in the first place.
func TestRWAFunnelImbalance_AgreesWithBalanced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		m      rwaMembership
		assets []RWAAsset
	}{
		{name: "balanced", m: rwaMembership{refusals: map[string]int{}}},
		{
			name:   "unbalanced",
			m:      rwaMembership{refusals: map[string]int{}},
			assets: []RWAAsset{{}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := rwaFunnelOf(tc.m, rwaCatalogueJoin{}, 0, 0, 0, tc.assets)
			if f.Balanced != (f.Imbalance == "") {
				t.Errorf("balanced = %v beside imbalance = %q: the two are one "+
					"statement and cannot disagree", f.Balanced, f.Imbalance)
			}
		})
	}
}
