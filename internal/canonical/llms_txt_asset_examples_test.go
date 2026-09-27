// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// llmsTxtAssetIDExample matches the backtick-quoted asset_id examples
// in llms.txt's intro paragraph — the ones whose prefix or shape
// declares them as canonical asset identifiers, not incidental code
// (route paths, HTTP verbs, etc).
var llmsTxtAssetIDExample = regexp.MustCompile("`(native|XLM|[a-z]+:[A-Za-z0-9:]+|[A-Z0-9]+-G[A-Z0-9]{55})`")

// TestLlmsTxtAssetExamples_ParseAsCanonicalAssets pins T259: llms.txt
// is public-facing documentation an AI agent reads to learn our
// asset_id format, and it once taught a shape (`credit:USDC:G…`) that
// ParseAsset rejects outright — a colon inside the issuer breaks the
// classic code:issuer alias split. Every asset_id example the file
// advertises must actually round-trip through ParseAsset.
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
