package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Regression tests for issue #282 — the P1
// `stellarindex_stellar_archive_divergence` page had no producer on
// r1 because `stellarindex_verify_archive_mismatches_total` only
// ever left the process through the opt-in `-metrics-listen` HTTP
// endpoint. These pin the textfile export path AND the three
// properties that make the exported counter fire `increase()`:
// zero-seeding, cross-run accumulation, and per-tier isolation.

// TestRenderVerifyArchiveTextfile_zeroSeedsEveryReason is the
// anti-"absence reads as health" pin. A counter series that first
// APPEARS at 1 and stays flat has no earlier sample to subtract, so
// `increase(...) > 0` never fires on the first divergence. Every
// reason must therefore be present at 0 on a clean run.
func TestRenderVerifyArchiveTextfile_zeroSeedsEveryReason(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := renderVerifyArchiveTextfile(&sb, "chain", map[string]uint64{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := sb.String()
	for _, want := range []string{
		"# TYPE stellarindex_verify_archive_mismatches_total counter",
		`stellarindex_verify_archive_mismatches_total{tier="chain",reason="chain"} 0`,
		`stellarindex_verify_archive_mismatches_total{tier="chain",reason="checkpoint"} 0`,
		`stellarindex_verify_archive_mismatches_total{tier="chain",reason="sequence"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in textfile:\n%s", want, got)
		}
	}
}

// TestWriteVerifyArchiveTextfile_accumulatesAcrossRuns pins the
// monotonic-counter contract. The file is rewritten wholesale each
// run, so without folding the previous total back in, a clean run
// would reset the counter to 0 and `increase()` over the alert's
// window would go NEGATIVE-then-reset — i.e. the page would clear
// itself the night after a divergence.
func TestWriteVerifyArchiveTextfile_accumulatesAcrossRuns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "verify_archive_tier_a.prom")

	// Run 1: one chain break.
	if err := writeVerifyArchiveTextfile(path, "chain", map[string]uint64{"chain": 1}); err != nil {
		t.Fatalf("run 1 write: %v", err)
	}
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="chain",reason="chain"}`, "1")

	// Run 2: clean. The total must HOLD, not reset.
	if err := writeVerifyArchiveTextfile(path, "chain", map[string]uint64{}); err != nil {
		t.Fatalf("run 2 write: %v", err)
	}
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="chain",reason="chain"}`, "1")

	// Run 3: two more breaks + a sequence gap. Totals advance.
	if err := writeVerifyArchiveTextfile(path, "chain", map[string]uint64{"chain": 2, "sequence": 1}); err != nil {
		t.Fatalf("run 3 write: %v", err)
	}
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="chain",reason="chain"}`, "3")
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="chain",reason="sequence"}`, "1")
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="chain",reason="checkpoint"}`, "0")
}

// TestWriteVerifyArchiveTextfile_tierScopedCarryForward proves the
// tier-a and tier-b units cannot transplant each other's totals. A
// repointed `-tier` starts a fresh zero baseline instead of
// inheriting a foreign total (which would read as a counter jump and
// false-page a P1).
func TestWriteVerifyArchiveTextfile_tierScopedCarryForward(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "verify_archive.prom")

	if err := writeVerifyArchiveTextfile(path, "chain", map[string]uint64{"chain": 4}); err != nil {
		t.Fatalf("chain write: %v", err)
	}
	if err := writeVerifyArchiveTextfile(path, "checkpoint", map[string]uint64{}); err != nil {
		t.Fatalf("checkpoint write: %v", err)
	}
	assertSample(t, path, `stellarindex_verify_archive_mismatches_total{tier="checkpoint",reason="chain"}`, "0")
	if body := readFile(t, path); strings.Contains(body, `tier="chain"`) {
		t.Errorf("tier=chain samples survived a tier=checkpoint rewrite:\n%s", body)
	}
}

// TestWriteVerifyArchiveTextfile_atomicRenameLeavesNoTmp pins the
// node_exporter contract: the collector parses every file in its
// directory that does not end in `.tmp`, and treats a partial write
// as a scrape error for the WHOLE directory.
func TestWriteVerifyArchiveTextfile_atomicRenameLeavesNoTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "verify_archive_tier_a.prom")
	if err := writeVerifyArchiveTextfile(path, "chain", map[string]uint64{"checkpoint": 1}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("stat %s.tmp = %v, want not-exist (rename must clean up)", path, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Errorf("dir contents = %v, want only %s", entries, filepath.Base(path))
	}
}

// TestReadPriorVerifyArchiveTextfile_degradesToEmpty covers the
// best-effort contract: a missing or garbage previous file must not
// fail the run. To Prometheus that is an ordinary counter reset; an
// ops binary that refused to publish because it could not parse its
// own last output would recreate the #282 blind spot.
func TestReadPriorVerifyArchiveTextfile_degradesToEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if got := readPriorVerifyArchiveTextfile(filepath.Join(dir, "nope.prom"), "chain"); len(got) != 0 {
		t.Errorf("missing file → %v, want empty", got)
	}

	garbage := filepath.Join(dir, "garbage.prom")
	if err := os.WriteFile(garbage, []byte("this is not\nexposition {format\n"), 0o600); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	if got := readPriorVerifyArchiveTextfile(garbage, "chain"); len(got) != 0 {
		t.Errorf("garbage file → %v, want empty", got)
	}
}

// TestCollectVerifyArchiveMismatches_sumsOverChunkIdx proves the
// readback aggregates the per-chunk in-process counter down to the
// per-reason totals the textfile carries — chunk_idx is a per-run
// worker index with no cross-run meaning, so a mismatch on a
// never-before-seen chunk must not create a brand-new (and therefore
// unfireable) series.
//
// Not t.Parallel: obs.VerifyArchiveMismatchesTotal is process-global.
func TestCollectVerifyArchiveMismatches_sumsOverChunkIdx(t *testing.T) {
	obs.VerifyArchiveMismatchesTotal.Reset()
	t.Cleanup(obs.VerifyArchiveMismatchesTotal.Reset)

	obs.VerifyArchiveMismatchesTotal.WithLabelValues("3", "chain").Inc()
	obs.VerifyArchiveMismatchesTotal.WithLabelValues("11", "chain").Inc()
	obs.VerifyArchiveMismatchesTotal.WithLabelValues("11", "checkpoint").Add(2)

	got := collectVerifyArchiveMismatches()
	want := map[string]uint64{"chain": 2, "checkpoint": 2}
	if len(got) != len(want) {
		t.Fatalf("totals = %v, want %v", got, want)
	}
	for reason, n := range want {
		if got[reason] != n {
			t.Errorf("totals[%q] = %d, want %d", reason, got[reason], n)
		}
	}
}

func assertSample(t *testing.T, path, series, want string) {
	t.Helper()
	body := readFile(t, path)
	line := series + " " + want
	if !strings.Contains(body, line) {
		t.Errorf("missing %q in %s:\n%s", line, path, body)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-owned t.TempDir path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
