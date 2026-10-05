package main

import (
	"strings"
	"testing"
)

// TestSubcommandDispatch_LeafHandlersSeeTheirFlags is the coverage that was
// missing and let every leaf subcommand ship UNINVOKABLE.
//
// The dispatch table hands a handler the FULL argv (args[0] == the verb).
// Go's flag package stops parsing at the first non-flag argument, so a leaf
// handler doing fs.Parse(args) on that argv parses NOTHING and every flag
// keeps its zero value: `stellarindex-ops usage-rollup-backfill -config
// /etc/stellarindex.toml -from DATE` answered "-config is required"
// and there was no way to run it at all. The existing unit tests all call
// the handlers DIRECTLY with flags-only argv, so they exercised the
// convention the handlers wanted and never the one the dispatcher used —
// which is precisely why nothing caught it.
//
// This test goes through `subcommands`, the way main() does. The assertion
// is deliberately "the error is NOT the no-flags-parsed symptom": each
// handler's next gate differs, but every one of them reports
// "-config is required" if and only if the flags were dropped.
func TestSubcommandDispatch_LeafHandlersSeeTheirFlags(t *testing.T) {
	cases := []struct {
		verb string
		argv []string
	}{
		{"mint-key", []string{"mint-key", "-config", "/nonexistent.toml", "-identifier", "customer-acme", "-label", "Acme"}},
		{"upgrade-key", []string{"upgrade-key", "-config", "/nonexistent.toml"}},
		{"emit-incident", []string{"emit-incident", "-config", "/nonexistent.toml", "-slug", "s", "-event", "sev1"}},
		{"usage-rollup-backfill", []string{"usage-rollup-backfill", "-config", "/nonexistent.toml", "-from", "2026-07-19"}},
		{"freeze-unfreeze", []string{"freeze-unfreeze", "-config", "/nonexistent.toml", "-list"}},
		{"change-summary-reset", []string{"change-summary-reset", "-config", "/nonexistent.toml", "-entity-type", "coin", "-entity-id", "crypto:XLM"}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			run, ok := subcommands[tc.verb]
			if !ok {
				t.Fatalf("%s is not in the dispatch table", tc.verb)
			}
			err := run(tc.argv)
			if err == nil {
				t.Fatalf("%s: expected an error (the config file does not exist)", tc.verb)
			}
			if strings.Contains(err.Error(), "-config is required") {
				t.Fatalf("%s: dispatch dropped the flags — -config was passed but the handler never saw it (got %q). The subcommand is uninvokable from the CLI.", tc.verb, err)
			}
		})
	}
}
