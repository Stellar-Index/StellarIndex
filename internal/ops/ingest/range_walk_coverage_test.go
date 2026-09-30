package ingest

import (
	"os"
	"strings"
	"testing"
)

// A walk the trailing-missing tolerance ended early returns nil from
// ledgerstream.Stream; only the delivered count can tell it from a full one.
func TestRangeWalkCoverage(t *testing.T) {
	t.Parallel()
	const bucket = "galexie-archive"

	complete := []struct {
		name     string
		from, to uint32
		walked   int
	}{
		{"full range", 1_000_000, 1_000_999, 1000},
		{"single ledger", 5, 5, 1},
		{"top of the uint32 range", 4_294_967_000, 4_294_967_295, 296},
	}
	for _, tc := range complete {
		if err := rangeWalkCoverage("backfill-router", tc.from, tc.to, tc.walked, bucket); err != nil {
			t.Errorf("%s: complete walk refused: %v", tc.name, err)
		}
	}

	short := rangeWalkCoverage("backfill-router", 51_000_000, 51_599_999, 352_111, bucket)
	if short == nil {
		t.Fatal("rangeWalkCoverage over a 352111-of-600000 walk = nil; a partial walk would exit 0")
	}
	for _, want := range []string{"backfill-router", "352111 of 600000", "247889 trailing ledgers were NOT walked", `"galexie-archive"`, "[51000000,51599999]"} {
		if !strings.Contains(short.Error(), want) {
			t.Errorf("short-walk error is missing %q: %v", want, short)
		}
	}
	if rangeWalkCoverage("scan-soroban-events", 100, 199, 99, bucket) == nil {
		t.Error("a walk one ledger short of the range returned nil")
	}

	if over := rangeWalkCoverage("backfill-router", 100, 199, 101, bucket); over == nil || !strings.Contains(over.Error(), "holds only 100") {
		t.Errorf("a walk that over-delivered: want a refusal, got: %v", over)
	}

	none := rangeWalkCoverage("backfill-router", 2, 1_000_000, 0, "galexie-live")
	if none == nil || !strings.Contains(none.Error(), "walked 0 of 999999") || !strings.Contains(none.Error(), `"galexie-live"`) {
		t.Errorf("zero-ledger walk: want a refusal naming the count and bucket, got: %v", none)
	}
}

// Both commands need S3 (and backfill-router Postgres), so pin the
// coverage call at the source.
func TestRangeWalkCoverageCallers(t *testing.T) {
	t.Parallel()
	for file, call := range map[string]string{
		"backfill_router.go":     `rangeWalkCoverage("backfill-router", startLedger, uint32(*to), totalLedgers, streamBucket)`,
		"scan_soroban_events.go": `rangeWalkCoverage("scan-soroban-events", uint32(*from), uint32(*to), totalLedgers, bucket)`,
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), call) {
			t.Errorf("%s no longer calls %s — a short walk exits 0 as if the range were complete", file, call)
		}
	}
	src, err := os.ReadFile("scan_soroban_events.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "if col.matched < col.limit {") {
		t.Error("scan-soroban-events must skip the coverage check when -limit stopped the walk on purpose")
	}
}
