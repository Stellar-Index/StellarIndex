package archive

import (
	"os"
	"strings"
	"testing"
)

// TestWasmWalkCoverage pins the RLT-282 coverage rule for wasm-history.
// REQUESTED is -to minus -from plus one; DELIVERED is totalScanned, the
// sum of the per-worker ledger counts. An unbounded walk (-to 0, the
// live tail) has no requested count and is exempt.
//
// This output is copied into docs/operations/wasm-audits/* and is what a
// BackfillSafe determination rests on, so a walk that covered less than
// the operator asked for cannot be allowed to exit 0.
func TestWasmWalkCoverage(t *testing.T) {
	t.Parallel()

	const (
		from   = uint32(51_000_000)
		to     = uint32(51_599_999) // 600,000 ledgers requested
		bucket = "galexie-live"
	)

	if err := wasmWalkCoverage(from, to, 600_000, bucket); err != nil {
		t.Errorf("complete walk = %v, want nil", err)
	}
	if err := wasmWalkCoverage(from, 0, 12, bucket); err != nil {
		t.Errorf("unbounded walk (-to 0) = %v, want nil — there is no requested count to assert", err)
	}
	if err := wasmWalkCoverage(from, from, 1, bucket); err != nil {
		t.Errorf("single-ledger complete walk = %v, want nil", err)
	}

	// The issue's own worked example: a lost partition in chunk 7 stops
	// the walk at 51,352,110 and TolerateTrailingMissing returns nil.
	short := wasmWalkCoverage(from, to, 352_111, bucket)
	if short == nil {
		t.Fatal("a 352,111-of-600,000 walk = nil; the audit covers less than the range named " +
			"and a WASM upgrade in the unread tail is simply absent from the published epochs")
	}
	for _, want := range []string{"352111", "600000", "galexie-live", "galexie-archive"} {
		if !strings.Contains(short.Error(), want) {
			t.Errorf("short-walk error %q does not mention %q", short.Error(), want)
		}
	}

	none := wasmWalkCoverage(from, to, 0, bucket)
	if none == nil {
		t.Fatal("a zero-ledger walk = nil; every watched contract is then reported as having " +
			"no transitions, which reads as a clean audit")
	}
	if !strings.Contains(none.Error(), "scanned 0 ledgers") {
		t.Errorf("zero-walk error = %q, want its own diagnosis", none.Error())
	}
}

// TestWasmHistory_GatesCoverageAfterEmittingJSON pins the call site.
// wasmHistory needs a config file and an S3-backed galexie before it
// runs a line, so the wiring is pinned at the source — the same way
// ingest.TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion
// pins its sibling rule.
//
// Ordering is the point: the JSON must still be written (its ranges are
// honest about what was observed, and a partial audit is worth having)
// and the coverage error must come after it, so the operator gets both
// the artifact and a non-zero exit.
func TestWasmHistory_GatesCoverageAfterEmittingJSON(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("wasm_history.go")
	if err != nil {
		t.Fatalf("read wasm_history.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "\nfunc wasmHistory(")
	if start < 0 {
		t.Fatal("wasmHistory not found — this test is asserting nothing")
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	encode := strings.Index(body, "enc.Encode(out)")
	coverage := strings.Index(body, "return wasmWalkCoverage(uint32(*from), uint32(*to), totalScanned, bucketName)")
	switch {
	case encode < 0:
		t.Fatal("JSON encode anchor not found — this test is asserting nothing")
	case coverage < 0:
		t.Fatal("wasmHistory never gates on coverage — a short walk publishes a WASM-epoch " +
			"audit and exits 0 (RLT-282)")
	case coverage < encode:
		t.Error("the coverage guard runs BEFORE the JSON is written — a partial audit is worth " +
			"having, so emit it and then fail")
	}
}

// TestRunOneWasmHistoryWorker_UpperEndComesFromTheWalk guards the other
// half at the source: upperEnd must be assigned from the ledger the walk
// callback receives, never seeded from the requested chunk bound. The
// executing proof is
// TestWasmHistoryWorker_ShortWalk_UpperEndIsLastObserved (build tag
// integration); this one runs in the default suite so the regression
// cannot slip in between integration runs.
func TestRunOneWasmHistoryWorker_UpperEndComesFromTheWalk(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("wasm_history.go")
	if err != nil {
		t.Fatalf("read wasm_history.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "\nfunc runOneWasmHistoryWorker(")
	if start < 0 {
		t.Fatal("runOneWasmHistoryWorker not found — this test is asserting nothing")
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	if strings.Contains(body, "result.upperEnd = b.To") {
		t.Error("upperEnd is seeded from the REQUESTED chunk bound before the walk; " +
			"mergeWasmHistories closes every open WASM range at it, so a short walk publishes " +
			"a range the worker never opened (RLT-282)")
	}
	if !strings.Contains(body, "result.upperEnd = seq") {
		t.Error("upperEnd is never assigned from the ledger the walk callback received — it is " +
			"documented as the last ledger the worker actually saw")
	}
}
