package diagnostics

import (
	"os"
	"strings"
	"testing"
)

// TestVerifyWalkCoverage guards delivered-equals-requested for verify-decoders: REQUESTED is -to minus -from plus one, DELIVERED is totalLedgers.
// A wrong-bucket or short walk (TolerateTrailingMissing hides holes near -to) would print every decoder silent and exit 0, a false "all drifted".
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

// TestVerifyDecoders_ResolvesBucketAndGatesCoverage pins the call site at the source (verifyDecoders needs a config, S3-backed galexie and an RPC endpoint),
// like ingest.TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion.
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
	if !strings.Contains(body, "if err := verifyWalkCoverage(uint32(*from), uint32(*to), totalLedgers, streamBucket); err != nil {") {
		t.Error("verifyDecoders never gates on coverage — a short walk exits 0 with every " +
			"decoder reported silent")
	}
}
