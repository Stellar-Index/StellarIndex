package auth

import "testing"

// TestHashEmail_NormalisesBeforeHashing is the LOW finding, at the unit
// layer: hashEmail's doc promises a digest of a "lowercased email", but
// pre-fix it hashed the raw bytes and relied on every caller to normalise
// first. This pins the self-enforcing invariant — case + surrounding
// whitespace are folded away BEFORE hashing, so every spelling of one inbox
// maps to the same Redis key fragment (one throttle bucket).
func TestHashEmail_NormalisesBeforeHashing(t *testing.T) {
	canonical := HashEmail("victim@x.com")

	// Every spelling of the same inbox must collapse to the canonical hash.
	for _, spelling := range []string{
		"Victim@X.com ",
		"VICTIM@X.COM",
		"  victim@x.com",
		"victim@x.com\t",
	} {
		if got := HashEmail(spelling); got != canonical {
			t.Errorf("HashEmail(%q) = %q, want %q (normalise case + whitespace before hashing)",
				spelling, got, canonical)
		}
	}

	// Non-vacuity: a genuinely different address must NOT collide — this
	// rejects a "return a constant" degenerate normalisation.
	if got := HashEmail("someone-else@x.com"); got == canonical {
		t.Errorf("HashEmail(distinct address) collided with %q — normalisation must not erase identity", canonical)
	}
}

// Every RFC-5322 spelling of ONE inbox must share ONE throttle bucket.
// Case+trim alone gave `<v@x.com>` and `"n" <v@x.com>` their own 5/hour
// budgets while all of them deliver to the same mailbox, so the per-email
// cap — whose entire purpose is bounding inbox-bombing — was bypassable by
// re-spelling the target (cold audit 2026-08-03).
func TestHashEmail_RFC5322SpellingsShareOneBucket(t *testing.T) {
	t.Parallel()

	want := HashEmail("victim@example.com")
	for _, spelling := range []string{
		"victim@example.com",
		"  Victim@Example.COM  ",
		"<victim@example.com>",
		"<Victim@Example.com>",
		`"Display Name" <victim@example.com>`,
		`Display Name <victim@example.com>`,
	} {
		if got := HashEmail(spelling); got != want {
			t.Errorf("HashEmail(%q) = %s, want %s — a re-spelling of the same inbox "+
				"got its own throttle budget", spelling, got, want)
		}
	}

	// Different inboxes must still separate.
	if HashEmail("other@example.com") == want {
		t.Error("distinct addresses collided into one bucket")
	}
}

// Unparseable input must never panic or collapse to a shared bucket — it
// falls back to case+trim, which is exactly the pre-fix behaviour.
func TestHashEmail_UnparseableFallsBackToCaseTrim(t *testing.T) {
	t.Parallel()

	if HashEmail(" NOT-AN-ADDRESS ") != HashEmail("not-an-address") {
		t.Error("unparseable input lost its case/trim normalisation")
	}
	if HashEmail("garbage-a") == HashEmail("garbage-b") {
		t.Error("distinct unparseable inputs collapsed to one bucket")
	}
}

// RLT-324 / RSEC-N2: a `+tag` subaddress or a gmail dot re-spelling must
// share the target inbox's bucket, or an attacker mints a fresh 5/hour
// budget per spelling and the per-email cap never engages.
func TestHashEmail_PlusTagAndGmailDotFolding(t *testing.T) {
	t.Parallel()

	want := HashEmail("victim@gmail.com")
	for _, spelling := range []string{
		"victim@gmail.com",
		"victim+1@gmail.com",
		"victim+anything@gmail.com",
		"vic.tim@gmail.com",
		"v.i.c.t.i.m@gmail.com",
		"VIC.TIM+xyz@GMAIL.com",
		"victim@googlemail.com",
		"vic.tim+1@googlemail.com",
	} {
		if got := HashEmail(spelling); got != want {
			t.Errorf("HashEmail(%q) = %s, want %s — a +tag/gmail-dot re-spelling "+
				"of the same inbox got its own throttle budget", spelling, got, want)
		}
	}

	// +tag stripping applies on non-gmail domains too (general subaddressing
	// convention), but dot-folding must NOT — a non-gmail provider may treat
	// dots as significant.
	plain := HashEmail("victim@example.com")
	if got := HashEmail("victim+work@example.com"); got != plain {
		t.Errorf("HashEmail(+tag on non-gmail) = %s, want %s", got, plain)
	}
	if got := HashEmail("vic.tim@example.com"); got == plain {
		t.Error("dot-folding must be gmail-only: vic.tim@example.com wrongly collapsed to victim@example.com")
	}

	// Non-vacuity: a genuinely different gmail inbox must not collide.
	if HashEmail("someoneelse@gmail.com") == want {
		t.Error("distinct gmail inbox collided with victim@gmail.com")
	}
}
