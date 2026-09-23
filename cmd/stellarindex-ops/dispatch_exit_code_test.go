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

// TestDispatchExitCode_HelpIsNotAFailure pins F070/K055: every subcommand
// flag.FlagSet uses flag.ContinueOnError, so `-h`/`-help` on a subcommand
// surfaces as flag.ErrHelp from fs.Parse, propagated up through the
// handler's Run. Before this fix that fell through to the generic error
// branch, printing "<subcommand>: flag: help requested" and returning exit
// code 1 — indistinguishable from a real failure for a scripted caller
// that asked for help.
func TestDispatchExitCode_HelpIsNotAFailure(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", flag.ErrHelp, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (help is not a failure)", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty — the flag package already printed usage via fs.Output()", stderr.String())
	}
}

// TestDispatchExitCode_WrappedHelpIsNotAFailure covers a handler that wraps
// flag.ErrHelp (fmt.Errorf("%w", ...)) rather than returning it bare —
// errors.Is must still unwrap it.
func TestDispatchExitCode_WrappedHelpIsNotAFailure(t *testing.T) {
	var stderr bytes.Buffer
	wrapped := errors.Join(flag.ErrHelp)
	code := dispatchExitCode("backfill", wrapped, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (wrapped help is still not a failure)", code)
	}
}

// TestDispatchExitCode_RealErrorsStillFail guards against a too-broad fix:
// an ordinary handler error must still print the "<subcommand>: <err>"
// prefix and return 1.
func TestDispatchExitCode_RealErrorsStillFail(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", errors.New("boom"), &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backfill: boom") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "backfill: boom")
	}
}

// TestDispatchExitCode_ExitSilentlyStillSilent guards the existing
// ErrExitSilently convention (handler already printed its own message).
func TestDispatchExitCode_ExitSilentlyStillSilent(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("backfill", opsutil.ErrExitSilently, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestDispatchExitCode_ExitCodeErrorStillWins guards the existing
// opsutil.ExitCodeError convention (a specific non-1 exit code).
func TestDispatchExitCode_ExitCodeErrorStillWins(t *testing.T) {
	var stderr bytes.Buffer
	code := dispatchExitCode("reconcile-balances", &opsutil.ExitCodeError{Code: 3, Err: errors.New("3 mismatches")}, &stderr)

	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "reconcile-balances: 3 mismatches") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "reconcile-balances: 3 mismatches")
	}
}

// TestEverySubcommandHelpExitsZero drives every help spelling through
// every dispatch-table entry and the real exit-code chokepoint. Namespace
// verbs (discovery, supply, archive-completeness) and the positional verb
// rpc-probe read args[1] before any FlagSet exists, so their help request
// never reaches fs.Parse and must be answered with flag.ErrHelp directly.
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
