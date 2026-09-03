package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A flag must reach the tool wherever the operator puts it — and if it
// cannot, the tool must say so rather than run against something else.
//
// THE DEFECT. Go's flag package stops parsing at the first non-flag
// argument, so a single Parse over the whole argv stopped at the verb.
// In `stellarindex-migrate down 1 -dsn postgres://staging/…` the -dsn was
// never parsed, was silently dropped, and the DSN fell back to
// $STELLARINDEX_POSTGRES_DSN — so an operator dropping a migration on
// what they believed was staging dropped it on PRODUCTION, and the
// command printed success. All four verbs were affected, and so was
// -migrations.
//
// THE TEST BUILDS THE REAL BINARY AND ASSERTS ON THE HOST IT DIALS,
// because that is the only thing that separates the bug from the fix —
// both spellings otherwise "work" and both print a plausible result. A
// unit test on an internal helper could not have caught a defect that
// lives entirely in argv handling at main(). Both hostnames are
// unresolvable, so nothing connects anywhere; the name in the error is
// the evidence.
func TestMigrate_FlagsReachTheToolInEitherPosition(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stellarindex-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	const envHost = "env-host-stands-for-production.invalid"
	const flagHost = "flag-host-stands-for-staging.invalid"
	env := append(os.Environ(),
		"STELLARINDEX_POSTGRES_DSN=postgres://u:p@"+envHost+":5432/db?sslmode=disable")
	flagDSN := "postgres://u:p@" + flagHost + ":5432/staging?sslmode=disable"

	// A real migrations directory, so every invocation gets far enough to
	// DIAL. Without it the tool fails on the source first and the test
	// would pass while proving nothing.
	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(migDir); err != nil {
		t.Fatalf("migrations dir %s: %v", migDir, err)
	}
	base := []string{"-migrations", migDir}

	run := func(tail ...string) (string, int) {
		args := append(append([]string{}, base...), tail...)
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run %v: %v", args, err)
			}
			code = ee.ExitCode()
		}
		return string(out), code
	}

	// The reported trap, and the same shape on every verb: an explicit
	// -dsn AFTER the verb must be honoured, never silently replaced by
	// the environment.
	for _, tail := range [][]string{
		{"down", "1", "-dsn", flagDSN},
		{"up", "-dsn", flagDSN},
		{"status", "-dsn", flagDSN},
		{"force", "153", "-dsn", flagDSN},
	} {
		out, _ := run(tail...)
		if strings.Contains(out, envHost) {
			t.Errorf("%v DIALED THE ENV HOST %q — an explicit -dsn was ignored and the "+
				"command ran against a different database than it was asked for:\n%s",
				tail, envHost, out)
		}
		if !strings.Contains(out, flagHost) {
			t.Errorf("%v did not reach the flag's host %q:\n%s", tail, flagHost, out)
		}
	}

	// The historical placement keeps working, so no runbook or playbook
	// breaks on the fix.
	for _, tail := range [][]string{
		{"-dsn", flagDSN, "down", "1"},
		{"-dsn", flagDSN, "up"},
		{"-dsn", flagDSN, "status"},
	} {
		out, _ := run(tail...)
		if !strings.Contains(out, flagHost) {
			t.Errorf("%v did not reach the flag's host %q:\n%s", tail, flagHost, out)
		}
	}

	// A positional AFTER the flags is ambiguous — is `1` a count or a
	// flag value? It is refused rather than guessed.
	out, code := run("down", "-dsn", flagDSN, "1")
	if code == 0 {
		t.Errorf("`down -dsn … 1` exited 0; an ambiguous positional must be refused:\n%s", out)
	}
	if !strings.Contains(out, "unexpected argument") {
		t.Errorf("`down -dsn … 1` did not explain the refusal:\n%s", out)
	}

	// An unknown flag is a parse error, in either position.
	for _, tail := range [][]string{{"status", "-nope"}, {"-nope", "status"}} {
		if out, code := run(tail...); code == 0 {
			t.Errorf("%v accepted an unknown flag:\n%s", tail, out)
		}
	}

	// No flag at all: the environment is the documented fallback and must
	// keep working, or every deploy breaks.
	if out, _ := run("status"); !strings.Contains(out, envHost) {
		t.Errorf("bare `status` did not fall back to the env DSN:\n%s", out)
	}
}
