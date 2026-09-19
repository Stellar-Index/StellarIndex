package diagnostics

import (
	"os"
	"strings"
	"testing"
)

// TestVerifyWalkCoverage is the RLT-282 regression guard for
// verify-decoders. REQUESTED is -to minus -from plus one; DELIVERED is
// totalLedgers, incremented once per LedgerCloseMeta handed to the
// dispatcher.
//
// The claim this command exists to make is "decoder X fired / did not
// fire over [from,to]". That claim was computed over whatever the walk
// happened to deliver, and two defaults made short walks routine: the
// bucket was hardcoded to the TRIMMED live bucket with no override
// flag, and opsutil.NewBoundedLedgerStreamConfig always sets
// TolerateTrailingMissing, so a hole within 65,536 ledgers of -to
// returns a clean walk-complete. A wrong-bucket run therefore printed
// every decoder as silent and exited 0 — which reads as "every topic
// or schema has drifted", the loudest possible false positive from a
// tool whose whole job is to detect exactly that.
func TestVerifyWalkCoverage(t *testing.T) {
	t.Parallel()

	const (
		from   = uint32(48_000_000)
		to     = uint32(48_000_999) // 1000 ledgers requested
		bucket = "galexie-live"
	)

	if err := verifyWalkCoverage(from, to, 1000, bucket); err != nil {
		t.Errorf("complete walk = %v, want nil", err)
	}
	if err := verifyWalkCoverage(from, from, 1, bucket); err != nil {
		t.Errorf("single-ledger complete walk = %v, want nil", err)
	}

	short := verifyWalkCoverage(from, to, 412, bucket)
	if short == nil {
		t.Fatal("412-of-1000 walk = nil; the per-source table describes only that subset, " +
			"so a decoder reported silent may just be absent from the part that was read")
	}
	for _, want := range []string{"412", "1000", "galexie-live", "galexie-archive"} {
		if !strings.Contains(short.Error(), want) {
			t.Errorf("short-walk error %q does not mention %q", short.Error(), want)
		}
	}

	none := verifyWalkCoverage(from, to, 0, bucket)
	if none == nil {
		t.Fatal("zero-ledger walk = nil; nothing was examined and every decoder is reported silent")
	}
	if !strings.Contains(none.Error(), "processed 0 ledgers") {
		t.Errorf("zero-walk error = %q, want its own diagnosis", none.Error())
	}
}

// TestVerifyDecoders_ResolvesBucketAndGatesCoverage pins the call site.
// verifyDecoders needs a config file, an S3-backed galexie and (for the
// Soroswap seed) an RPC endpoint before it runs a line, so the wiring is
// pinned at the source — the same way
// ingest.TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion
// pins its sibling rule.
func TestVerifyDecoders_ResolvesBucketAndGatesCoverage(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("verify_decoders.go")
	if err != nil {
		t.Fatalf("read verify_decoders.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "\nfunc verifyDecoders(")
	if start < 0 {
		t.Fatal("verifyDecoders not found — this test is asserting nothing")
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, `fs.String("bucket"`) {
		t.Error("verify-decoders has no -bucket flag, so a historic range can only be read " +
			"from whatever the config's live bucket happens to be")
	}
	if !strings.Contains(body, "opsutil.ResolveStreamBucket(cfg, *bucket, uint32(*from), uint32(*to))") {
		t.Error("the galexie bucket must come from opsutil.ResolveStreamBucket, not a hardcoded field")
	}
	if strings.Contains(body, "NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketLive") {
		t.Error("verifyDecoders still streams from the hardcoded TRIMMED live bucket (RLT-282)")
	}
	if !strings.Contains(body, "return verifyWalkCoverage(uint32(*from), uint32(*to), totalLedgers, streamBucket)") {
		t.Error("verifyDecoders never gates on coverage — a short walk exits 0 with every " +
			"decoder reported silent")
	}
}
