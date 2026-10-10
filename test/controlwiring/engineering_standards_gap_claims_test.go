package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEngineeringStandardsSection6DeprecationScanIsReal: section 6 names
// check-deprecations.sh as the deprecation policy's mechanism, so it must exist.
func TestEngineeringStandardsSection6DeprecationScanIsReal(t *testing.T) {
	root := repoRoot(t)
	doc := readEngineeringStandardsDoc(t, root)
	const artifact = "scripts/ci/check-deprecations.sh"

	row := regexp.MustCompile(`(?m)^\|\s*Deprecation policy\s*\|.*$`).FindString(doc)
	if !strings.Contains(row, "`"+artifact+"`") {
		t.Errorf("section 6 Deprecation policy row does not name %s:\n%s", artifact, row)
	}
	if _, err := os.Stat(filepath.Join(root, artifact)); err != nil {
		t.Errorf("section 6 names %s as the deprecation policy's enforcement, but it is missing: %v", artifact, err)
	}
}

func readEngineeringStandardsDoc(t *testing.T, root string) string {
	t.Helper()
	docPath := filepath.Join(root, "docs", "engineering-standards.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	return string(raw)
}

// extractHeadedSections splits doc on Markdown "### " headings and returns,
// for each heading in want present in doc, the text from that heading up to
// (but not including) the next "### " or "## " heading.
func extractHeadedSections(t *testing.T, doc string, want []string) map[string]string {
	t.Helper()
	headingRE := regexp.MustCompile(`(?m)^#{2,3} .+$`)
	idx := headingRE.FindAllStringIndex(doc, -1)
	out := make(map[string]string, len(want))
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	for i, loc := range idx {
		heading := doc[loc[0]:loc[1]]
		if !wantSet[heading] {
			continue
		}
		end := len(doc)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[heading] = doc[loc[0]:end]
	}
	return out
}
