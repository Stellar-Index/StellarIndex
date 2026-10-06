package main

import (
	"sort"
	"strings"
	"testing"
)

// TestUsageBodyCoversEverySubcommand pins the parity invariant stated in the
// [subcommands] doc: "The canonical subcommand list is this table + the
// usageBody help text." Every dispatchable subcommand MUST have its OWN help
// entry — a line whose first non-blank content is the subcommand name at the
// two-space entry column — so `stellarindex-ops --help` never omits a command
// an operator (or a migration follow-up, e.g. projector-replay) is told to run.
//
// A mention buried inside another command's description or example lines does
// NOT count: those are indented far past the entry column. This test therefore
// requires an entry line matching "^  <name>( |\t|[|$)".
func TestUsageBodyCoversEverySubcommand(t *testing.T) {
	lines := strings.Split(usageBody, "\n")

	hasOwnEntry := func(name string) bool {
		prefix := "  " + name
		for _, ln := range lines {
			// Own entry: exactly two leading spaces, then the name, then a
			// token boundary. A three-plus-space continuation/example line
			// fails the exact "  " prefix, and a longer command that merely
			// starts with this name fails the boundary check.
			if !strings.HasPrefix(ln, prefix) {
				continue
			}
			rest := ln[len(prefix):]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '[' {
				return true
			}
		}
		return false
	}

	var missing []string
	for name := range subcommands {
		if !hasOwnEntry(name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("%d dispatchable subcommand(s) have no own --help entry in usageBody "+
			"(add one line each at the two-space entry column): %s",
			len(missing), strings.Join(missing, ", "))
	}
}

// TestUsageBodyListsOnlyRealSubcommands is the other half of the parity
// check: an entry-column line in usageBody naming a command that is neither
// dispatchable nor a built-in is a stale help entry (a renamed or deleted
// subcommand still advertised).
func TestUsageBodyListsOnlyRealSubcommands(t *testing.T) {
	builtin := map[string]bool{"version": true, "help": true}
	var stale []string
	for _, ln := range strings.Split(usageBody, "\n") {
		if len(ln) < 3 || ln[0] != ' ' || ln[1] != ' ' || ln[2] == ' ' {
			continue
		}
		name := strings.Fields(ln)[0]
		if _, ok := subcommands[name]; ok || builtin[name] || name == "stellarindex-ops" || strings.HasSuffix(name, ":") {
			continue
		}
		if !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, "[](){}<>=") {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("usageBody advertises %d name(s) that are not dispatchable: %s", len(stale), strings.Join(stale, ", "))
	}
}
