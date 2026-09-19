package controlwiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── F050: every re-derive path must consult BackfillSafe ──────────
//
// external.BackfillSafe is the per-source "this decoder is safe against
// every historical WASM generation" gate. For a long time its only
// caller was `stellarindex-ops backfill`, while the commands operators
// actually use to re-decode history never asked.
//
// This leg GRADUATED out of the k023evidence build tag (it lived in
// deployed_controls_test.go) once all three re-derive paths asked:
// projector-replay, ch-rebuild, and projected-rebuild — the bulk sibling
// the projector-replay runbook sends any rewind over ~1M ledgers to,
// which builds the same current decoder over the same history. It is
// untagged so it runs in the default suite as the class's regression
// guard for this control; the other four legs stay tagged until their
// owning fixes land.
//
// Source-level on purpose: the assertion is "some non-test file on each
// re-derive path CALLS the external gate" (a comment mentioning it, or a
// local helper whose name merely ends in BackfillSafe, does not count).
// The behavioural tests that drive each real entry point are
// internal/ops/ingest/projector_backfillsafe_test.go,
// internal/ops/chops/ch_rebuild_backfillsafe_test.go and
// internal/ops/chops/projected_rebuild_backfillsafe_test.go.
//
// A NEW command that runs a current decoder over historical events must
// be added to this map in the same change that adds it.
func TestK023_ReplayPathsConsultBackfillSafe(t *testing.T) {
	t.Parallel()
	paths := map[string][]string{
		"projector-replay":  {"internal/ops/ingest/projector*.go", "internal/projector/*.go"},
		"ch-rebuild":        {"internal/ops/chops/ch_rebuild*.go"},
		"projected-rebuild": {"internal/ops/chops/projected_rebuild*.go"},
	}
	// The three exported forms of the one gate in
	// internal/sources/external/registry.go.
	gateCalls := []string{
		"external.BackfillSafe(",
		"external.ReplayBackfillSafe(",
		"external.UnsafeReplaySources(",
	}
	for cmd, globs := range paths {
		files, found := scanForCall(t, globs, gateCalls)
		if files == 0 {
			t.Fatalf("%s: globs %v matched no files — this test is asserting nothing", cmd, globs)
		}
		if !found {
			t.Errorf("%s never calls the external BackfillSafe gate (searched %d files under %v for %v): "+
				"it re-decodes history with the CURRENT decoder without asking whether that decoder was "+
				"audited against every WASM generation (F050)", cmd, files, globs, gateCalls)
		}
	}
}

// scanForCall reports how many non-test Go files the globs matched and
// whether any of them contains one of needles on a non-comment line.
func scanForCall(t *testing.T, globs, needles []string) (files int, found bool) {
	t.Helper()
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(repoRoot(t), g))
		if err != nil {
			t.Fatalf("glob %s: %v", g, err)
		}
		for _, f := range matches {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			files++
			b, err := os.ReadFile(f) //nolint:gosec // repo-relative, test-only
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if codeContainsAny(string(b), needles) {
				found = true
			}
		}
	}
	return files, found
}

// codeContainsAny reports whether any needle appears on a line that is
// not a `//` comment line.
func codeContainsAny(src string, needles []string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, n := range needles {
			if strings.Contains(line, n) {
				return true
			}
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// test/controlwiring -> repo root
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
