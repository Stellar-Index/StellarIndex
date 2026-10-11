package chops

import (
	"os"
	"strings"
	"testing"
)

// TestWalkCoverage guards the rule every galexie-walking subcommand in this package applies
// before it reports on what it saw: DELIVERED must equal REQUESTED.
//
// Every other ch-gate check measured the walk against itself. `ch.LedgerRows != uint64(walked)`
// compares the rows ClickHouse holds for the range to the ledgers the walk produced, so when a
// wrong bucket or a hole tolerated by TolerateTrailingMissing shortens the walk, both sides shrink
// together and the gate certifies the slice it happened to read.
func TestWalkCoverage(t *testing.T) {
	t.Parallel()

	const (
		from   = uint32(50_000_000)
		to     = uint32(50_000_099) // 100 ledgers requested
		bucket = "galexie-live"
	)

	t.Run("full walk passes", func(t *testing.T) {
		t.Parallel()
		if err := walkCoverage("ch-gate", from, to, 100, bucket); err != nil {
			t.Fatalf("walkCoverage over a complete walk = %v, want nil", err)
		}
	})

	t.Run("single ledger range", func(t *testing.T) {
		t.Parallel()
		if err := walkCoverage("ch-gate", from, from, 1, bucket); err != nil {
			t.Fatalf("walkCoverage(from==to, walked 1) = %v, want nil", err)
		}
		if err := walkCoverage("ch-gate", from, from, 0, bucket); err == nil {
			t.Fatal("walkCoverage(from==to, walked 0) = nil, want an error")
		}
	})

	t.Run("short walk fails and names both numbers", func(t *testing.T) {
		t.Parallel()
		err := walkCoverage("ch-gate", from, to, 37, bucket)
		if err == nil {
			t.Fatal("walkCoverage over a 37-of-100 walk = nil; a partially examined range " +
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
		err := walkCoverage("ch-gate", from, to, 0, bucket)
		if err == nil {
			t.Fatal("walkCoverage over a zero-ledger walk = nil, want the vacuous-pass refusal")
		}
		if !strings.Contains(err.Error(), "walked 0 ledgers") {
			t.Errorf("zero-walk error = %q, want the distinct 'walked 0 ledgers' diagnosis — "+
				"nothing-was-examined is the shape operators misread as clean", err.Error())
		}
	})

	t.Run("command name is carried through", func(t *testing.T) {
		t.Parallel()
		err := walkCoverage("other-audit", from, to, 5, bucket)
		if err == nil || !strings.Contains(err.Error(), "other-audit") {
			t.Fatalf("err = %v, want it prefixed with the calling subcommand", err)
		}
	})

	// The old gate compared the ClickHouse row count to `walked`. This asserts that rule accepts
	// the short walk the new rule rejects, so the two are not equivalent and the fix is
	// load-bearing.
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
		if err := walkCoverage("ch-gate", from, to, walked, bucket); err == nil {
			t.Fatal("the new rule accepts a walk the old one already accepted — " +
				"nothing was fixed")
		}
	})
}

// funcBody returns the source text of the named top-level function in the given file of this
// package. The call-site wiring below is pinned at the source because chGate needs a config file,
// a galexie bucket over S3 and a live ClickHouse before it runs a single line, as with
// ingest.TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(b)
	start := strings.Index(src, "\nfunc "+fn+"(")
	if start < 0 {
		t.Fatalf("%s not found in %s — this test is asserting nothing", fn, file)
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	return body
}
