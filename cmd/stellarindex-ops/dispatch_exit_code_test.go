package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// caseDispatchExitCode_HelpIsNotAFailure: -h/-help surfaces as flag.ErrHelp (FlagSets use ContinueOnError) and must exit 0,
// not fall into the generic error branch that exits 1 like a real failure.
func caseDispatchExitCode_HelpIsNotAFailure(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", flag.ErrHelp, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (help is not a failure)", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty — the flag package already printed usage via fs.Output()", stderr.String())
	}
}

// caseDispatchExitCode_WrappedHelpIsNotAFailure covers a handler that wraps
// flag.ErrHelp (fmt.Errorf("%w", ...)) rather than returning it bare —
// errors.Is must still unwrap it.
func caseDispatchExitCode_WrappedHelpIsNotAFailure(t *testing.T) {
	var stderr bytes.Buffer
	wrapped := errors.Join(flag.ErrHelp)
	code := dispatchExitCode("backfill", wrapped, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (wrapped help is still not a failure)", code)
	}
}

// caseDispatchExitCode_RealErrorsStillFail guards against a too-broad fix:
// an ordinary handler error must still print the "<subcommand>: <err>"
// prefix and return 1.
func caseDispatchExitCode_RealErrorsStillFail(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", errors.New("boom"), &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backfill: boom") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "backfill: boom")
	}
}

// caseDispatchExitCode_ExitSilentlyStillSilent guards the existing
// ErrExitSilently convention (handler already printed its own message).
func caseDispatchExitCode_ExitSilentlyStillSilent(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", opsutil.ErrExitSilently, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// caseDispatchExitCode_ExitCodeErrorStillWins guards the existing
// opsutil.ExitCodeError convention (a specific non-1 exit code).
func caseDispatchExitCode_ExitCodeErrorStillWins(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("reconcile-balances", &opsutil.ExitCodeError{Code: 3, Err: errors.New("3 mismatches")}, &stderr)

	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "reconcile-balances: 3 mismatches") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "reconcile-balances: 3 mismatches")
	}
}

// TestEverySubcommandHelpExitsZero drives every help spelling through every dispatch entry and the real exit-code chokepoint.
// Namespace verbs and rpc-probe read args[1] before any FlagSet exists, so they must answer flag.ErrHelp directly.
func TestEverySubcommandHelpExitsZero(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	stdout, stderrFile := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	defer func() { os.Stdout, os.Stderr = stdout, stderrFile }()

	for name, run := range subcommands {
		for _, help := range []string{"-h", "-help", "--help"} {
			var stderr bytes.Buffer
			code := dispatchExitCode(name, run([]string{name, help}), &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Errorf("%s %s: exit %d, stderr %q; want exit 0 and no error line", name, help, code, stderr.String())
			}
		}
	}
}

// TestDispatchExitCode runs every exit-code mapping of the dispatch chokepoint as a named subtest.
func TestDispatchExitCode(t *testing.T) {
	t.Run("DispatchExitCode_HelpIsNotAFailure", caseDispatchExitCode_HelpIsNotAFailure)
	t.Run("DispatchExitCode_WrappedHelpIsNotAFailure", caseDispatchExitCode_WrappedHelpIsNotAFailure)
	t.Run("DispatchExitCode_RealErrorsStillFail", caseDispatchExitCode_RealErrorsStillFail)
	t.Run("DispatchExitCode_ExitSilentlyStillSilent", caseDispatchExitCode_ExitSilentlyStillSilent)
	t.Run("DispatchExitCode_ExitCodeErrorStillWins", caseDispatchExitCode_ExitCodeErrorStillWins)
}

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
