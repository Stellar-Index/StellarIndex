// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"regexp"
	"strings"
	"testing"
)

// ─── RSWP-061: CHANGELOG.md must not carry a dangling issue link ────
//
// The recompute-usd-volume-soroban.sql entry carried a bare `(#1000)`
// reference that never resolved to this change (it 404'd at the time
// this line was written). Because GitHub autolinks any bare `#NNN` in
// rendered markdown, and the tracker's counter has since advanced past
// 1000, that same text now silently resolves to an unrelated issue
// filed long after this release — an operator following the "source"
// link for this ops script lands somewhere else entirely. This pins
// the entry so a future edit can't reintroduce a bare, autolinkable
// issue reference here.
func TestChangelogSorobanRecomputeEntryHasNoDanglingIssueLink(t *testing.T) {
	changelog := readRepoFile(t, "CHANGELOG.md")

	const marker = "scripts/ops/recompute-usd-volume-soroban.sql"
	idx := strings.Index(changelog, marker)
	if idx == -1 {
		t.Fatalf("CHANGELOG.md no longer mentions %s — update this test alongside its removal", marker)
	}

	end := idx + 600
	if end > len(changelog) {
		end = len(changelog)
	}
	entry := changelog[idx:end]

	// A bare "(#1000)" (parens directly around the number, outside any
	// code span) is what GitHub autolinks to a live issue/PR. A form
	// like `` `#1000` `` inside backticks renders as plain text and is
	// fine.
	if bareIssueRef.MatchString(entry) {
		t.Errorf("CHANGELOG.md's %s entry still carries a bare (#NNN) reference that GitHub will autolink to whatever issue now holds that number:\n%s", marker, entry)
	}
}

var bareIssueRef = regexp.MustCompile(`[^` + "`" + `](\(#[0-9]+\))`)
