package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// T497: the file's own header comment claims auto-discovery at "the
// conventional /openapi.yaml and /openapi.json paths", but the rule table
// only ever defined the .yaml/.yml/.spec forms, never .json. A tool that
// probes /openapi.json (or /spec.json) hit the CF Pages SPA fallback
// (200, wrong body) instead of the spec.
func TestRedirectsCoverJSONDiscoveryPaths(t *testing.T) {
	data, err := os.ReadFile("_redirects")
	if err != nil {
		t.Fatalf("read _redirects: %v", err)
	}
	text := string(data)

	for _, path := range []string{"/openapi.json", "/spec.json"} {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(path) + `\s+/stellar-index\.v1\.yaml\s+200\s*$`)
		if !re.MatchString(text) {
			t.Errorf("expected a rule redirecting %s to /stellar-index.v1.yaml (200), matching the header comment's claimed auto-discovery paths; none found in:\n%s", path, text)
		}
	}

	if !strings.Contains(text, "openapi.json") {
		t.Fatalf("sanity check failed: %q not found anywhere in _redirects", "openapi.json")
	}
}
