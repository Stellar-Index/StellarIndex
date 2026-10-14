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

// nonProtocolDocs are the docs/protocols pages deliberately not a /v1/protocols row: the index, the two supply write-ups,
// and the Blend Emitter. Any other doc must be linked by a protocol (TestProtocolVerificationPages/EveryDocIsLinked).
var nonProtocolDocs = map[string]bool{
	"README.md":           true,
	"sep41-supply.md":     true,
	"supply-observers.md": true,
	"blend_emitter.md":    true,
	// Integration not built yet (no source, no registry row); move it to a
	// registry entry when the spectra source lands.
	"spectra.md": true,
}

// TestProtocolVerificationPages pins the registry to the docs/protocols tree in both directions: every link points at a
// real file (the web page renders it as a repo blob URL that would 404), a protocol whose page exists links it (or
// /v1/protocols reports verification_page: null), and every doc belongs to a row or is declared a non-protocol page.
func TestProtocolVerificationPages(t *testing.T) {
	t.Run("PointAtRealFiles", func(t *testing.T) {
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
	})
	t.Run("ProtocolWithADocLinksIt", func(t *testing.T) {
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
	})
	t.Run("EveryDocIsLinked", func(t *testing.T) {
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
	})
}
