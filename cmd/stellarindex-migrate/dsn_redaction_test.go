package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dsnSentinel stands in for the production Postgres password. The
// trailing `%ZZ` is not decoration: an unescaped `%` is what makes
// net/url refuse the DSN, and a generated password containing one is
// the ordinary operator mistake that reaches this path. Everything
// before it says what the value is, so a reviewer reading a failure
// cannot mistake it for a real credential.
const dsnSentinel = "PLACEHOLDER-NOT-A-REAL-SECRET-%ZZ"

// The migration tool is handed the production DSN — password inline —
// on every deploy, and it prints its failures to stderr, where the
// deploy job's log, journald, promtail and Loki all pick them up.
//
// THE DEFECT. Nothing in this binary formatted the DSN itself, so an
// audit of its format strings found nothing. The leak came from the
// library: golang-migrate rejects an unparseable database URL with
// net/url's *url.Error, which renders as `parse "<the whole URL>": …`,
// and `newMigrator` wrapped that with %w. A password with an unescaped
// `%`, `#` or space — none of them exotic in a generated credential —
// therefore printed the live production password to the deploy log, in
// full, on the very run where an operator is most likely to copy the
// output into a ticket.
//
// The second case is ours rather than the library's, and is the reason
// the fix belongs at the point the process writes rather than at the
// one call site: `down <dsn>` (a paste into the slot where N goes)
// echoes the argument back verbatim through an unrelated error path.
//
// THE TEST BUILDS AND RUNS THE REAL BINARY, because the leak lives in a
// dependency's error text. A unit test over our own strings is exactly
// the audit that missed it — it can only assert that the code we wrote
// behaves, and the code we wrote was already fine.
func TestMigrate_FatalOutputNeverCarriesTheDSNPassword(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stellarindex-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// A real migrations directory, so every invocation gets past the
	// source and as far as the DATABASE URL. Without it the tool fails on
	// the source first and this test would pass while proving nothing.
	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(migDir); err != nil {
		t.Fatalf("migrations dir %s: %v", migDir, err)
	}

	const host = "redaction-target.invalid"
	dsn := "postgres://stellarindex:" + dsnSentinel + "@" + host + ":5432/stellarindex?sslmode=disable"

	run := func(env []string, tail ...string) string {
		args := append([]string{"-migrations", migDir}, tail...)
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run %v: %v", args, err)
			}
		}
		return string(out)
	}

	for _, tc := range []struct {
		name string
		env  []string
		args []string
		// keep is the diagnostic that must SURVIVE. Without it a fix
		// that prints nothing at all, or that redacts the whole
		// message, would pass — and leave the operator unable to see
		// which database the failed migration was aimed at.
		keep []string
	}{
		{
			name: "unparseable DSN passed as a flag",
			args: []string{"status", "-dsn", dsn},
			keep: []string{host, "invalid URL escape"},
		},
		{
			name: "unparseable DSN taken from the environment",
			env:  []string{"STELLARINDEX_POSTGRES_DSN=" + dsn},
			args: []string{"status"},
			keep: []string{host, "invalid URL escape"},
		},
		{
			name: "DSN pasted into the slot where the step count goes",
			env:  []string{"STELLARINDEX_POSTGRES_DSN=" + dsn},
			args: []string{"down", dsn},
			keep: []string{"positive integer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := run(tc.env, tc.args...)

			if strings.Contains(out, dsnSentinel) {
				t.Errorf("the DSN password reached stderr — every deploy log, journald and Loki now hold it:\n%s", out)
			}
			// The password's distinctive stem, in case a partial
			// redaction cut only the tail off.
			if strings.Contains(out, "PLACEHOLDER-NOT-A-REAL-SECRET") {
				t.Errorf("part of the DSN password reached stderr:\n%s", out)
			}
			for _, keep := range tc.keep {
				if !strings.Contains(out, keep) {
					t.Errorf("redaction ate the %q diagnostic — the operator cannot tell what failed:\n%s", keep, out)
				}
			}
		})
	}
}

// Two halves of one stand-in password, so a test can tell WHICH part of
// a credential survived. Both say what they are; neither has the shape
// of a real key.
const (
	stemHead = "PLACEHOLDER-HEAD-NOT-REAL"
	stemTail = "PLACEHOLDER-TAIL-NOT-REAL"
)

// redactionHost is the host every DSN below points at. `.invalid` never
// resolves, so a DSN that does parse fails at the dial without touching
// a database.
const redactionHost = "redaction-target.invalid"

// buildMigrate compiles the real binary. See the comment on
// TestMigrate_FatalOutputNeverCarriesTheDSNPassword for why nothing less
// than the built binary proves anything here.
func buildMigrate(t *testing.T) (bin, migDir string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "stellarindex-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(migDir); err != nil {
		t.Fatalf("migrations dir %s: %v", migDir, err)
	}
	return bin, migDir
}

// runMigrate returns everything the process wrote, stdout and stderr
// together: a deploy log does not keep them apart, so neither may carry
// the credential. The inherited DSN is cleared first so a developer's
// own environment cannot stand in for the one under test.
func runMigrate(t *testing.T, bin string, env, args []string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "STELLARINDEX_POSTGRES_DSN="), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return string(out)
}

// assertNoStem fails if either half of the stand-in password is in out.
func assertNoStem(t *testing.T, out string) {
	t.Helper()
	for _, stem := range []string{stemHead, stemTail} {
		if strings.Contains(out, stem) {
			t.Errorf("%s of the DSN password reached the output:\n%s", stem, out)
		}
	}
}

// The first redaction was proven on a password with an unescaped `%` and
// declared closed. It was a pattern over the URL's TEXT, and the pattern
// ended the password at the FIRST `@` and refused to cross a space or a
// quote. Reproduced against the built binary before this fix:
//
//	…:HEAD%ZZ@TAIL@host…   printed  `<redacted>@TAIL@host`   (tail leaked)
//	…:HEAD TAIL@host…      printed the DSN untouched          (all of it)
//	…:HEAD'TAIL%ZZ@host…   printed the DSN untouched
//	…:HEAD"TAIL%ZZ@host…   printed the DSN untouched
//
// An unescaped `@` is named in the original finding as a trigger, and
// every one of these is an ordinary character in a generated password.
// Each shape is fed through BOTH routes the DSN arrives by.
func TestMigrate_PasswordShapesThePatternStoppedShortOn(t *testing.T) {
	bin, migDir := buildMigrate(t)

	for _, tc := range []struct{ name, pw string }{
		{"unescaped @ in an unparseable password", stemHead + "%ZZ@" + stemTail},
		{"two unescaped @ in an unparseable password", stemHead + "@mid@" + stemTail + "%ZZ"},
		{"space", stemHead + " " + stemTail},
		{"single quote", stemHead + "'" + stemTail + "%ZZ"},
		{"double quote", stemHead + `"` + stemTail + "%ZZ"},
		{"backslash then double quote", stemHead + `\"` + stemTail + "%ZZ"},
	} {
		dsn := "postgres://stellarindex:" + tc.pw + "@" + redactionHost + ":5432/stellarindex?sslmode=disable"
		for route, inv := range map[string]struct{ env, args []string }{
			"flag": {nil, []string{"-migrations", migDir, "status", "-dsn", dsn}},
			"env":  {[]string{"STELLARINDEX_POSTGRES_DSN=" + dsn}, []string{"-migrations", migDir, "status"}},
		} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				out := runMigrate(t, bin, inv.env, inv.args)
				assertNoStem(t, out)
				// The half that keeps the fix honest: a redactor that
				// blanks the message passes the check above and leaves
				// the operator unable to see which database it was.
				if !strings.Contains(out, redactionHost) {
					t.Errorf("redaction ate the host — the operator cannot tell what was dialled:\n%s", out)
				}
			})
		}
	}
}
