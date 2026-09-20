package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEngineeringStandardsDoesNotClaimUnbuiltEnforcement pins NS30/NS31/NS32:
// docs/engineering-standards.md §2.4, §2.6 and §2.7 described three CI
// enforcement mechanisms (a flag-age build warning/failure off a
// `internal/config/flags.go` registry, a `docs/reference/deprecations.md`
// table, and a CI removal-suggestion scan for `// workaround` comments) as
// if they existed. None of the three artifacts they name are in the repo,
// and the doc's own §Status line binds every PR and agent to it as design-
// locked policy — so an unqualified claim here is read as ground truth, not
// aspiration. This test fails if the doc goes back to asserting any of the
// three as implemented without the honest "Gap:" disclaimer this fix added,
// and it fails (loudly, telling the fixer to update it) the day one of the
// three artifacts actually lands.
func TestEngineeringStandardsDoesNotClaimUnbuiltEnforcement(t *testing.T) {
	root := repoRoot(t)

	docPath := filepath.Join(root, "docs", "engineering-standards.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(raw)

	sections := extractHeadedSections(t, doc, []string{
		"### 2.4. Deprecation policy",
		"### 2.6. Feature flag hygiene",
		"### 2.7. No \"temporary\" workarounds",
	})

	cases := []struct {
		heading      string
		artifact     string // repo-relative path the section claims exists
		unfixedRegex string // exact prose the doc asserted while the artifact didn't exist
	}{
		{
			heading:      "### 2.4. Deprecation policy",
			artifact:     "docs/reference/deprecations.md",
			unfixedRegex: "a\\s*\\n?\\s*dedicated `docs/reference/deprecations\\.md` table\\.",
		},
		{
			heading:      "### 2.6. Feature flag hygiene",
			artifact:     "internal/config/flags.go",
			unfixedRegex: "The flag registry `internal/config/flags\\.go` contains a struct",
		},
		{
			heading:      "### 2.7. No \"temporary\" workarounds",
			artifact:     "scripts/ci/check-workaround-removal.sh",
			unfixedRegex: "A removal test: when upstream fixes, CI finds the workaround",
		},
	}

	for _, c := range cases {
		section, ok := sections[c.heading]
		if !ok {
			t.Fatalf("%s: heading %q not found — has it moved or been renamed?", docPath, c.heading)
		}

		if _, err := os.Stat(filepath.Join(root, c.artifact)); err == nil {
			t.Fatalf("%s now exists — %s's enforcement claim is no longer aspirational; "+
				"update the doc to state it plainly and delete this test's gap check for it", c.artifact, c.heading)
		}

		if m, _ := regexp.MatchString(c.unfixedRegex, section); m {
			t.Errorf("%s still asserts (without qualification) that %s exists and is enforced, "+
				"but the file is absent from the repo — a PR/agent reading this design-locked doc "+
				"would believe non-existent CI enforcement is real", c.heading, c.artifact)
		}

		if !strings.Contains(strings.ToLower(section), "gap:") {
			t.Errorf("%s no longer carries a \"Gap:\" disclaimer for the unimplemented %s mechanism — "+
				"the section must say plainly that this enforcement does not exist yet", c.heading, c.artifact)
		}
	}
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
