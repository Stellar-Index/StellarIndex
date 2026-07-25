package ingest

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// testCensusConfig is the r1-shaped storage block: both buckets named, seam
// unset (r1's live value — see configs/ansible/inventory/r1.yml).
func testCensusConfig(seam uint32) config.Config {
	var cfg config.Config
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"
	cfg.Ingestion.LiveSeamLedger = seam
	return cfg
}

// TestCensusBucket_SeamAware — census-backfill carried the SAME unconditional
// `streamBucket := cfg.Storage.S3BucketLive` default that ch-backfill was
// fixed for at 5179250a. With a seam configured, the requested range must
// decide the bucket, and a straddling range must be a hard error rather than
// a coin flip (one walk reads exactly one bucket).
func TestCensusBucket_SeamAware(t *testing.T) {
	const seam = uint32(60_000_000)
	cfg := testCensusConfig(seam)

	// The historic range census-backfill exists to serve: pre-live ledgers
	// that only the archive bucket holds.
	got, err := censusBucket(cfg, "", 2, 1_000_000)
	if err != nil {
		t.Fatalf("below the seam errored: %v", err)
	}
	if got != "galexie-archive" {
		t.Fatalf("census-backfill resolved a historic range to %q, want %q — the live "+
			"bucket cannot hold pre-live ledgers, so the walk streams nothing", got, "galexie-archive")
	}
	if got, err := censusBucket(cfg, "", seam, seam+1000); err != nil || got != "galexie-live" {
		t.Errorf("at/above the seam = (%q, %v), want (galexie-live, nil)", got, err)
	}
	straddle, err := censusBucket(cfg, "", seam-1, seam+1)
	if err == nil {
		t.Fatalf("a range straddling the seam resolved to %q instead of erroring — "+
			"one of the two sides would be silently skipped", straddle)
	}
	if !strings.Contains(err.Error(), "straddle") || !strings.Contains(err.Error(), "60000000") {
		t.Errorf("straddle error must name the seam, got: %v", err)
	}
}

// TestCensusBucket_NoSeamKeepsTheLiveDefault pins the DELIBERATE limit of the
// shared resolver on r1 (no seam configured): the default stays live, and it
// is censusCoverage — not a guessed default — that makes a wrong bucket
// unmissable. Mirrors chops.TestBackfillBucket_NoSeamKeepsTheLiveDefault.
func TestCensusBucket_NoSeamKeepsTheLiveDefault(t *testing.T) {
	cfg := testCensusConfig(0)

	got, err := censusBucket(cfg, "", 2, 1_000_000)
	if err != nil {
		t.Fatalf("no-seam resolution errored: %v", err)
	}
	if got != cfg.Storage.S3BucketLive {
		t.Fatalf("resolved = %q, want %q — changing the no-seam default is what "+
			"breaks ch-live-catchup.sh", got, cfg.Storage.S3BucketLive)
	}
	// ...and the historic range that lands there must not be able to pass.
	if censusCoverage(2, 1_000_000, 0, 0, got, false) == nil {
		t.Fatal("a historic range resolved to the live bucket persisted zero ledgers and still " +
			"reported success — the wrong-bucket defect is not closed")
	}
}

// TestCensusBucket_ExplicitOverrideAlwaysWins — docs/operations/
// adr-0033-data-recovery.md drives census-backfill with an explicit -bucket;
// resolution must never second-guess it.
func TestCensusBucket_ExplicitOverrideAlwaysWins(t *testing.T) {
	cfg := testCensusConfig(60_000_000)
	for _, tc := range []struct{ from, to uint32 }{
		{2, 1_000_000},           // below the seam
		{61_000_000, 62_000_000}, // above it
		{59_000_000, 61_000_000}, // straddling it
	} {
		got, err := censusBucket(cfg, "some-other-bucket", tc.from, tc.to)
		if err != nil {
			t.Fatalf("[%d,%d]: override errored: %v", tc.from, tc.to, err)
		}
		if got != "some-other-bucket" {
			t.Errorf("[%d,%d]: override = %q, want %q", tc.from, tc.to, got, "some-other-bucket")
		}
	}
}

// TestCensusCoverage_ZeroPersistedIsAHardError — the live-observed shape:
// census-backfill against a historic range read galexie-live, streamed
// nothing, printed "done — 0 ledgers processed" and exited 0. ledger_ingest_log
// is the substrate the ADR-0033 projection reconcile measures completeness
// against, so a range recorded as backfilled but never written becomes a
// completeness lie later.
func TestCensusCoverage_ZeroPersistedIsAHardError(t *testing.T) {
	err := censusCoverage(2, 1_000_000, 0, 0, "galexie-live", false)
	if err == nil {
		t.Fatal("censusCoverage(persisted=0) returned nil — census-backfill would exit 0 having " +
			"written no substrate rows for [2,1000000]")
	}
	for _, want := range []string{"ZERO", "1000000", "galexie-live"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the operator sees which bucket was read; got: %v", want, err)
		}
	}
}

// TestCensusCoverage_PartialRunIsAHardError — a run whose ledgers were
// partly absent / skipped / failed to upsert left a substrate hole and still
// exited 0. The C2-14 watermark froze the resume cursor correctly, but
// nothing told the caller.
func TestCensusCoverage_PartialRunIsAHardError(t *testing.T) {
	err := censusCoverage(1_000_000, 1_000_999, 900, 40, "galexie-archive", false)
	if err == nil {
		t.Fatal("censusCoverage(persisted=900, want=1000) returned nil — a 100-ledger " +
			"substrate hole would be reported as a completed range")
	}
	if !strings.Contains(err.Error(), "900 of 1000") || !strings.Contains(err.Error(), "100 ledgers are missing") {
		t.Errorf("error must state the shortfall exactly, got: %v", err)
	}
	if !strings.Contains(err.Error(), "40 skipped") {
		t.Errorf("error must carry the skip count so the operator can tell absent objects "+
			"from G15-06 tx-read-error skips, got: %v", err)
	}
}

// TestCensusCoverage_InterruptedRunIsNotADoneRange — a SIGINT'd walk unwinds
// through `errors.Is(walkErr, context.Canceled)` and used to return nil.
func TestCensusCoverage_InterruptedRunIsNotADoneRange(t *testing.T) {
	err := censusCoverage(1_000_000, 1_000_999, 1000, 0, "galexie-archive", true)
	if err == nil {
		t.Fatal("an interrupted run reported success — Ctrl-C would look like a completed range")
	}
	if !strings.Contains(err.Error(), "INTERRUPTED") {
		t.Errorf("error must say it was interrupted, got: %v", err)
	}
}

// TestCensusCoverage_CompleteRunPasses — the fix must not turn a genuinely
// complete run (including a RESUMED one, which starts above -from) into a
// failure.
func TestCensusCoverage_CompleteRunPasses(t *testing.T) {
	if err := censusCoverage(1_000_000, 1_000_999, 1000, 0, "galexie-archive", false); err != nil {
		t.Fatalf("a complete run must pass, got: %v", err)
	}
	if err := censusCoverage(5, 5, 1, 0, "galexie-archive", false); err != nil {
		t.Fatalf("a single-ledger run must pass, got: %v", err)
	}
	// Resumed run: coverage is charged from the ledger the run actually
	// started at, not the original -from.
	if err := censusCoverage(1_000_500, 1_000_999, 500, 0, "galexie-archive", false); err != nil {
		t.Fatalf("a complete RESUMED run must pass, got: %v", err)
	}
}

// TestContiguousWatermark_FreezesOnGap is the C2-14 proof for the
// census-backfill resume checkpoint: a mid-range skipped/failed ledger must
// NOT let the checkpoint stride past it. The pre-fix code checkpointed the
// last WRITTEN ledger (`lastProcessed`), which advanced straight over a gap
// — leaving a permanent substrate hole on resume. The watermark instead
// freezes at the last contiguous ledger before the first gap.
func TestContiguousWatermark_FreezesOnGap(t *testing.T) {
	t.Run("no gaps advances to the last ledger", func(t *testing.T) {
		var wm contiguousWatermark
		for seq := uint32(100); seq <= 105; seq++ {
			wm.persisted(seq)
		}
		if wm.seq != 105 {
			t.Fatalf("watermark = %d, want 105 (all persisted, no gap)", wm.seq)
		}
	})

	t.Run("mid-range gap freezes the checkpoint before it", func(t *testing.T) {
		// Ledgers 100,101 persist; 102 is skipped (read error); 103,104
		// persist. The durable checkpoint MUST be 101 (last ledger with no
		// preceding gap), so a resume re-reads from 102 (the gap) — NOT 104,
		// which would strand ledger 102 forever.
		var wm contiguousWatermark
		wm.persisted(100)
		wm.persisted(101)
		wm.gap() // ledger 102 skipped / failed
		wm.persisted(103)
		wm.persisted(104)

		if wm.seq != 101 {
			t.Fatalf("watermark = %d, want 101 — checkpoint strode past the gap at 102 (C2-14 regression)", wm.seq)
		}
		// Resume start = checkpoint + 1 must land on the gap, not beyond it.
		if resume := wm.seq + 1; resume != 102 {
			t.Fatalf("resume start = %d, want 102 (re-read the gap)", resume)
		}
	})

	t.Run("gap on the first ledger writes no checkpoint", func(t *testing.T) {
		// If the very first ledger of the run is un-persisted, seq stays 0 so
		// the caller writes no checkpoint and the existing cursor is untouched
		// — resume restarts from the same place and re-reads the gap.
		var wm contiguousWatermark
		wm.gap() // first ledger skipped
		wm.persisted(201)
		wm.persisted(202)
		if wm.seq != 0 {
			t.Fatalf("watermark = %d, want 0 (first ledger was a gap)", wm.seq)
		}
	})

	t.Run("second gap after freeze does not un-freeze", func(t *testing.T) {
		var wm contiguousWatermark
		wm.persisted(300)
		wm.gap()
		wm.persisted(302)
		wm.gap()
		wm.persisted(304)
		if wm.seq != 300 {
			t.Fatalf("watermark = %d, want 300 (frozen at first gap)", wm.seq)
		}
	})
}
