package main

import (
	"sort"
	"strings"
	"testing"
)

// TestUsageBody holds the --help text invariants as subtests: every
// dispatchable subcommand has its own entry, no entry is stale, and the
// usage-rollup-backfill entry carries -write in its synopsis and example.
func TestUsageBody(t *testing.T) {
	t.Run("covers_every_subcommand", func(t *testing.T) {
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
	})

	t.Run("lists_only_real_subcommands", func(t *testing.T) {
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
	})

	t.Run("rollup_backfill_documents_write", func(t *testing.T) {
		i := strings.Index(usageBody, "  usage-rollup-backfill ")
		if i < 0 {
			t.Fatal("usageBody has no usage-rollup-backfill entry")
		}
		// The entry runs until the next line indented like a synopsis
		// ("  <name>"), i.e. the next subcommand.
		synopsis, entry, _ := strings.Cut(usageBody[i:], "\n")
		for n, line := range strings.Split(entry, "\n") {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
				entry = strings.Join(strings.Split(entry, "\n")[:n], "\n")
				break
			}
		}
		if !strings.Contains(synopsis, "[-write]") {
			t.Errorf("usage-rollup-backfill synopsis lacks [-write]: %q", synopsis)
		}
		if !strings.Contains(entry, "-to 2026-07-21 -write") {
			t.Errorf("usage-rollup-backfill --help example does not pass -write:\n%s", entry)
		}
	})
}
