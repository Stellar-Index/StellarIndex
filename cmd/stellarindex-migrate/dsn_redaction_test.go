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
// THE DEFECT. Nothing in this binary formats the DSN itself, so a scan
// of its format strings finds nothing. The leak comes from the
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
// dependency's error text. A unit test over our own strings can only
// assert that the code we wrote behaves, and that code is fine.
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
		// that prints nothing at all would pass — and leave the
		// operator unable to see what failed, or which of the two
		// places a DSN comes from held the value that was wrong.
		keep []string
	}{
		{
			name: "unparseable DSN passed as a flag",
			args: []string{"status", "-dsn", dsn},
			keep: []string{"does not parse", "invalid URL escape", "from -dsn"},
		},
		{
			name: "unparseable DSN taken from the environment",
			env:  []string{"STELLARINDEX_POSTGRES_DSN=" + dsn},
			args: []string{"status"},
			keep: []string{"does not parse", "invalid URL escape", "from $STELLARINDEX_POSTGRES_DSN"},
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

// assertWithheld is the stronger property, and the one two rejected
// attempts at this fix did not have: the tool must not echo the argument
// AT ALL, scrubbed or otherwise.
//
// Both earlier attempts printed operator-typed text and scrubbed the
// secret back out of it, and each round the next reviewer found one more
// shape the scrubber mis-parsed — because a malformed DSN is by
// definition not parseable and every rule for "where the password ends"
// in one has a counter-example. So the harmless parts are the canary
// here: if the host or a `password=` key reached the output, the tool
// echoed the argument and only a heuristic stood between the credential
// and the deploy log.
func assertWithheld(t *testing.T, out, arg string) {
	t.Helper()
	assertNoStem(t, out)
	for _, piece := range []string{redactionHost, "password="} {
		if strings.Contains(arg, piece) && strings.Contains(out, piece) {
			t.Errorf("the tool echoed %q out of an argument it was handed — "+
				"a scrubber is all that stands between the password and the log:\n%s", piece, out)
		}
	}
}

// The first redaction was proven on a password with an unescaped `%` and
// declared closed. It was a pattern over the URL's TEXT, and the pattern
// ended the password at the FIRST `@` and refused to cross a space or a
// quote. Against the built binary that pattern produced:
//
//	…:HEAD%ZZ@TAIL@host…   printed  `<redacted>@TAIL@host`   (tail leaked)
//	…:HEAD TAIL@host…      printed the DSN untouched          (all of it)
//	…:HEAD'TAIL%ZZ@host…   printed the DSN untouched
//	…:HEAD"TAIL%ZZ@host…   printed the DSN untouched
//
// An unescaped `@` is named in the original finding as a trigger, and
// every one of these is an ordinary character in a generated password.
// Each shape is fed through BOTH routes the DSN arrives by.
//
// None of them is now rendered at all: a DSN that does not parse has no
// structure to report, so the tool names the ROUTE the value came by and
// the KIND of syntax error and says nothing of the value. The host is
// part of what is withheld, which is the deliberate trade — the operator
// is holding the string in the file they just edited, and two rounds of
// keeping the host by reading a malformed DSN with a heuristic each
// printed part of a password out of a shape the heuristic read wrong.
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
		for route, inv := range map[string]struct {
			env, args []string
			named     string
		}{
			"flag": {nil, []string{"-migrations", migDir, "status", "-dsn", dsn}, "from -dsn"},
			"env": {
				[]string{"STELLARINDEX_POSTGRES_DSN=" + dsn},
				[]string{"-migrations", migDir, "status"},
				"from $STELLARINDEX_POSTGRES_DSN",
			},
		} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				out := runMigrate(t, bin, inv.env, inv.args)
				assertWithheld(t, out, dsn)
				// The half that keeps the fix honest: a redactor that
				// blanks the message passes the check above and leaves
				// the operator with nothing to act on. What replaces the
				// value is which of the two places it came from, and
				// what kind of thing is wrong with it.
				for _, keep := range []string{inv.named, "does not parse"} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is gone — the operator cannot tell what failed:\n%s", keep, out)
					}
				}
			})
		}
	}
}

// Scrubbing the URL out of the library's error was never enough, because
// the URL is not the only place net/url repeats the password. Its REASON
// quotes the piece it choked on, and for a password holding `/`, `?` or
// `#` — every base64 secret has a fair chance of a `/` — that piece is
// the password up to that character, presented as a port:
//
//	parse "postgres://stellarindex:<redacted>@host…": invalid port ":HEAD" after host
//
// and with a `#` the URL it echoes is truncated BEFORE the `@`, so no
// pattern keyed on userinfo can even see it:
//
//	parse "postgres://stellarindex:HEAD": invalid port ":HEAD" after host
//
// No pattern can tell such a fragment from an honest port, and no
// rendering of the input can be trusted either, so the tool parses the
// DSN itself first and reports the KIND of failure in its own words —
// the library's text, fragment and all, never reaches the writer.
func TestMigrate_TheLibrarysReasonNeverRepeatsAPieceOfThePassword(t *testing.T) {
	bin, migDir := buildMigrate(t)

	for _, tc := range []struct{ name, dsn string }{
		{"slash in the password", "postgres://stellarindex:" + stemHead + "/" + stemTail + "@" + redactionHost + ":5432/stellarindex"},
		{"question mark in the password", "postgres://stellarindex:" + stemHead + "?" + stemTail + "@" + redactionHost + ":5432/stellarindex"},
		{"hash in the password", "postgres://stellarindex:" + stemHead + "#" + stemTail + "@" + redactionHost + ":5432/stellarindex"},
		// No `@` anywhere: nothing marks where the secret ends, which is
		// the shape every heuristic reading of this string got wrong.
		{"hash typed where the @ goes", "postgres://stellarindex:" + stemHead + "#" + redactionHost + ":5432/stellarindex"},
	} {
		for route, inv := range map[string]struct{ env, args []string }{
			"flag": {nil, []string{"-migrations", migDir, "up", "-dsn", tc.dsn}},
			"env":  {[]string{"STELLARINDEX_POSTGRES_DSN=" + tc.dsn}, []string{"-migrations", migDir, "up"}},
		} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				out := runMigrate(t, bin, inv.env, inv.args)
				assertWithheld(t, out, tc.dsn)
				// The reason still says what kind of thing is wrong —
				// here a fragment that reads as a port — in this
				// project's words rather than the library's.
				for _, keep := range []string{"invalid port", "stellarindex-migrate: up:", "does not parse"} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is gone — the operator cannot tell what failed:\n%s", keep, out)
					}
				}
			})
		}
	}
}

// errf was documented as the only stderr writer, and it was not: the
// flag package prints its own parse errors straight to the FlagSet's
// output, which defaulted to the raw os.Stderr, and those errors repeat
// the argument they rejected. A DSN that lands where a flag name goes —
// `-dsn` dropped, or `-$DSN_VAR` typed for `-dsn $DSN_VAR` — printed
// whole, with no redaction of any kind in front of it.
//
// Pointing that output at a scrubbing writer was the SECOND attempt at
// this, and it was rejected: the flag package cuts a rejected "flag
// name" at its first `=`, so base64 padding on a password produced an
// echo with no `@` in it for the scrubber to key on, and every fix for
// one such shape left another. The output is discarded now and the tool
// composes its own message, which names the argument's POSITION and
// never its text.
func TestMigrate_FlagParseErrorsDoNotEchoTheArgument(t *testing.T) {
	bin, migDir := buildMigrate(t)
	tail := "@" + redactionHost + ":5432/stellarindex?sslmode=disable"

	for _, tc := range []struct {
		name string
		args []string
		keep string
	}{
		{
			"a DSN where a flag name goes",
			[]string{"-postgres://stellarindex:" + stemHead + tail, "status"},
			"argument 3 is not a flag this tool defines",
		},
		{
			"the same, with a space in the password — no token pattern can bound it",
			[]string{"-postgres://stellarindex:" + stemHead + " " + stemTail + tail, "status"},
			"argument 3 is not a flag this tool defines",
		},
		{
			"after the verb, where the second parse sees it",
			[]string{"status", "-postgres://stellarindex:" + stemHead + " " + stemTail + tail},
			"argument 4 is not a flag this tool defines",
		},
		{
			"bad flag syntax",
			[]string{"-=postgres://stellarindex:" + stemHead + " " + stemTail + tail, "status"},
			"argument 3 is not a flag this tool defines (an empty flag name)",
		},
		{
			"a DSN where the subcommand goes",
			[]string{"postgres://stellarindex:" + stemHead + " " + stemTail + tail},
			"unknown subcommand",
		},
		{
			"a DSN left over after the flags",
			[]string{"down", "-migrations", migDir, "postgres://stellarindex:" + stemHead + " " + stemTail + tail},
			"unexpected argument",
		},
		// The flag package cuts a "flag name" at its FIRST `=` and echoes
		// only what precedes it. When that `=` is inside the password the
		// echo is `-postgres://user:<password so far>` — no `@`, so neither
		// the `:password@` span nor any userinfo pattern has anything to
		// match. Base64 padding is the ordinary way to get there
		// (`openssl rand -base64 32` ends in `=`), and it printed the whole
		// secret bar the padding.
		{
			"a DSN where a flag name goes, base64 padding on the password",
			[]string{"-postgres://stellarindex:" + stemHead + "==" + tail, "status"},
			"argument 3 is not a flag this tool defines (<withheld",
		},
		{
			"the same, with the = in the middle of the password",
			[]string{"-postgres://stellarindex:" + stemHead + "=" + stemTail + tail, "status"},
			"argument 3 is not a flag this tool defines (<withheld",
		},
		{
			"the same, after the verb, where the second parse sees it",
			[]string{"status", "-postgres://stellarindex:" + stemHead + "==" + tail},
			"argument 4 is not a flag this tool defines (<withheld",
		},
		{
			"the same, typed with two dashes — the echo keeps only one",
			[]string{"--postgres://stellarindex:" + stemHead + "=" + stemTail + tail, "status"},
			"argument 3 is not a flag this tool defines (<withheld",
		},
		{
			"the same, with no @ anywhere in the DSN",
			[]string{"-postgres://stellarindex:" + stemHead + "=" + stemTail + "#" + redactionHost + "/stellarindex", "status"},
			"argument 3 is not a flag this tool defines (<withheld",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runMigrate(t, bin, nil, append([]string{"-migrations", migDir}, tc.args...))
			for _, arg := range tc.args {
				assertWithheld(t, out, arg)
			}
			if !strings.Contains(out, tc.keep) {
				t.Errorf("the %q diagnostic is gone:\n%s", tc.keep, out)
			}
		})
	}
}

// A password can be held in three spellings, and the first redaction by
// value covered one. The `?password=` query parameter and the libpq
// keyword form were left to a pattern written for free text, which ends
// a value at the first space, quote or `;` — and, for a query, at the
// first `&`. A DSN with no `@` at all was covered by nothing: it has no
// `:password@` span and no userinfo for a pattern to key on. Against
// the built binary that produced:
//
//	down "…?password=HEAD TAIL"    printed  `password=<redacted> TAIL`
//	down "…?password=HEAD&TAIL&…"  printed  `password=<redacted>&TAIL&…`
//	down "password=HEAD\ TAIL"     printed  `password=<redacted> TAIL`
//	down "postgres://u:HEAD TAIL#host/db"   printed whole
//
// Each is fed through every slot that echoed a positional. None of them
// echoes one now: a positional the tool cannot use is reported by its
// POSITION and its kind, and its text is never interpolated. The
// by-value scrubber is still armed behind that — these assertions hold
// whichever of the two stops the leak, and both have to.
func TestMigrate_NoSlotEchoesAConnectionStringItWasHanded(t *testing.T) {
	bin, migDir := buildMigrate(t)
	queryDSN := func(pw string) string {
		return "postgres://" + redactionHost + ":5432/stellarindex?password=" + pw + "&sslmode=disable"
	}

	for _, tc := range []struct{ name, dsn string }{
		{"query password with a space", queryDSN(stemHead + " " + stemTail)},
		{"query password with a semicolon", queryDSN(stemHead + ";" + stemTail)},
		{"query password with a single quote", queryDSN(stemHead + "'" + stemTail)},
		{"query password with a raw ampersand", queryDSN(stemHead + "&" + stemTail)},
		// The shape the second attempt was rejected on. Any `:` before the
		// `@` — a host:port is enough — makes the userinfo reading of this
		// string run to the `@` INSIDE the query password, so the scrubber
		// cut as far as the head and printed `@` and the tail.
		{"query password with an unescaped @", queryDSN(stemHead + "@" + stemTail)},
		{"query password with two unescaped @", queryDSN(stemHead + "@x@" + stemTail)},
		{"query password with an @, no other parameter", "postgres://" + redactionHost + ":5432/stellarindex?password=" + stemHead + "@" + stemTail},
		{"query password with an @, IPv6 host", "postgres://[::1]:5432/stellarindex?password=" + stemHead + "@" + stemTail},
		{
			// Both spellings at once: cutting either one must not consume
			// the text the other is recognised by.
			"userinfo password and a query password holding an @",
			"postgres://stellarindex:HEADLESS-" + stemHead + "@" + redactionHost + ":5432/db?password=" + stemHead + "@" + stemTail,
		},
		{
			"query sslpassword with a space",
			"postgres://" + redactionHost + "/stellarindex?sslmode=verify-full&sslpassword=" + stemHead + " " + stemTail,
		},
		{
			"query sslpassword with an @",
			"postgres://" + redactionHost + ":5432/stellarindex?sslpassword=" + stemHead + "@" + stemTail,
		},
		{
			"keyword form, escaped space",
			"host=" + redactionHost + " password=" + stemHead + `\ ` + stemTail + " sslmode=disable",
		},
		{
			"keyword form, quoted with an escaped quote",
			"host=" + redactionHost + " password='" + stemHead + `\' ` + stemTail + "' sslmode=disable",
		},
		{
			"no @ anywhere, space in the password",
			"postgres://stellarindex:" + stemHead + " " + stemTail + "#" + redactionHost + "/stellarindex",
		},
		{
			// A password that opens with the marker the scrubber writes.
			"password beginning with the redaction marker",
			"postgres://stellarindex:<redacted>" + stemHead + "@" + redactionHost + "/stellarindex",
		},
	} {
		for slot, inv := range map[string]struct {
			args []string
			keep string
		}{
			"subcommand": {[]string{tc.dsn}, "unknown subcommand"},
			"down N":     {[]string{"down", tc.dsn}, "N must be a positive integer"},
			"force V":    {[]string{"force", tc.dsn}, "version must be a non-negative integer"},
			"leftover":   {[]string{"status", "-migrations", migDir, tc.dsn}, "unexpected argument"},
		} {
			t.Run(tc.name+"/"+slot, func(t *testing.T) {
				out := runMigrate(t, bin, nil, append([]string{"-migrations", migDir}, inv.args...))
				assertWithheld(t, out, tc.dsn)
				// The other half of the diagnostic: what went wrong, and
				// where. Without it a tool that prints nothing passes.
				for _, keep := range []string{inv.keep, "stellarindex-migrate"} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is gone:\n%s", keep, out)
					}
				}
			})
		}
	}

	// The remaining two routes an unusable DSN reaches the output by: the
	// unparseable-DSN path, which the tool renders itself, by BOTH ways
	// the value arrives; and the flag-name slot, which the flag package
	// echoes. Both carry the query-password-with-@ shape.
	for name, dsn := range map[string]string{
		"query password with a raw ampersand": "postgres://" + redactionHost + "/stellarindex%ZZ?password=" + stemHead + "&" + stemTail + "&sslmode=disable",
		"query password with an @":            "postgres://" + redactionHost + ":5432/db%ZZ?password=" + stemHead + "@" + stemTail,
	} {
		for route, inv := range map[string]struct {
			env, args []string
			named     string
		}{
			"unparseable, via -dsn": {nil, []string{"status", "-dsn", dsn}, "from -dsn"},
			"unparseable, via the environment": {
				[]string{"STELLARINDEX_POSTGRES_DSN=" + dsn},
				[]string{"status"},
				"from $STELLARINDEX_POSTGRES_DSN",
			},
		} {
			t.Run(name+"/"+route, func(t *testing.T) {
				out := runMigrate(t, bin, inv.env, append([]string{"-migrations", migDir}, inv.args...))
				assertWithheld(t, out, dsn)
				for _, keep := range []string{"invalid URL escape", inv.named} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is gone:\n%s", keep, out)
					}
				}
			})
		}
		t.Run(name+"/flag-name slot", func(t *testing.T) {
			out := runMigrate(t, bin, nil, []string{"-migrations", migDir, "-" + dsn, "status"})
			assertWithheld(t, out, dsn)
			if keep := "argument 3 is not a flag this tool defines"; !strings.Contains(out, keep) {
				t.Errorf("the %q diagnostic is gone:\n%s", keep, out)
			}
		})
	}
}

// A lossy transform is tested WITH its trigger. The scrubber is armed by
// the DSN the process holds, and the dangerous configuration is the
// everyday development one: user, password and database all the same
// word. If the scrubber cut that word wherever it appeared, the help
// text and every message naming the database would be mangled. It cuts
// only the `:password@` span, so the word survives everywhere else.
func TestMigrate_ScrubbingAKnownPasswordDoesNotMangleOrdinaryOutput(t *testing.T) {
	bin, migDir := buildMigrate(t)
	dsn := "postgres://stellarindex:stellarindex@" + redactionHost + ":5432/stellarindex?sslmode=disable"

	out := runMigrate(t, bin, []string{"STELLARINDEX_POSTGRES_DSN=" + dsn}, []string{"-migrations", migDir, "bogus-verb"})
	for _, keep := range []string{
		`unknown subcommand "bogus-verb"`,
		"stellarindex-migrate [-dsn DSN] [-migrations DIR] <subcommand> [args]",
		"Example: postgres://user:pass@host:5432/db?sslmode=disable",
		`export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex@localhost/stellarindex?sslmode=disable"`,
		"Path to the migrations directory",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("scrubbing mangled the usage text; %q is gone:\n%s", keep, out)
		}
	}

	out = runMigrate(t, bin, []string{"STELLARINDEX_POSTGRES_DSN=" + dsn}, []string{"-migrations", migDir, "status"})
	if want := "stellarindex-migrate: status: open migrator: failed to open database: dial tcp: lookup " + redactionHost; !strings.Contains(out, want) {
		t.Errorf("a parseable DSN must still reach the dial and report it verbatim; want %q in:\n%s", want, out)
	}
}

// A DSN with its `//` left out parses — net/url reads `postgres:u:pw@h`
// as opaque and `postgres:/u:pw@h` as a path — so the parse check let it
// through to lib/pq, which does not see a `postgres://` prefix, reads it
// as keyword/value, and fails with `missing "=" after "<the whole DSN>"`.
// The scrubber anchored on `://` and `password=`, so the password reached
// the deploy log whole. The tool refuses the shape itself, before the
// driver sees it and before `down`'s prompt prints it.
func TestMigrate_ADSNMissingItsSlashesIsRefusedWithoutEchoingIt(t *testing.T) {
	bin, migDir := buildMigrate(t)
	userinfo := "stellarindex:" + stemHead + "@" + redactionHost + ":5432/stellarindex"

	for _, tc := range []struct{ name, dsn, keep string }{
		{"no slashes", "postgres:" + userinfo, "missing the // after its scheme"},
		{"one slash", "postgres:/" + userinfo, "missing the // after its scheme"},
		{"postgresql, no slashes", "postgresql:" + userinfo, "missing the // after its scheme"},
		{"with a query", "postgres:" + userinfo + "?sslmode=disable", "missing the // after its scheme"},
		{"scheme left out", userinfo, "does not start with postgres://"},
	} {
		for route, inv := range map[string]struct {
			env, args []string
			from      string
		}{
			"flag":        {nil, []string{"-migrations", migDir, "up", "-dsn", tc.dsn}, "from -dsn"},
			"env":         {[]string{"STELLARINDEX_POSTGRES_DSN=" + tc.dsn}, []string{"-migrations", migDir, "up"}, "from $STELLARINDEX_POSTGRES_DSN"},
			"down prompt": {[]string{"STELLARINDEX_POSTGRES_DSN=" + tc.dsn}, []string{"-migrations", migDir, "-i-know", "down", "1"}, "from $STELLARINDEX_POSTGRES_DSN"},
		} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				out := runMigrate(t, bin, inv.env, inv.args)
				assertWithheld(t, out, tc.dsn)
				if strings.Contains(out, "open migrator") || strings.Contains(out, "not a TTY") {
					t.Errorf("the DSN got past the check to the driver or the prompt:\n%s", out)
				}
				for _, keep := range []string{tc.keep, inv.from} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is missing:\n%s", keep, out)
					}
				}
			})
		}
	}

	// The libpq socket form has no host and must still reach the driver.
	socket := "postgres:///stellarindex?host=/nonexistent-socket-dir-for-test"
	out := runMigrate(t, bin, []string{"STELLARINDEX_POSTGRES_DSN=" + socket}, []string{"-migrations", migDir, "status"})
	if !strings.Contains(out, "open migrator") {
		t.Errorf("a hostless socket DSN was refused before the driver:\n%s", out)
	}
}

// THE DEFECT. `down` calls m.Steps(-n) — a destructive rollback —
// with no confirmation prompt, no -yes/-force flag and no TTY guard: an
// operator who fat-fingers `down` (or a script that inherits the wrong
// $STELLARINDEX_POSTGRES_DSN) drops production migrations with no
// chance to notice, and the command prints success. `up` is the only
// verb any deploy pipeline runs (deploy-binary.yml); `down` is a manual,
// break-glass command, so the safe default is to ask first and to
// refuse — never guess — when nothing can answer the prompt, matching
// scripts/dev/cut-release.sh's rule for the same class of prompt.
//
// THE TEST BUILDS THE REAL BINARY, because the gate has to run before
// `newMigrator` ever opens a connection: it asserts on WHICH failure
// comes back, not just that one occurs, and only the real dispatch in
// main() proves the gate runs first.
func TestMigrate_DownRefusesWithoutConfirmationOnNonTTYStdin(t *testing.T) {
	bin := buildMigrateBinary(t)

	// Port 1 on loopback refuses immediately with no DNS or connect
	// timeout, so the test only takes real time if the gate is missing
	// and the tool goes on to actually try to connect.
	const dsn = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"

	cmd := exec.Command(bin, "-dsn", dsn, "-i-know", "down", "1")
	// exec.Command leaves Stdin nil, which os/exec wires to /dev/null —
	// guaranteed non-interactive, exactly the shape this gate must
	// refuse rather than guess on.
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit refusing the unconfirmed rollback, got success:\n%s", out)
	}

	got := string(out)
	if !strings.Contains(got, "not a TTY") || !strings.Contains(got, "-yes") {
		t.Fatalf("expected the confirmation refusal naming -yes and the non-TTY stdin, got:\n%s", got)
	}
	// The defect this replaces: on unfixed code there is no gate, so the
	// tool goes straight to newMigrator and this message never appears —
	// this asserts the connection was never attempted.
	if strings.Contains(got, "connect") || strings.Contains(got, "refused") || strings.Contains(got, "open migrator") {
		t.Fatalf("rollback should have been refused before any connection attempt, got:\n%s", got)
	}
}

// -yes must actually skip the prompt and let the command proceed to the
// real work, not just always fail differently.
func TestMigrate_DownYesSkipsConfirmationAndReachesTheMigrator(t *testing.T) {
	bin := buildMigrateBinary(t)

	const dsn = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"

	cmd := exec.Command(bin, "-dsn", dsn, "-yes", "-i-know", "down", "1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit (connection refused), got success:\n%s", out)
	}

	got := string(out)
	if strings.Contains(got, "not a TTY") || strings.Contains(got, "-i-know") {
		t.Fatalf("-yes -i-know should have passed both gates, got:\n%s", got)
	}
	if !strings.Contains(got, "open migrator") {
		t.Fatalf("expected the tool to go on and open the migrator, got:\n%s", got)
	}
}

// -yes only skips the prompt; it is what every non-interactive caller
// passes, so it cannot also be the acknowledgement that the rollback may
// discard data. r1 production reaches Postgres at 127.0.0.1
// (14-stellarindex-services.yml), so this is the production-shape DSN
// and the refusal must not depend on the host looking remote.
func TestMigrate_DownRefusesWithoutIKnowOnProductionShapeDSN(t *testing.T) {
	bin := buildMigrateBinary(t)

	for _, dsn := range []string{
		"postgres://stellarindex:p@127.0.0.1:1/stellarindex?sslmode=disable",
		"postgres://stellarindex:p@localhost:1/stellarindex?sslmode=disable",
		"postgres://stellarindex:p@/stellarindex?host=db.invalid&port=1",
		"postgres://stellarindex:p@db.invalid:1/stellarindex?sslmode=disable",
	} {
		cmd := exec.Command(bin, "-dsn", dsn, "-yes", "down", "1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("down -yes without -i-know exited 0:\n%s", out)
		}
		got := string(out)
		if !strings.Contains(got, "refusing") || !strings.Contains(got, "-i-know") {
			t.Fatalf("expected a refusal naming -i-know, got:\n%s", got)
		}
		if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "db.invalid") ||
			strings.Contains(got, "localhost") || strings.Contains(got, "open migrator") {
			t.Fatalf("down without -i-know went on to dial the database:\n%s", got)
		}
	}
}

// -i-know is an acknowledgement, not a prompt skip: on a non-TTY stdin
// the confirmation gate still refuses when -yes is absent.
func TestMigrate_DownIKnowAloneDoesNotSkipConfirmation(t *testing.T) {
	bin := buildMigrateBinary(t)

	const dsn = "postgres://u:p@db.invalid:1/db?sslmode=disable"
	out, err := exec.Command(bin, "-dsn", dsn, "-i-know", "down", "1").CombinedOutput()
	if err == nil {
		t.Fatalf("expected the non-TTY confirmation refusal, got success:\n%s", out)
	}
	if got := string(out); !strings.Contains(got, "not a TTY") {
		t.Fatalf("-i-know must not skip the confirmation prompt, got:\n%s", got)
	}
}

// buildMigrateBinary compiles the real binary once per test into a temp
// dir, matching the other black-box tests in this package.
func buildMigrateBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stellarindex-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// An empty or repeated -dsn must be refused before anything is dialled.
// The environment DSN stands in for production: an explicit `-dsn ""`
// (the quoted expansion of an unset variable) must not fall through to it,
// and a second -dsn must not silently replace the first. Both hosts are
// unresolvable, so the host named in the output is the evidence of which
// database the tool would have written to.
func TestMigrate_EmptyOrRepeatedDSNIsRefusedBeforeAnyDial(t *testing.T) {
	bin, migDir := buildMigrate(t)

	const envHost = "env-host-stands-for-production.invalid"
	const hostA = "flag-host-a.invalid"
	const hostB = "flag-host-b.invalid"
	envDSN := "STELLARINDEX_POSTGRES_DSN=postgres://u:p@" + envHost + ":5432/db?sslmode=disable"
	dsnA := "postgres://u:p@" + hostA + ":5432/a?sslmode=disable"
	dsnB := "postgres://u:p@" + hostB + ":5432/b?sslmode=disable"

	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"-migrations", migDir}, args...)...)
		cmd.Env = append(os.Environ(), envDSN)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return string(out), 0
		case errors.As(err, &ee):
			return string(out), ee.ExitCode()
		default:
			t.Fatalf("run %v: %v", args, err)
			return "", 0
		}
	}

	// Controls: the instrument can see each host, so an absent host below
	// means "not dialled", not "not reported".
	if out, _ := run("status"); !strings.Contains(out, envHost) {
		t.Fatalf("no -dsn: expected the env host to be dialled, got:\n%s", out)
	}
	if out, _ := run("-dsn", dsnA, "status"); !strings.Contains(out, hostA) || strings.Contains(out, envHost) {
		t.Fatalf("-dsn once: expected only %s to be dialled, got:\n%s", hostA, out)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty before verb", []string{"-dsn", "", "status"}, "empty"},
		{"empty with equals", []string{"-dsn=", "status"}, "empty"},
		{"empty after verb", []string{"status", "-dsn", ""}, "empty"},
		{"blank value", []string{"-dsn", "  ", "up"}, "empty"},
		{"empty on down -yes", []string{"-dsn", "", "-yes", "-i-know", "down", "2"}, "empty"},
		{"repeated before verb", []string{"-dsn", dsnA, "-dsn", dsnB, "status"}, "more than once"},
		{"repeated across verb", []string{"-dsn", dsnA, "status", "-dsn", dsnB}, "more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (usage error)\n%s", code, out)
			}
			for _, h := range []string{envHost, hostA, hostB} {
				if strings.Contains(out, h) {
					t.Errorf("dialled %s — the flag must be refused before any connection:\n%s", h, out)
				}
			}
			if !strings.Contains(out, "-dsn") || !strings.Contains(out, tc.want) {
				t.Errorf("diagnostic must name -dsn and say %q, got:\n%s", tc.want, out)
			}
		})
	}
}

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
		{"down", "1", "-dsn", flagDSN, "-yes", "-i-know"},
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
		{"-dsn", flagDSN, "-yes", "-i-know", "down", "1"},
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
