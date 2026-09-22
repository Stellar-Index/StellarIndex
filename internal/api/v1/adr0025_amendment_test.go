package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADR0025PinsThe1239AmendmentNotice guards RSWP-132: ADR-0025's two
// "PR #1239" citations (Context intro, References > Implementation) no
// longer identify the Caddy trusted-proxy fix they describe. No PR
// #1239 has ever existed in this repo; GitHub has since assigned #1239
// to a real but unrelated open issue about admin-key monthly quotas,
// so a reader following the citation lands on wrong content instead of
// a 404 — worse than a dead link.
//
// Per docs/adr/README.md's amendment rule, the original ADR body is
// left intact (both "PR #1239" mentions stay, as historical record)
// and the correction lives in a dated Amendment blockquote. This test
// pins that the blockquote exists and still calls out both the wrong
// resolution and the unrelated target, so it can't be silently dropped
// in a future edit of the doc.
func TestADR0025PinsThe1239AmendmentNotice(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "adr", "0025-caddy-cloudflare-trusted-proxy.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(b)

	for _, want := range []string{
		"Amendment (2026-09-21, RSWP-132)",
		"No PR #1239 has ever\n> existed in this repo",
		"admin-key monthly quotas",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing amendment text %q; the dangling PR #1239 citation is no longer flagged as misdirecting a reader to an unrelated issue", path, want)
		}
	}

	// The original citations must survive untouched (README.md's
	// "amend, don't rewrite" rule) — the amendment explains them, it
	// doesn't replace them.
	for _, original := range []string{
		"The bug we caught on 2026-05-10 (PR #1239): Caddy's",
		"Implementation: PR #1239 — `fix(caddy): resolve real client",
	} {
		if !strings.Contains(doc, original) {
			t.Errorf("%s: original citation %q was rewritten or removed; ADR body text must only be amended, per docs/adr/README.md", path, original)
		}
	}
}
