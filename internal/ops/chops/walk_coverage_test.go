package chops

import (
	"os"
	"strings"
	"testing"
)

// TestWalkCoverage is the RLT-282 regression guard for the rule every
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
		err := walkCoverage("sdex-claim-audit", from, to, 5, bucket)
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
		if err := walkCoverage("ch-gate", from, to, walked, bucket); err == nil {
			t.Fatal("the new rule accepts a walk the old one already accepted — " +
				"nothing was fixed")
		}
	})
}

// funcBody returns the source text of the named top-level function in
// the given file of this package. The call-site wiring below is pinned
// at the source because chGate and sdexClaimAudit each need a config
// file, a galexie bucket over S3 and (for the gate) a live ClickHouse
// before they will run a single line — the same reason
// ingest.TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion
// pins its sibling rule this way.
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

// TestChGate_GatesOnRequestedCoverage pins the ch-gate call site of the
// RLT-282 rule. REQUESTED there is -to minus -from plus one; DELIVERED
// is `walked`, incremented once per LedgerCloseMeta the census walk
// hands back. The guard has to run before the PASSED banner, and the
// ClickHouse row count has to be measured against the requested span —
// measuring it against `walked` is what let a short walk certify itself.
func TestChGate_GatesOnRequestedCoverage(t *testing.T) {
	t.Parallel()
	body := funcBody(t, "ch_gate.go", "chGate")

	coverage := strings.Index(body, `walkCoverage("ch-gate", uint32(*from), uint32(*to), walked, streamBucket)`)
	passed := strings.Index(body, "completeness gate PASSED")
	switch {
	case coverage < 0:
		t.Fatal("chGate never calls walkCoverage — a short walk still reports a verdict over " +
			"the slice it happened to read (RLT-282)")
	case passed < 0:
		t.Fatal("PASSED banner not found — this test is asserting nothing")
	case coverage > passed:
		t.Error("the coverage guard runs AFTER the PASSED banner — the gate announces success " +
			"on a range it only partly opened")
	}
	if strings.Contains(body, "ch.LedgerRows != uint64(walked)") {
		t.Error("ledger coverage is still measured against `walked`; both sides of that " +
			"comparison shrink together on a short walk, so the gate compares a subset to itself")
	}
	if !strings.Contains(body, "ch.LedgerRows != requested") {
		t.Error("ledger coverage must be measured against the REQUESTED span")
	}
}

// TestSdexClaimAudit_GatesOnRequestedCoverage pins the sdex-claim-audit
// call site. REQUESTED is -to minus -from plus one; DELIVERED is
// `walked`, now incremented once per ledger inside the stream callback
// (the command previously counted no ledgers at all). Coverage matters
// more here than almost anywhere: the tool's output exists to be
// differenced against an EXTERNAL anchor's trade count for the same
// range, so ledgers the walk never read become a phantom decoder gap of
// exactly that size.
//
// It also pins the bucket default. Resolving through
// opsutil.ResolveStreamBucket puts the seam policy in one place; the old
// local default sent every historic audit at the trimmed live bucket.
func TestSdexClaimAudit_GatesOnRequestedCoverage(t *testing.T) {
	t.Parallel()
	body := funcBody(t, "sdex_claim_audit.go", "sdexClaimAudit")

	if !strings.Contains(body, "walked++") {
		t.Error("sdexClaimAudit does not count the ledgers it walked — it cannot assert " +
			"delivered == requested without the delivered half")
	}
	if !strings.Contains(body, `return walkCoverage("sdex-claim-audit", uint32(*from), uint32(*to), walked, streamBucket)`) {
		t.Error("sdexClaimAudit never returns walkCoverage — a short walk exits 0 and its " +
			"claim-atom tally reads as a decoder gap (RLT-282)")
	}
	if !strings.Contains(body, "opsutil.ResolveStreamBucket(cfg, *bucket, uint32(*from), uint32(*to))") {
		t.Error("the galexie bucket must come from opsutil.ResolveStreamBucket, not a local default")
	}
	if strings.Contains(body, ":= cfg.Storage.S3BucketLive") {
		t.Error("sdexClaimAudit still defaults to the TRIMMED live bucket, which cannot hold " +
			"a historic range (the flag help text may name it; the code must not pick it)")
	}
}
