package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEngineeringStandardsForbiddenPatternsMatchLintReality pins NS21:
// docs/engineering-standards.md §2.2 listed eight "forbidden patterns" under
// the unqualified header "The following trigger automatic PR blocks" — read
// literally by a design-locked, agent-binding doc as "golangci-lint blocks
// all eight". `.golangci.yml` enables no `forbidigo`, `gochecknoglobals`, or
// any rule targeting `panic()`/`time.Now()`/`init()`; only the goroutine-
// context pattern (`contextcheck`) and the SQL pattern (`sqlclosecheck`,
// which catches the unclosed-rows symptom, not the concatenation itself)
// have any mechanical backing. This test fails if the section reverts to
// the blanket claim, and it fails (loudly) the day `.golangci.yml` actually
// gains one of the missing rules — at which point the doc's annotation for
// that bullet should flip to CI-enforced instead of the check being deleted.
func TestEngineeringStandardsForbiddenPatternsMatchLintReality(t *testing.T) {
	root := repoRoot(t)
	doc := readEngineeringStandardsDoc(t, root)
	section := extractHeadedSections(t, doc, []string{"### 2.2. Forbidden patterns"})["### 2.2. Forbidden patterns"]
	if section == "" {
		t.Fatal(`heading "### 2.2. Forbidden patterns" not found — has it moved or been renamed?`)
	}

	// The blanket claim this fix removed: every listed pattern is an
	// "automatic PR block", i.e. mechanically enforced by CI.
	blanketRE := regexp.MustCompile(`(?i)following\s+trigger\s+automatic\s+PR\s+blocks`)
	if blanketRE.MatchString(section) {
		t.Error(`§2.2 still claims "the following trigger automatic PR blocks" — that is false for ` +
			"six of the eight listed patterns; qualify each bullet with its real enforcement status instead")
	}

	// Reviewer-enforced bullets: no golangci-lint rule backs these. Each
	// must say so plainly rather than imply CI enforcement.
	reviewerEnforced := []string{
		"interface{}` / `any` in `pkg/*`",
		"panic()` outside `main()` and tests",
		"init()` functions that do more",
		"time.Now()` inside business logic",
		"Global mutable state",
		"Dependency with < 1 GitHub star",
	}
	for _, marker := range reviewerEnforced {
		bullet := bulletContaining(t, section, marker)
		if !strings.Contains(strings.ToLower(bullet), "reviewer-enforced") {
			t.Errorf("bullet for %q no longer marks itself reviewer-enforced, but no golangci-lint rule "+
				"(forbidigo/gochecknoglobals/panic-time-init) backs it:\n%s", marker, bullet)
		}
	}

	// The two bullets that do have a real lint rule behind them must name it.
	ciBacked := []struct{ marker, rule string }{
		{"Goroutines without explicit context", "contextcheck"},
		{"SQL string concatenation", "sqlclosecheck"},
	}
	for _, c := range ciBacked {
		bullet := bulletContaining(t, section, c.marker)
		if !strings.Contains(bullet, c.rule) {
			t.Errorf("bullet for %q should name the lint rule (%s) that actually enforces it:\n%s",
				c.marker, c.rule, bullet)
		}
	}

	// Cross-check against reality: if any of these get enabled, the
	// affected bullet's annotation needs to flip from reviewer- to
	// CI-enforced, not just have this test deleted.
	golangci, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	for _, forbidden := range []string{"forbidigo", "gochecknoglobals"} {
		if strings.Contains(string(golangci), forbidden) {
			t.Errorf(".golangci.yml now enables %q — §2.2's reviewer-enforced annotations are stale for "+
				"whichever bullet that rule covers; update the doc, don't just delete this check", forbidden)
		}
	}
}

// bulletContaining returns the single markdown bullet (from its leading
// "- " to the line before the next "- ") whose text contains marker.
func bulletContaining(t *testing.T, section, marker string) string {
	t.Helper()
	lines := strings.Split(section, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "- ") {
			if start != -1 {
				break
			}
		}
		if strings.Contains(line, marker) && start == -1 {
			// walk back to the bullet's own "- " start
			for j := i; j >= 0; j-- {
				if strings.HasPrefix(lines[j], "- ") {
					start = j
					break
				}
			}
		}
	}
	if start == -1 {
		t.Fatalf("no bullet containing %q found in section:\n%s", marker, section)
	}
	end := len(lines)
	for k := start + 1; k < len(lines); k++ {
		if strings.HasPrefix(lines[k], "- ") || strings.HasPrefix(lines[k], "### ") {
			end = k
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}
