package chops

import (
	"strings"
	"testing"
)

// TestWalkCoverageErr is the RLT-282 regression guard for the rule every
// galexie-walking subcommand in this package now applies before it
// reports on what it saw: DELIVERED must equal REQUESTED.
//
// The defect it pins is not "the gate lacked a check" — ch-gate had
// plenty of checks. It is that every one of them measured the walk
// against ITSELF. `ch.LedgerRows != uint64(walked)` compares the rows
// ClickHouse holds for the range to the number of ledgers the walk
// produced, so when a wrong bucket or a hole tolerated by
// TolerateTrailingMissing shortens the walk, both sides shrink together
// and the gate certifies the slice it happened to read. The subset of
// the range that was never opened has nothing to disagree with.
func TestWalkCoverageErr(t *testing.T) {
	t.Parallel()

	const (
		from   = uint32(50_000_000)
		to     = uint32(50_000_099) // 100 ledgers requested
		bucket = "galexie-live"
	)

	t.Run("full walk passes", func(t *testing.T) {
		t.Parallel()
		if err := walkCoverageErr("ch-gate", from, to, 100, bucket); err != nil {
			t.Fatalf("walkCoverageErr over a complete walk = %v, want nil", err)
		}
	})

	t.Run("single ledger range", func(t *testing.T) {
		t.Parallel()
		if err := walkCoverageErr("ch-gate", from, from, 1, bucket); err != nil {
			t.Fatalf("walkCoverageErr(from==to, walked 1) = %v, want nil", err)
		}
		if err := walkCoverageErr("ch-gate", from, from, 0, bucket); err == nil {
			t.Fatal("walkCoverageErr(from==to, walked 0) = nil, want an error")
		}
	})

	t.Run("short walk fails and names both numbers", func(t *testing.T) {
		t.Parallel()
		err := walkCoverageErr("ch-gate", from, to, 37, bucket)
		if err == nil {
			t.Fatal("walkCoverageErr over a 37-of-100 walk = nil; a partially examined range " +
				"must not be reported on as if it were complete")
		}
		msg := err.Error()
		for _, want := range []string{"ch-gate", "37", "100", "galexie-live", "galexie-archive"} {
			if !strings.Contains(msg, want) {
				t.Errorf("short-walk error %q does not mention %q — the operator needs the "+
					"delivered/requested counts and the bucket that was actually read", msg, want)
			}
		}
	})

	t.Run("zero walked keeps its own diagnosis", func(t *testing.T) {
		t.Parallel()
		err := walkCoverageErr("ch-gate", from, to, 0, bucket)
		if err == nil {
			t.Fatal("walkCoverageErr over a zero-ledger walk = nil, want the vacuous-pass refusal")
		}
		if !strings.Contains(err.Error(), "walked 0 ledgers") {
			t.Errorf("zero-walk error = %q, want the distinct 'walked 0 ledgers' diagnosis — "+
				"nothing-was-examined is the shape operators misread as clean", err.Error())
		}
	})

	t.Run("command name is carried through", func(t *testing.T) {
		t.Parallel()
		err := walkCoverageErr("sdex-claim-audit", from, to, 5, bucket)
		if err == nil || !strings.Contains(err.Error(), "sdex-claim-audit") {
			t.Fatalf("err = %v, want it prefixed with the calling subcommand", err)
		}
	})

	// The old gate, stated exactly: ch-gate compared the ClickHouse row
	// count to `walked`. This asserts that rule ACCEPTS the short walk
	// the new rule rejects — i.e. that the two are not equivalent and
	// the fix is load-bearing, not decorative. A backfill that wrote 37
	// ledgers and a walk that read 37 of the 100 requested agreed
	// perfectly, and the gate printed PASSED.
	t.Run("the rule it replaces accepts the short walk", func(t *testing.T) {
		t.Parallel()
		const (
			walked      = 37
			chLedgerRow = uint64(37)
		)
		oldRuleFails := chLedgerRow != uint64(walked)
		if oldRuleFails {
			t.Fatal("fixture is wrong: the pre-fix rule (CH rows == walked) should agree here")
		}
		if err := walkCoverageErr("ch-gate", from, to, walked, bucket); err == nil {
			t.Fatal("the new rule accepts a walk the old one already accepted — " +
				"nothing was fixed")
		}
	})
}
