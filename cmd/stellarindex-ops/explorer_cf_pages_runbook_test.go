package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRunbooksDirHasExplorerCloudflarePagesOutageRunbook pins T299
// (audit-2026-09-02): docs/operations/runbooks/ had no runbook for an
// explorer, Cloudflare Pages, edge-function, or CDN failure — a
// filename grep for explorer|cloudflare|cf-pages|cdn across the
// directory returned nothing, and a content grep for the same terms
// only hit infra runbooks mentioning Cloudflare incidentally (as a
// dependency), never as the subject of the runbook.
func TestRunbooksDirHasExplorerCloudflarePagesOutageRunbook(t *testing.T) {
	root := repoRootForOpsTest(t)
	dir := filepath.Join(root, "docs/operations/runbooks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	nameRE := regexp.MustCompile(`(?i)explorer|cloudflare|cf-pages|cdn`)
	var subject string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if nameRE.MatchString(e.Name()) {
			subject = e.Name()
			break
		}
	}
	if subject == "" {
		t.Fatal("docs/operations/runbooks/ has no runbook file named for " +
			"explorer/cloudflare/cf-pages/cdn — there is no runbook for an " +
			"explorer, Cloudflare Pages, edge-function, or CDN failure")
	}

	body, err := os.ReadFile(filepath.Join(dir, subject))
	if err != nil {
		t.Fatalf("read %s: %v", subject, err)
	}
	content := string(body)

	// The runbook must actually cover the three failure modes named in
	// the finding, not just exist under a matching filename.
	for _, want := range []string{
		"Cloudflare Pages",
		"edge-function", // edge-function failure mode
		"rollback",      // recovery path
	} {
		if !strings.Contains(strings.ToLower(content), strings.ToLower(want)) {
			t.Errorf("%s does not mention %q — the runbook must cover the "+
				"CF Pages outage, edge-function failure, and rollback modes "+
				"the finding calls out", subject, want)
		}
	}
}
