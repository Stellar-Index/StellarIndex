package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// hostilePasswords are the characters that make a DSN unparseable, which
// is what sends it into an error message in the first place. Each holds
// both stems so a partial cut is visible.
var hostilePasswords = map[string]string{
	"plain":        sentinel + tailStem,
	"at sign":      sentinel + "@" + tailStem,
	"space":        sentinel + " " + tailStem,
	"single quote": sentinel + "'" + tailStem,
	"double quote": sentinel + `"` + tailStem,
	"backslash":    sentinel + `\` + tailStem,
	"slash":        sentinel + "/" + tailStem,
	"question":     sentinel + "?" + tailStem,
	"hash":         sentinel + "#" + tailStem,
	"percent":      sentinel + "%ZZ" + tailStem,
	"newline":      sentinel + "\n" + tailStem,
	"everything":   sentinel + ` @'"\/?#%ZZ` + tailStem,
}

func assertNoStems(t *testing.T, in, got string) {
	t.Helper()
	for _, stem := range []string{sentinel, tailStem} {
		if strings.Contains(got, stem) {
			t.Errorf("kept %s:\n  in:  %s\n  out: %s", stem, in, got)
		}
	}
}

// Known finds the secret by VALUE, so it has to hold for every way Go
// repeats a string it was given — bare, %q, and cut short the way the
// flag package cuts a "flag name" at its first `=` — and for every
// character, including the whitespace no pattern over unquoted text can
// bound.
func TestKnownCutsThePasswordHoweverItIsRepeated(t *testing.T) {
	for name, pw := range hostilePasswords {
		dsn := "postgres://stellarindex:" + pw + "@db.example.invalid:5432/app?sslmode=disable"
		for shape, text := range map[string]string{
			"bare":      "flag provided but not defined: -" + dsn,
			"quoted":    fmt.Sprintf("unknown subcommand %q", dsn),
			"truncated": "flag provided but not defined: -" + dsn[:strings.LastIndex(dsn, "=")],
			"as a flag": "bad flag syntax: -dsn==" + dsn,
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				got := Known(text, "unrelated", "", dsn)
				assertNoStems(t, text, got)
				if !strings.Contains(got, "stellarindex:<redacted>@db.example.invalid:5432/app") {
					t.Errorf("lost the user, host or database:\n  in:  %s\n  out: %s", text, got)
				}
				if twice := Known(got, dsn); twice != got {
					t.Errorf("second pass changed the output:\n  once:  %s\n  twice: %s", got, twice)
				}
			})
		}
	}
}

// The destructive branch, WITH its trigger. The scrubber is armed by a
// held DSN, and the everyday development DSN uses one word for the
// user, the password and the database. A scrubber that cut the bare
// password would rewrite every message naming the database; this one
// cuts only the `:password@` span, so text that merely contains the
// word must come back byte-identical.
func TestKnownDoesNotMangleTextThatMerelyContainsThePassword(t *testing.T) {
	dsn := "postgres://app:app@localhost:5432/app?sslmode=disable"
	for _, in := range []string{
		`pq: database "app" does not exist`,
		`pq: password authentication failed for user "app"`,
		"migrated to version 412 (dirty=false)",
		"dial tcp 127.0.0.1:5432: connect: connection refused",
		"Example: postgres://user:pass@host:5432/db?sslmode=disable — see app docs",
		"",
	} {
		want := Credentials(in) // the pattern pass still runs; the VALUE pass must add nothing
		if got := Known(in, dsn); got != want {
			t.Errorf("Known changed text that does not repeat the DSN:\n  in:   %s\n  got:  %s\n  want: %s", in, got, want)
		}
	}
	if got, want := Known("open "+dsn, dsn), "open postgres://app:<redacted>@localhost:5432/app?sslmode=disable"; got != want {
		t.Errorf("Known on the DSN itself\n  got:  %s\n  want: %s", got, want)
	}
}

// Values that are not URL-form connection strings arm nothing.
func TestKnownIgnoresValuesWithNoPasswordSpan(t *testing.T) {
	const in = "status: current version: 412 (dirty=false) for user@host at 12:30"
	for _, v := range []string{
		"", "up", "-migrations", "/usr/local/share/migrations", "://x:y@z",
		"postgres://db.example.invalid:5432/app",  // no userinfo
		"postgres://user@db.example.invalid/app",  // no password
		"postgres://user:@db.example.invalid/app", // empty password
		"host=h user=u password=x",                // keyword form: the pattern pass owns it
	} {
		if got := Known(in, v); got != in {
			t.Errorf("Known(%q) armed on %q:\n  out: %s", in, v, got)
		}
	}
}

// ParseFailure runs the real parser over each hostile DSN and renders
// what it returned. The reason's quoted fragment is the leak this exists
// for: `invalid port ":<password up to the slash>" after host`.
func TestParseFailureRepeatsNothingOfThePassword(t *testing.T) {
	failed := 0
	for name, pw := range hostilePasswords {
		dsn := "postgres://stellarindex:" + pw + "@db.example.invalid:5432/app?sslmode=disable"
		_, err := url.Parse(dsn)
		if err == nil {
			continue // parseable: the migration tool never renders it
		}
		failed++
		t.Run(name, func(t *testing.T) {
			got := ParseFailure(dsn, err)
			assertNoStems(t, err.Error(), got)
			if want := "postgres://stellarindex:<redacted>@db.example.invalid:5432/app?sslmode=disable"; !strings.Contains(got, want) {
				t.Errorf("lost the connection string's diagnostic half %q:\n  out: %s", want, got)
			}
			// The reason's words survive; only its quoted echo is withheld.
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Fatalf("expected a *url.Error, got %T", err)
			}
			words, _, _ := strings.Cut(ue.Err.Error(), `"`)
			if !strings.HasPrefix(got, words) {
				t.Errorf("lost the reason %q:\n  out: %s", words, got)
			}
		})
	}
	// Self-accounting: if net/url ever starts accepting these, the loop
	// above passes over nothing and says so here instead of going green.
	if failed < 6 {
		t.Fatalf("only %d of %d hostile DSNs failed to parse — this test is no longer exercising the renderer", failed, len(hostilePasswords))
	}
}

func TestKnownConnString(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:" + sentinel + "@h:5432/db":                 "postgres://u:<redacted>@h:5432/db",
		"postgres://u:" + sentinel + " " + tailStem + "@h/db":     "postgres://u:<redacted>@h/db",
		"postgres://h/db?password=" + sentinel + " x&sslmode=off": "postgres://h/db?password=<redacted>&sslmode=off",
		"postgres://h/db?sslmode=off&SSLPassword=" + sentinel:     "postgres://h/db?sslmode=off&SSLPassword=<redacted>",
		"host=h user=u password=" + sentinel + " sslmode=require": "host=h user=u password=<redacted> sslmode=require",
		// No `@`: a numeric port is believed, anything else is a secret
		// with no end marker and takes the rest of the string with it.
		"postgres://h:5432/db%ZZ":                  "postgres://h:5432/db%ZZ",
		"postgres://h:5432":                        "postgres://h:5432",
		"postgres://[::1]:5432/db%ZZ":              "postgres://[::1]:5432/db%ZZ",
		"postgres://u:" + sentinel + "#h:5432/db":  "postgres://u:<redacted>",
		"postgres://u:" + sentinel + "/x#h:5432":   "postgres://u:<redacted>",
		"postgres://u:12345#" + sentinel + "/db":   "postgres://u:<redacted>",
		"postgres://user@db.example.invalid/db%ZZ": "postgres://user@db.example.invalid/db%ZZ",
	} {
		if got := knownConnString(in); got != want {
			t.Errorf("knownConnString(%q)\n  got:  %s\n  want: %s", in, got, want)
		}
	}
}
