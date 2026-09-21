// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"regexp"
	"strings"
	"testing"
)

// ─── RSWP-069: CHANGELOG.md must not carry a dangling/misdirected issue link ─
//
// The "/assets/[slug] converter: searchable CurrencyCombobox" entry carried
// a bare `(PR #1043)` reference that never resolved to this change. GitHub
// autolinks any bare `#NNN` in rendered markdown, and the tracker's counter
// has since advanced past 1043, so the same text now silently resolves to
// an unrelated issue filed long after this release — a reader following
// the "source" link for this entry lands somewhere else entirely. This
// pins the entry so a future edit can't reintroduce a bare, autolinkable
// issue reference here.
func TestChangelogCurrencyComboboxEntryHasNoDanglingIssueLink(t *testing.T) {
	changelog := readRepoFile(t, "CHANGELOG.md")

	const marker = "searchable CurrencyCombobox"
	markerIdx := strings.Index(changelog, marker)
	if markerIdx == -1 {
		t.Fatalf("CHANGELOG.md no longer mentions %s — update this test alongside its removal", marker)
	}

	// Isolate just this bullet: from its own "- **" up to (but not
	// including) the next changelog bullet, so a reference on a
	// neighbouring entry can't leak into this one's check.
	start := strings.LastIndex(changelog[:markerIdx], "- **")
	if start == -1 {
		start = markerIdx
	}
	rest := changelog[start+len("- **"):]
	end := len(changelog)
	if next := strings.Index(rest, "\n- "); next != -1 {
		end = start + len("- **") + next
	}
	entry := changelog[start:end]

	// A bare "#1043" (with or without a "PR"/"Issue" prefix or
	// surrounding parens) is what GitHub autolinks to a live issue/PR.
	// A form like `` `#1043` `` inside backticks renders as plain text
	// and is fine.
	if ref := comboboxBareIssueRef(entry); ref != "" {
		t.Errorf("CHANGELOG.md's %s entry still carries a bare issue/PR reference (%q) that GitHub will autolink to whatever issue now holds that number:\n%s", marker, ref, entry)
	}
}

var comboboxIssueNumberRef = regexp.MustCompile(`#[0-9]+`)

// comboboxBareIssueRef returns the first "#NNN" reference in s that is not
// wrapped in backticks (i.e. would render as a live GitHub autolink), or ""
// if every reference is backtick-quoted.
func comboboxBareIssueRef(s string) string {
	for _, loc := range comboboxIssueNumberRef.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		wrapped := start > 0 && s[start-1] == '`' && end < len(s) && s[end] == '`'
		if !wrapped {
			return s[start:end]
		}
	}
	return ""
}
