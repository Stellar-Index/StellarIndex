package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

var backfillExampleSourceRe = regexp.MustCompile(`stellarindex-ops backfill \\[^$]*?-source ([a-z0-9_,-]+)`)

// usageEntry returns name's usageBody entry: its synopsis line plus continuation lines up to the next "  <name>" line.
func usageEntry(t *testing.T, name string) string {
	t.Helper()
	i := strings.Index(usageBody, "  "+name+" ")
	if i < 0 {
		t.Fatalf("usageBody has no %s entry", name)
	}
	lines := strings.Split(usageBody[i:], "\n")
	for n, line := range lines[1:] {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			return strings.Join(lines[:n+1], "\n")
		}
	}
	return usageBody[i:]
}

// TestUsageBody holds the --help text invariants as subtests: every
// dispatchable subcommand has its own entry, no entry is stale, and the
// per-command entries match what their handlers do.
func TestUsageBody(t *testing.T) {
	t.Run("covers_every_subcommand", func(t *testing.T) {
		lines := strings.Split(usageBody, "\n")

		hasOwnEntry := func(name string) bool {
			prefix := "  " + name
			for _, ln := range lines {
				// Own entry: exactly two leading spaces, then the name, then a token boundary; continuation lines and longer commands sharing
				// the prefix must not match.
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
		entry := usageEntry(t, "usage-rollup-backfill")
		synopsis, _, _ := strings.Cut(entry, "\n")
		if !strings.Contains(synopsis, "[-write]") {
			t.Errorf("usage-rollup-backfill synopsis lacks [-write]: %q", synopsis)
		}
		if !strings.Contains(entry, "-to 2026-07-21 -write") {
			t.Errorf("usage-rollup-backfill --help example does not pass -write:\n%s", entry)
		}
	})

	// The handler writes curated_rwa_published_series (monthly market-cap total, subclass split) and refuses the WHOLE run on a bad row.
	t.Run("curated_rwa_sync_matches_handler", func(t *testing.T) {
		entry := usageEntry(t, "curated-rwa-sync")
		if strings.Contains(entry, "rwa_curated_directory") {
			t.Errorf("curated-rwa-sync help names rwa_curated_directory, which the handler never writes:\n%s", entry)
		}
		if !strings.Contains(entry, "curated_rwa_published_series") {
			t.Errorf("curated-rwa-sync help does not name curated_rwa_published_series, the table it writes:\n%s", entry)
		}
		if strings.Contains(entry, "per-token") || strings.Contains(entry, "per token") {
			t.Errorf("curated-rwa-sync help claims a per-token price; the handler reads a monthly market-cap total:\n%s", entry)
		}
		if strings.Contains(entry, "skipped and counted") {
			t.Errorf("curated-rwa-sync help claims malformed rows are skipped; curatedRWAWholeResult refuses the whole run:\n%s", entry)
		}
	})

	// The backfill example may name only sources backfill accepts: not projector-owned (invariant [7]) and BackfillSafe.
	t.Run("backfill_example_is_admissible", func(t *testing.T) {
		m := backfillExampleSourceRe.FindStringSubmatch(usageBody)
		if m == nil {
			t.Fatal("no `stellarindex-ops backfill ... -source` example found in usageBody; the detector is broken, not the help text")
		}
		for _, s := range strings.Split(m[1], ",") {
			if projector.IsProjectedSource(s, config.OracleConfig{}, nil) {
				t.Errorf("backfill example names projector-owned source %q; backfill refuses it", s)
			}
			if !external.BackfillSafe(s) {
				t.Errorf("backfill example names %q, which is not BackfillSafe; backfill refuses it", s)
			}
		}
	})
}
