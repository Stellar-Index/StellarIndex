// lint-chaos-ci-wiring guards the fix for T430: test/chaos/ (Task #75,
// Wave 1) shipped with a runner, scenarios and a design note that settled
// on "nightly is enough" for CI cadence, but no workflow ever ran it — a
// grep of .github/workflows/*.yml for 'chaos' returned zero matches. The
// suite existed and caught nothing.
//
// This lint fails when no workflow file references the chaos runner
// (`test/chaos/run.sh` or the `test-chaos`/`test-chaos-check` Makefile
// targets), so a future edit that renames or removes the wiring (e.g.
// chaos-nightly.yml) fails CI instead of silently reverting to the
// pre-T430 state.
//
// Usage (from the repo root):
//
//	go run ./scripts/ci/lint-chaos-ci-wiring
//
// Exits 0 when at least one workflow references the suite, 1 otherwise.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// chaosMarkers are the strings that count as "this workflow runs the chaos
// suite". Matched against the raw file text rather than parsed YAML: the
// runner is invoked from a shell `run:` step, not a `uses:` step, so there
// is no structured field to key off.
var chaosMarkers = []string{"test/chaos/run.sh", "test-chaos-check", "make test-chaos"}

func defaultGlob() string {
	return ".github/workflows/*.yml"
}

func main() {
	failures, summary, err := run(defaultGlob())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint-chaos-ci-wiring: FAULT — %v\n", err)
		os.Exit(1)
	}
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "lint-chaos-ci-wiring: FAIL — %d problem(s):\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println(summary)
}

// run reports a failure when no workflow matching glob references the
// chaos suite. A malformed glob is a fault (returned as err); zero matching
// files, or files that don't mention the suite, are a lint failure.
func run(glob string) (failures []string, summary string, err error) {
	files, err := filepath.Glob(glob)
	if err != nil {
		return nil, "", fmt.Errorf("glob %s: %w", glob, err)
	}
	sort.Strings(files)

	var wired []string
	for _, file := range files {
		raw, rerr := os.ReadFile(file) //nolint:gosec // fixed repo path (or a test fixture), not user input
		if rerr != nil {
			return nil, "", fmt.Errorf("read %s: %w", file, rerr)
		}
		text := string(raw)
		for _, marker := range chaosMarkers {
			if strings.Contains(text, marker) {
				wired = append(wired, file)
				break
			}
		}
	}

	if len(wired) == 0 {
		failures = append(failures, fmt.Sprintf(
			"no workflow under %s references the chaos suite (test/chaos/run.sh, test-chaos or test-chaos-check) — test/chaos/ (Task #75) exists but nothing runs it in CI (T430)",
			glob))
		return failures, "", nil
	}

	summary = fmt.Sprintf("lint-chaos-ci-wiring: OK — chaos suite wired into %s", strings.Join(wired, ", "))
	return failures, summary, nil
}
