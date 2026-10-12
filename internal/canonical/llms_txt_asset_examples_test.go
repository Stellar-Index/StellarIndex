// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Matches the backtick-quoted asset_id examples in llms.txt's intro whose prefix or shape marks them as canonical asset
// identifiers, not incidental code such as route paths or HTTP verbs.
var llmsTxtAssetIDExample = regexp.MustCompile("`(native|XLM|[a-z]+:[A-Za-z0-9:]+|[A-Z0-9]+-G[A-Z0-9]{55})`")

// llms.txt, which AI agents read to learn the asset_id format, may only advertise shapes ParseAsset accepts. A colon
// inside the issuer breaks the classic code:issuer split, as `credit:USDC:G...` did; every example must round-trip.
func TestLlmsTxtAssetExamples_ParseAsCanonicalAssets(t *testing.T) {
	path := filepath.Join(repoRoot(), "web/explorer/public/llms.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	matches := llmsTxtAssetIDExample.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		t.Fatal("no asset_id examples found in llms.txt — the regex or the doc's intro paragraph moved; update one of them")
	}

	for _, m := range matches {
		example := m[1]
		if _, err := ParseAsset(example); err != nil {
			t.Errorf("llms.txt advertises asset_id example %q, but ParseAsset(%q) = %v — "+
				"the published format must be one our own API accepts", example, example, err)
		}
	}
}
