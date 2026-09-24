// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// explorerLayoutEntry matches one node of the README's box-drawn tree:
// indentation in 4-column units, a connector, then the entry name.
var explorerLayoutEntry = regexp.MustCompile(`^((?:│   | {4})*)(?:├── |└── )(\S+)`)

// TestExplorerReadmeLayoutEntriesExist pins web/explorer/README.md's
// "## Layout" tree to the files on disk, so deleting a component or lib
// module cannot leave the README advertising it as live.
func TestExplorerReadmeLayoutEntriesExist(t *testing.T) {
	const readme = "web/explorer/README.md"
	paths := explorerLayoutPaths(t, readMarkdownSection(t, readme, "## Layout"))
	if len(paths) < 10 {
		t.Fatalf("parsed only %d entries from %s's Layout tree; the parser no longer matches the tree", len(paths), readme)
	}
	base := filepath.Join(repoRoot(t), "web", "explorer")
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s Layout lists %s, which does not exist", readme, p)
		}
	}
}

// readMarkdownSection returns the first fenced block after heading.
func readMarkdownSection(t *testing.T, rel, heading string) string {
	t.Helper()
	doc := readRepoFile(t, rel)
	i := strings.Index(doc, "\n"+heading+"\n")
	if i == -1 {
		t.Fatalf("%s has no %q heading", rel, heading)
	}
	parts := strings.SplitN(doc[i:], "```", 3)
	if len(parts) < 3 {
		t.Fatalf("%s %q has no fenced block", rel, heading)
	}
	return parts[1]
}

// explorerLayoutPaths turns the tree into slash paths relative to the
// tree's root line (e.g. "src/components/AssetLabel.tsx").
func explorerLayoutPaths(t *testing.T, tree string) []string {
	t.Helper()
	var stack, out []string
	for _, line := range strings.Split(tree, "\n") {
		if stack == nil && strings.HasSuffix(strings.TrimSpace(line), "/") {
			stack = []string{strings.TrimSuffix(strings.TrimSpace(line), "/")}
			continue
		}
		m := explorerLayoutEntry.FindStringSubmatch(line)
		if m == nil || stack == nil {
			continue
		}
		depth := len([]rune(m[1]))/4 + 1
		if depth > len(stack) {
			t.Fatalf("tree line %q is nested deeper than its parent", line)
		}
		stack = append(stack[:depth], strings.TrimSuffix(m[2], "/"))
		out = append(out, strings.Join(stack, "/"))
	}
	return out
}
