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

// Every RFC-5322 spelling of ONE inbox must share ONE throttle bucket.
// Case+trim alone gave `<v@x.com>` and `"n" <v@x.com>` their own 5/hour
// budgets while all of them deliver to the same mailbox, so the per-email
// cap — whose entire purpose is bounding inbox-bombing — was bypassable by
// re-spelling the target (cold audit 2026-08-03).
func TestHashEmail_RFC5322SpellingsShareOneBucket(t *testing.T) {
	t.Parallel()

	want := hashEmail("victim@example.com")
	for _, spelling := range []string{
		"victim@example.com",
		"  Victim@Example.COM  ",
		"<victim@example.com>",
		"<Victim@Example.com>",
		`"Display Name" <victim@example.com>`,
		`Display Name <victim@example.com>`,
	} {
		if got := hashEmail(spelling); got != want {
			t.Errorf("hashEmail(%q) = %s, want %s — a re-spelling of the same inbox "+
				"got its own throttle budget", spelling, got, want)
		}
	}

	// Different inboxes must still separate.
	if hashEmail("other@example.com") == want {
		t.Error("distinct addresses collided into one bucket")
	}
}

// Unparseable input must never panic or collapse to a shared bucket — it
// falls back to case+trim, which is exactly the pre-fix behaviour.
func TestHashEmail_UnparseableFallsBackToCaseTrim(t *testing.T) {
	t.Parallel()

	if hashEmail(" NOT-AN-ADDRESS ") != hashEmail("not-an-address") {
		t.Error("unparseable input lost its case/trim normalisation")
	}
	if hashEmail("garbage-a") == hashEmail("garbage-b") {
		t.Error("distinct unparseable inputs collapsed to one bucket")
	}
}
