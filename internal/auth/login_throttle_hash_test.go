package auth

import "testing"

// TestHashEmail_NormalisesBeforeHashing is the LOW finding, at the unit
// layer: hashEmail's doc promises a digest of a "lowercased email", but
// pre-fix it hashed the raw bytes and relied on every caller to normalise
// first. This pins the self-enforcing invariant — case + surrounding
// whitespace are folded away BEFORE hashing, so every spelling of one inbox
// maps to the same Redis key fragment (one throttle bucket).
func TestHashEmail_NormalisesBeforeHashing(t *testing.T) {
	canonical := hashEmail("victim@x.com")

	// Every spelling of the same inbox must collapse to the canonical hash.
	for _, spelling := range []string{
		"Victim@X.com ",
		"VICTIM@X.COM",
		"  victim@x.com",
		"victim@x.com\t",
	} {
		if got := hashEmail(spelling); got != canonical {
			t.Errorf("hashEmail(%q) = %q, want %q (normalise case + whitespace before hashing)",
				spelling, got, canonical)
		}
	}

	// Non-vacuity: a genuinely different address must NOT collide — this
	// rejects a "return a constant" degenerate normalisation.
	if got := hashEmail("someone-else@x.com"); got == canonical {
		t.Errorf("hashEmail(distinct address) collided with %q — normalisation must not erase identity", canonical)
	}
}
