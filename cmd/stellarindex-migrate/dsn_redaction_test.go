package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dsnSentinel stands in for the production Postgres password. The trailing `%ZZ` is
// load-bearing: an unescaped `%` makes net/url refuse the DSN, and a generated password
// containing one is the ordinary operator mistake that reaches this path.
const dsnSentinel = "PLACEHOLDER-NOT-A-REAL-SECRET-%ZZ"

// The migration tool gets the production DSN on every deploy and prints failures to stderr, which deploy logs collect. The leak
// comes from golang-migrate's net/url error quoting the whole URL, and from `down <dsn>` echoing its argument. The test runs the
// REAL BINARY because the leak lives in a dependency's error text.
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
		// keep is the diagnostic that must SURVIVE; otherwise printing nothing would pass and leave the operator blind.
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

// runMigrate returns stdout and stderr together, since a deploy log keeps neither apart. The inherited DSN is cleared so the
// developer's environment cannot stand in for the one under test.
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

// assertWithheld is the strong property: the argument must not be echoed AT ALL, scrubbed or otherwise. The harmless parts (host,
// `password=` key) are the canary: if they appear, only a heuristic stood between the credential and the log.
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

// A pattern over the URL's TEXT cannot be trusted: one that ends the password at the FIRST `@`,
// or refuses to cross a space or quote, leaks the tail or prints the DSN untouched for shapes
// like:
//
//	...:HEAD%ZZ@TAIL@host...   (tail leaked after the redaction)
//	...:HEAD TAIL@host...      (printed whole)
//	...:HEAD'TAIL%ZZ@host...   (printed whole)
//	...:HEAD"TAIL%ZZ@host...   (printed whole)
//
// Each is an ordinary character in a generated password, and each shape is fed through BOTH
// routes the DSN arrives by. None is rendered: a DSN that does not parse has no structure to
// report, so the tool names the ROUTE and the KIND of syntax error, never the value or host.
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
				// A redactor that blanks the whole message would pass the check above and
				// leave the operator nothing to act on, so the output must name which of
				// the two places the value came from and what kind of thing is wrong.
				for _, keep := range []string{inv.named, "does not parse"} {
					if !strings.Contains(out, keep) {
						t.Errorf("the %q diagnostic is gone — the operator cannot tell what failed:\n%s", keep, out)
					}
				}
			})
		}
	}
}

// Scrubbing the URL out of the library's error is not enough: net/url's REASON quotes the piece
// it choked on, and for a password holding `/`, `?` or `#` that piece is the password up to that
// character, presented as a port:
//
//	parse "postgres://stellarindex:<redacted>@host…": invalid port ":HEAD" after host
//
// With a `#` the echoed URL is truncated BEFORE the `@`, so no pattern keyed on userinfo can see
// it:
//
//	parse "postgres://stellarindex:HEAD": invalid port ":HEAD" after host
//
// No pattern can tell such a fragment from an honest port, so the tool parses the DSN itself
// and reports the KIND of failure in its own words; the library's text never reaches the writer.
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

// errf must be the only stderr writer, but the flag package echoes rejected arguments to the FlagSet output. A DSN landing in a
// flag-name slot would print whole, so that output is discarded and the tool names the argument's POSITION, never its text.
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
		// The flag package cuts a "flag name" at its FIRST `=` and echoes what precedes it. A `=` inside the password (base64 padding)
		// leaves an echo with no `@`, so no userinfo pattern matches.
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

// A password can be held in three spellings: userinfo, the `?password=` query parameter and the
// libpq keyword form. A pattern written for free text ends a value at the first space, quote or
// `;` (for a query, the first `&`), and a DSN with no `@` has no userinfo to key on, so these
// leak a TAIL or the whole value:
//
//	down "...?password=HEAD TAIL"
//	down "...?password=HEAD&TAIL&..."
//	down "password=HEAD\ TAIL"
//	down "postgres://u:HEAD TAIL#host/db"
//
// Each is fed through every slot that echoed a positional. A positional the tool cannot use is
// reported by its POSITION and kind, never its text; the by-value scrubber stays armed behind
// that, and these assertions hold whichever of the two stops the leak.
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
		// Any `:` before the `@` (a host:port suffices) makes the userinfo reading run to the `@` INSIDE the query password, so a
		// scrubber cut at the head and printed `@` and the tail.
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

	// The remaining routes an unusable DSN reaches output by: the tool-rendered unparseable-DSN path (both ways the value arrives)
	// and the flag-name slot the flag package echoes, each with the query-password-with-@ shape.
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

// A lossy transform is tested WITH its trigger: when user, password and database are the same word, the scrubber must cut only
// the `:password@` span, not mangle help text and messages naming the database.
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

// A DSN missing its `//` parses (net/url reads it as opaque or a path), so lib/pq reads it as keyword/value and errors with
// `missing "=" after "<the whole DSN>"`. The tool must refuse that shape itself, before the driver and before `down`'s prompt.
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

// `down` runs a destructive m.Steps(-n), so it must prompt, and refuse (never guess) when nothing can answer. The test builds the
// REAL BINARY because the gate must run before `newMigrator` opens a connection, and only main()'s dispatch proves that.
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

// -yes only skips the prompt, so it cannot also acknowledge that the rollback may discard data. r1 production reaches Postgres at
// 127.0.0.1, so the refusal must not depend on the host looking remote.
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

// An empty or repeated -dsn must be refused before any dial: `-dsn ""` (an unset variable's expansion) must not fall back to the
// environment DSN standing in for production, nor a second -dsn replace the first. Unresolvable hosts show which was used.
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

// A flag must reach the tool wherever the operator puts it, else the tool must say so. flag stops at the first non-flag, so in
// `down 1 -dsn staging` a single Parse drops -dsn and falls back to the production env DSN. The test runs the REAL BINARY and
// checks the host dialled (both hostnames unresolvable, so the name in the error is the evidence).
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
