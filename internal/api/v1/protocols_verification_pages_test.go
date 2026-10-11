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

// nonProtocolDocs are the pages under docs/protocols deliberately not a /v1/protocols row:
// the index, the two supply write-ups (indexing domains), and the Blend Emitter (folded into
// the Blend row). Anything else must be linked by a protocol, which
// TestProtocolVerificationPages_EveryDocIsLinked enforces.
var nonProtocolDocs = map[string]bool{
	"README.md":           true,
	"sep41-supply.md":     true,
	"supply-observers.md": true,
	"blend_emitter.md":    true,
	// Integration not built yet (no source, no registry row); move it to a
	// registry entry when the spectra source lands.
	"spectra.md": true,
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

// TestProtocolVerificationPages_ProtocolWithADocLinksIt: a protocol with a page under
// docs/protocols must have it linked, or /v1/protocols reports verification_page: null
// despite the doc existing.
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
