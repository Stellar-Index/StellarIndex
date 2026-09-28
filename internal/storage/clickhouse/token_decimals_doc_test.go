// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"os"
	"strings"
	"testing"
)

// The scale is read on two axes — which instance map (tokenMetadataMapKeys)
// and which field name inside it (tokenDecimalsKeys). Both methodology
// pages that explain the exponent must name every spelling on both axes,
// or a reader re-deriving a figure from one page misses half the reader.
func TestDecimalsDocsNameBothSpellingAxes(t *testing.T) {
	pages := []struct{ path, heading string }{
		{"../../../docs/methodology/contract-storage-supply.md", "## 6. Decimals are read, never assumed"},
		{"../../../docs/methodology/rwa-definition.md", "### Valuing a contract asset"},
	}
	keys := append(append([]string(nil), tokenMetadataMapKeys...), tokenDecimalsKeys...)
	for _, p := range pages {
		b, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatalf("read %s: %v", p.path, err)
		}
		doc := string(b)
		start := strings.Index(doc, "\n"+p.heading+"\n")
		if start < 0 {
			t.Fatalf("%s: heading %q not found", p.path, p.heading)
		}
		sec := doc[start+len(p.heading)+2:]
		if end := strings.Index(sec, "\n#"); end >= 0 {
			sec = sec[:end]
		}
		for _, k := range keys {
			if !strings.Contains(sec, "`"+k+"`") {
				t.Errorf("%s %q never names the %q spelling the reader accepts", p.path, p.heading, k)
			}
		}
	}
}
