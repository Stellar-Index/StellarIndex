package main

import (
	"strings"
	"testing"
)

// TestSubcommandDispatch_LeafHandlersSeeTheirFlags guards against uninvokable leaf subcommands. The table passes the full argv
// (args[0] == verb) and flag stops at the first non-flag, so fs.Parse(args) parses nothing and reports "-config is required".
// It goes through `subcommands` as main() does, and asserts that symptom is absent.
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
