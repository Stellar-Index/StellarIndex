package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decimalsErrorFoldAllowed are the files that may still fold a failed decimals read into "unknown", each with why.
var decimalsErrorFoldAllowed = map[string]string{
	"explorer.go":                  "display-only seam: omits the decimals field and never scales an amount with a default",
	"lending_emissions_history.go": "carried out as a decimals-unread flag on the emissions-history branch; remove on merge",
}

// A failed decimals read must not become "decimals unknown" (a settled, cacheable answer): no file folds the read's
// error into the known flag without also carrying it, as reserveDecimals does with unread.
func TestNoDecimalsReadErrorFoldedIntoUnknown(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			folds := strings.Contains(line, "known && err == nil") || strings.Contains(line, "err != nil || !known")
			if !folds || strings.Contains(line, "unread: err != nil") {
				continue
			}
			if _, ok := decimalsErrorFoldAllowed[f]; ok {
				seen[f] = true
				continue
			}
			t.Errorf("%s:%d folds a failed decimals read into unknown; carry the error (see reserveScale)", f, i+1)
		}
	}
	for f := range decimalsErrorFoldAllowed {
		if !seen[f] {
			t.Errorf("%s no longer folds the error; drop it from decimalsErrorFoldAllowed", f)
		}
	}
}
