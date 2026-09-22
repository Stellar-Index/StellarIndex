package main

import "testing"

// TestVerifyHashDBRangeFlags_NeitherSetIsNotRequested pins the default:
// with neither -verify-hashdb-from nor -verify-hashdb-to set, the ordinary
// full-ingest path runs unchanged.
func TestVerifyHashDBRangeFlags_NeitherSetIsNotRequested(t *testing.T) {
	from, to, requested, err := verifyHashDBRangeFlags{}.validate()
	if err != nil {
		t.Fatalf("validate() err = %v, want nil", err)
	}
	if requested {
		t.Fatalf("validate() requested = true, want false when neither flag is set")
	}
	if from != 0 || to != 0 {
		t.Fatalf("validate() = (%d, %d), want (0, 0) when not requested", from, to)
	}
}

// TestVerifyHashDBRangeFlags_ExplicitOlderRangeIsAccepted is T122: before
// this type existed there was no way to express a verify request for a
// ledger range OLDER than the live-tip trailing window — hashDBVerifySweep
// only ever computed [from,to] off lastAppended. A range far behind any
// plausible live tip (e.g. genesis-era ledgers) must validate and pass
// through unchanged, proving the CLI path is decoupled from the tip.
func TestVerifyHashDBRangeFlags_ExplicitOlderRangeIsAccepted(t *testing.T) {
	f := verifyHashDBRangeFlags{from: 1_000_000, to: 1_000_500}
	from, to, requested, err := f.validate()
	if err != nil {
		t.Fatalf("validate() err = %v, want nil", err)
	}
	if !requested {
		t.Fatalf("validate() requested = false, want true when both flags are set")
	}
	if from != 1_000_000 || to != 1_000_500 {
		t.Fatalf("validate() = (%d, %d), want (1000000, 1000500)", from, to)
	}
}

// TestVerifyHashDBRangeFlags_OnlyOneSetIsRejected guards the half-set case
// (an operator typo'ing one flag) so it fails loudly rather than silently
// falling back to full ingest or to a zero-valued range.
func TestVerifyHashDBRangeFlags_OnlyOneSetIsRejected(t *testing.T) {
	for _, f := range []verifyHashDBRangeFlags{
		{from: 100, to: 0},
		{from: 0, to: 100},
	} {
		_, _, requested, err := f.validate()
		if err == nil {
			t.Fatalf("validate(%+v) err = nil, want error when only one flag is set", f)
		}
		if !requested {
			t.Fatalf("validate(%+v) requested = false, want true (so the caller surfaces the error instead of silently ingesting)", f)
		}
	}
}

// TestVerifyHashDBRangeFlags_FromAfterToIsRejected guards an inverted
// range, which would otherwise reach hashDBVerifyPass and produce a
// nonsensical stream call.
func TestVerifyHashDBRangeFlags_FromAfterToIsRejected(t *testing.T) {
	_, _, requested, err := verifyHashDBRangeFlags{from: 200, to: 100}.validate()
	if err == nil {
		t.Fatalf("validate() err = nil, want error when from > to")
	}
	if !requested {
		t.Fatalf("validate() requested = false, want true")
	}
}
