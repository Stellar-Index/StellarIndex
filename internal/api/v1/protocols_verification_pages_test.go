package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootFromAPIV1 is the repo root relative to this package, so these
// tests can see the docs tree the registry links into.
const repoRootFromAPIV1 = "../../.."

// protocolDocsDir is the verification-page tree, repo-relative.
const protocolDocsDir = "docs/protocols"

// nonProtocolDocs are the pages under docs/protocols that are
// deliberately not a /v1/protocols row: the index, the two supply
// write-ups (indexing domains, not protocols), and the Blend Emitter
// (a module folded into the Blend row — it has no source name of its
// own in the directory). Anything else added to the tree must be linked
// by a protocol, which is what TestProtocolVerificationPages_EveryDocIsLinked
// enforces.
var nonProtocolDocs = map[string]bool{
	"README.md":           true,
	"sep41-supply.md":     true,
	"supply-observers.md": true,
	"blend_emitter.md":    true,
}

// TestProtocolVerificationPages_PointAtRealFiles fails on a link the
// wire would serve as a 404 on GitHub: the web protocol page renders
// verification_page as a repo blob URL, so a path with no file behind
// it is a broken link on a public page.
func TestProtocolVerificationPages_PointAtRealFiles(t *testing.T) {
	for _, p := range protocolRegistry {
		if p.VerificationPage == "" {
			continue
		}
		if !strings.HasPrefix(p.VerificationPage, protocolDocsDir+"/") {
			t.Errorf("%s: verification_page %q is not under %s/", p.Name, p.VerificationPage, protocolDocsDir)
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRootFromAPIV1, p.VerificationPage)); err != nil {
			t.Errorf("%s: verification_page %q has no file: %v", p.Name, p.VerificationPage, err)
		}
	}
}

// TestProtocolVerificationPages_ProtocolWithADocLinksIt is the missing
// half of the promise README and /protocols make. A page written for a
// protocol used to need a second, easily-missed edit here before the API
// linked it, and ten protocols shipped pages that /v1/protocols reported
// as verification_page: null.
func TestProtocolVerificationPages_ProtocolWithADocLinksIt(t *testing.T) {
	for _, p := range protocolRegistry {
		doc := filepath.Join(protocolDocsDir, p.Name+".md")
		if _, err := os.Stat(filepath.Join(repoRootFromAPIV1, doc)); err != nil {
			continue // documented on another protocol's page, or not yet written
		}
		if p.VerificationPage != doc {
			t.Errorf("%s: verification_page = %q, want %q — the page exists and the API must link it",
				p.Name, p.VerificationPage, doc)
		}
	}
}

// TestProtocolVerificationPages_EveryDocIsLinked walks the other way: a
// page nothing points at is invisible to every API consumer, so a new
// doc must either belong to a protocol row or be declared here as one
// of the non-protocol pages.
func TestProtocolVerificationPages_EveryDocIsLinked(t *testing.T) {
	linked := map[string]bool{}
	for _, p := range protocolRegistry {
		if p.VerificationPage != "" {
			linked[filepath.Base(p.VerificationPage)] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(repoRootFromAPIV1, protocolDocsDir))
	if err != nil {
		t.Fatalf("read %s: %v", protocolDocsDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || nonProtocolDocs[name] {
			continue
		}
		if !linked[name] {
			t.Errorf("%s/%s is linked by no protocol — wire it to a registry entry, "+
				"or add it to nonProtocolDocs if it documents no protocol", protocolDocsDir, name)
		}
	}
}
