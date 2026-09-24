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
	// `=` is what the flag package cuts a "flag name" at, and base64
	// padding puts one at the END of a generated password, so the echo
	// is the whole secret bar its padding, with no `@` after it.
	"equals":         sentinel + "=" + tailStem,
	"base64 padding": sentinel + tailStem + "==",
	"everything":     sentinel + ` @'"\/?#%ZZ=` + tailStem,
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
		// What the flag package echoes for `-<dsn>`: the argument up to its
		// FIRST `=`. That lands inside the password whenever the password
		// holds one, and in `sslmode=` otherwise.
		truncated := dsn[:strings.Index(dsn, "=")]
		for shape, tc := range map[string]struct{ text, held string }{
			"bare":      {"flag provided but not defined: -" + dsn, dsn},
			"quoted":    {fmt.Sprintf("unknown subcommand %q", dsn), dsn},
			"truncated": {"flag provided but not defined: -" + truncated, "-" + dsn},
			// The echo drops one of the dashes the argument arrived with,
			// so the held value does not open the way its echo does.
			"truncated, two dashes":               {"flag provided but not defined: -" + truncated, "--" + dsn},
			"truncated, then quoted":              {fmt.Sprintf("no such flag %q", truncated), dsn},
			"as a flag":                           {"bad flag syntax: -dsn==" + dsn, dsn},
			"as a flag, held as the argv element": {"bad flag syntax: -dsn==" + dsn, "-dsn==" + dsn},
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				text := tc.text
				got := Known(text, "unrelated", "", tc.held)
				assertNoStems(t, text, got)
				// An echo cut short inside the password has no host left
				// to keep; every other shape must keep all three.
				keep := "stellarindex:<redacted>@db.example.invalid:5432/app"
				if !strings.Contains(text, "@db.example.invalid") {
					keep = "postgres://stellarindex:<redacted>"
				}
				if !strings.Contains(got, keep) {
					t.Errorf("lost the user, host or database (%q):\n  in:  %s\n  out: %s", keep, text, got)
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
// cuts it only inside the `:password@` span or directly after the text
// it was held behind, so text that merely contains the word must come
// back byte-identical.
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
	// The same word again in the other spellings a secret is held in. Each
	// arms its own anchor — `password=`, or `postgres://app:` — and none
	// may touch text that does not repeat it.
	for _, held := range []string{
		"postgres://localhost:5432/app?password=app&sslmode=disable",
		"host=localhost user=app password=app dbname=app",
		"postgres://app:app#localhost/app", // no `@`: the secret runs to the end
	} {
		for _, in := range []string{
			`pq: database "app" does not exist`,
			`pq: password authentication failed for user "app"`,
			"app: applied 3 migrations to postgres://localhost:5432/app",
		} {
			if got := Known(in, held); got != in {
				t.Errorf("Known(%q) changed text that does not repeat it:\n  in:  %s\n  got: %s", held, in, got)
			}
		}
	}
	if got, want := Known("open "+dsn, dsn), "open postgres://app:<redacted>@localhost:5432/app?sslmode=disable"; got != want {
		t.Errorf("Known on the DSN itself\n  got:  %s\n  want: %s", got, want)
	}
}

// Values that hold no secret arm nothing, and one that does arms nothing
// on text that does not repeat it.
func TestKnownIgnoresValuesWithNoPasswordSpan(t *testing.T) {
	const in = "status: current version: 412 (dirty=false) for user@host at 12:30"
	for _, v := range []string{
		"", "up", "-migrations", "/usr/local/share/migrations", "://x:y@z",
		"postgres://db.example.invalid:5432/app",  // no userinfo
		"postgres://user@db.example.invalid/app",  // no password
		"postgres://user:@db.example.invalid/app", // empty password
		"host=h user=u password=x",                // keyword form: held, but not repeated here
	} {
		if got := Known(in, v); got != in {
			t.Errorf("Known(%q) armed on %q:\n  out: %s", in, v, got)
		}
	}
}

// ParseFailure runs the real parser over each hostile DSN and renders
// what it returned. The reason's quoted fragment is the leak this exists
// for: `invalid port ":<password up to the slash>" after host`.
//
// A string that does not parse has no structure, so NOTHING of it is
// rendered — not the fragment, not the host, not the scheme. Two rounds
// of this fix tried to keep the host by reading the malformed string
// with a heuristic, and each round a shape turned up that the heuristic
// read wrong and printed part of the secret out of. The kind of failure
// and the source of the value (the caller's to add) are the diagnostic.
func TestParseFailureRepeatsNothingOfTheUnparseableString(t *testing.T) {
	failed := 0
	for name, pw := range hostilePasswords {
		dsn := "postgres://stellarindex:" + pw + "@db.example.invalid:5432/app?sslmode=disable"
		_, err := url.Parse(dsn)
		if err == nil {
			continue // parseable: rendered structurally, see the test below
		}
		failed++
		t.Run(name, func(t *testing.T) {
			got := ParseFailure(dsn, err)
			assertNoStems(t, err.Error(), got)
			// Not one piece of the input, however innocent it looks: the
			// only way to be sure no fragment of the password is in there
			// is for no fragment of the string to be in there.
			for _, piece := range []string{"postgres://", "stellarindex", "db.example.invalid", "5432", "sslmode"} {
				if strings.Contains(got, piece) {
					t.Errorf("repeated %q out of a string that does not parse:\n  err: %s\n  out: %s", piece, err, got)
				}
			}
			if got == "" || got == "a syntax error" {
				t.Errorf("the operator is told nothing about what is wrong:\n  err: %s\n  out: %s", err, got)
			}
		})
	}
	// Self-accounting: if net/url ever starts accepting these, the loop
	// above passes over nothing and says so here instead of going green.
	if failed < 6 {
		t.Fatalf("only %d of %d hostile DSNs failed to parse — this test is no longer exercising the renderer", failed, len(hostilePasswords))
	}
}

// The other half: a string that DOES parse — refused by some parser
// further down, the driver's own, say — is rendered from the parse tree,
// so the operator gets the user, host, database and options and the tool
// stays diagnosable. Every part comes from the parser, so there is no
// boundary left to read wrong, and the refusing error's own text (which
// here embeds the whole DSN, as a driver's does) is classified and
// discarded rather than passed through.
func TestParseFailureRendersAParseableStringStructurally(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"userinfo password": {
			"postgres://stellarindex:" + sentinel + "@db.example.invalid:5432/app?sslmode=disable",
			"postgres://stellarindex@db.example.invalid:5432/app?sslmode=disable",
		},
		"query password": {
			"postgres://db.example.invalid/app?password=" + sentinel + "&sslmode=disable",
			"postgres://db.example.invalid/app?password=<redacted>&sslmode=disable",
		},
		"query sslpassword, mixed case": {
			"postgres://db.example.invalid/app?SSLPassword=" + sentinel,
			"postgres://db.example.invalid/app?SSLPassword=<redacted>",
		},
		"both spellings at once": {
			"postgres://u:" + sentinel + "@db.example.invalid/app?password=" + tailStem + "&sslmode=require",
			"postgres://u@db.example.invalid/app?password=<redacted>&sslmode=require",
		},
		"no credential at all": {
			"postgres://db.example.invalid:5432/app",
			"postgres://db.example.invalid:5432/app",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseFailure(tc.in, errors.New("cannot parse `"+tc.in+"`: failed to parse as DSN"))
			assertNoStems(t, tc.in, got)
			if want := "a syntax error in " + tc.want; got != want {
				t.Errorf("ParseFailure(%q)\n  got:  %s\n  want: %s", tc.in, got, want)
			}
		})
	}
}

// The query spelling, by value. keywordPasswordPattern is written for
// free text, where a space, a quote or a `;` is the only boundary on
// offer, so it ended the value there and the rest printed. A raw `&` is
// the same mistake one level up: read by the URL grammar it ends the
// value, and what follows it is the rest of the password.
func TestKnownCutsAQueryPasswordByValue(t *testing.T) {
	for name, sep := range map[string]string{
		"space": " ", "semicolon": ";", "single quote": "'", "double quote": `"`,
		"backslash": `\`, "ampersand": "&", "newline": "\n", "equals": "=",
	} {
		for _, key := range []string{"password", "sslpassword", "PassWord"} {
			dsn := "postgres://db.example.invalid:5432/app?" + key + "=" + sentinel + sep + tailStem + "&sslmode=disable"
			for shape, text := range map[string]string{
				"bare":   "unexpected argument " + dsn + " after the flags",
				"quoted": fmt.Sprintf("down: N must be a positive integer (got %q)", dsn),
			} {
				t.Run(name+"/"+key+"/"+shape, func(t *testing.T) {
					got := Known(text, dsn)
					assertNoStems(t, text, got)
					if want := "db.example.invalid:5432/app?" + key + "=<redacted>&sslmode=disable"; !strings.Contains(got, want) {
						t.Errorf("lost the host, the database or the parameter after the password; want %q:\n  in:  %s\n  out: %s", want, text, got)
					}
					if twice := Known(got, dsn); twice != got {
						t.Errorf("second pass changed the output:\n  once:  %s\n  twice: %s", got, twice)
					}
				})
			}
		}
	}
}

// The migration tool holds TWO connection strings that normally share an
// anchor: the environment's DSN and the one in argv, both
// `postgres://stellarindex:`. Cut one held value at a time and the first
// takes only the prefix the two passwords share — a rotated password, a
// naming convention — leaving the second nothing to match; its tail then
// prints wherever the text gives a pattern no boundary.
func TestKnownHoldsSeveralStringsBehindOneAnchor(t *testing.T) {
	const shared = "SHARED-PREFIX-"
	for name, tc := range map[string]struct{ other, echoed, keep string }{
		"userinfo": {
			"postgres://stellarindex:" + shared + "other@prod.example.invalid/app",
			"postgres://stellarindex:" + shared + sentinel + " " + tailStem + "@db.example.invalid/app",
			"postgres://stellarindex:<redacted>@db.example.invalid/app",
		},
		"query": {
			"postgres://prod.example.invalid/app?password=" + shared + "other",
			"postgres://db.example.invalid/app?password=" + shared + sentinel + " " + tailStem,
			"postgres://db.example.invalid/app?password=<redacted>",
		},
		"keyword": {
			"host=prod.example.invalid password=" + shared + "other",
			"host=db.example.invalid password=" + shared + sentinel + `\ ` + tailStem,
			"host=db.example.invalid password=<redacted>",
		},
	} {
		text := "flag provided but not defined: -" + tc.echoed
		// Either order: the process lists the environment's first.
		for order, held := range map[string][]string{
			"other first": {tc.other, tc.echoed},
			"other last":  {tc.echoed, tc.other},
		} {
			t.Run(name+"/"+order, func(t *testing.T) {
				got := Known(text, held...)
				assertNoStems(t, text, got)
				if strings.Contains(got, shared) {
					t.Errorf("kept the shared prefix %q:\n  out: %s", shared, got)
				}
				if !strings.Contains(got, tc.keep) {
					t.Errorf("want %q:\n  in:  %s\n  out: %s", tc.keep, text, got)
				}
			})
		}
	}
}

// The keyword/value spelling, by value. libpq lets an unquoted value
// escape a space and a quoted one escape a quote; once %q has doubled
// the backslash, the free-text pattern reads the escape as a literal
// backslash followed by the end of the value.
func TestKnownCutsAKeywordPasswordByValue(t *testing.T) {
	for name, value := range map[string]string{
		"escaped space":         sentinel + `\ ` + tailStem + " sslmode=require",
		"quoted, escaped quote": "'" + sentinel + `\' ` + tailStem + "' sslmode=require",
		"quoted, never closed":  "'" + sentinel + " " + tailStem,
	} {
		dsn := "host=db.example.invalid user=stellarindex password=" + value
		for shape, text := range map[string]string{
			"bare":   "unexpected argument " + dsn,
			"quoted": fmt.Sprintf("force: version must be a non-negative integer (got %q)", dsn),
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				got := Known(text, dsn)
				assertNoStems(t, text, got)
				if want := "host=db.example.invalid user=stellarindex password=<redacted>"; !strings.Contains(got, want) {
					t.Errorf("lost the host or the user; want %q:\n  in:  %s\n  out: %s", want, text, got)
				}
			})
		}
	}
}

// A URL with no `@` has no span and gives a pattern nothing to key on,
// so until the by-value pass learned the shape, Known printed it whole.
func TestKnownCutsAPasswordWithNoAtSignAfterIt(t *testing.T) {
	for name, dsn := range map[string]string{
		"hash where the @ goes": "postgres://stellarindex:" + sentinel + " " + tailStem + "#db.example.invalid/app",
		"pasted without a tail": "postgres://stellarindex:" + sentinel + tailStem,
	} {
		for shape, text := range map[string]string{
			"quoted":    fmt.Sprintf("unknown subcommand %q", dsn),
			"truncated": "flag provided but not defined: -" + dsn[:strings.Index(dsn, sentinel)+len(sentinel)],
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				got := Known(text, dsn)
				assertNoStems(t, text, got)
				if !strings.Contains(got, "postgres://stellarindex:<redacted>") {
					t.Errorf("lost the scheme or the user:\n  in:  %s\n  out: %s", text, got)
				}
			})
		}
	}
	// A numeric port is believed: a DSN with no credentials arms nothing.
	const in = "dial postgres://db.example.invalid:5432/app: connection refused"
	if got := Known(in, "postgres://db.example.invalid:5432/app"); got != in {
		t.Errorf("a credential-free DSN armed the cut:\n  out: %s", got)
	}
}

// A query password holding an unescaped `@` — the character F077 names,
// in the spelling attempt two did not cover. Nothing in a malformed DSN
// tells the `@` that ends a userinfo from one inside a secret, so the
// userinfo reading runs to the LAST `@` and swallows the `password=`
// that the query reading is recognised by: the cut ended at `HEAD` and
// printed `@TAIL`. The fix is not a better guess — both readings are
// held, and a cut may not stop inside a second secret whose anchor it
// consumed.
func TestKnownCutsAQueryPasswordThatHoldsAnAtSign(t *testing.T) {
	for name, dsn := range map[string]string{
		"host and port before the query": "postgres://db.example.invalid:5432/app?password=" + sentinel + "@" + tailStem + "&sslmode=disable",
		"no other parameter":             "postgres://db.example.invalid:5432/app?password=" + sentinel + "@" + tailStem,
		"bracketed IPv6 host":            "postgres://[::1]:5432/app?password=" + sentinel + "@" + tailStem,
		"sslpassword spelling":           "postgres://db.example.invalid:5432/app?sslpassword=" + sentinel + "@" + tailStem,
		"two at signs":                   "postgres://db.example.invalid:5432/app?password=" + sentinel + "@x@" + tailStem,
		"userinfo password as well":      "postgres://u:HEADLESS-" + sentinel + "@db.example.invalid:5432/app?password=" + sentinel + "@" + tailStem,
		"percent escape too":             "postgres://db.example.invalid:5432/app%ZZ?password=" + sentinel + "@" + tailStem,
	} {
		for shape, render := range map[string]func(string) string{
			"bare":      func(s string) string { return "unexpected argument " + s + " after the flags" },
			"quoted":    func(s string) string { return fmt.Sprintf("unknown subcommand %q", s) },
			"truncated": func(s string) string { return "flag provided but not defined: -" + s[:strings.Index(s, "@")] },
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				text := render(dsn)
				got := Known(text, dsn)
				assertNoStems(t, text, got)
				if twice := Known(got, dsn); twice != got {
					t.Errorf("second pass changed the output:\n  once:  %s\n  twice: %s", got, twice)
				}
			})
		}
	}
}

// A password that opens with the marker itself. cutHeld used to skip any
// position already reading `<redacted>`, for idempotence, and that skip
// read the start of this secret as a cut that had already happened and
// printed the rest of it.
func TestKnownCutsAPasswordThatStartsWithTheMarker(t *testing.T) {
	pw := "<redacted>" + sentinel + tailStem
	for name, dsn := range map[string]string{
		"userinfo": "postgres://u:" + pw + "@db.example.invalid/app",
		"query":    "postgres://db.example.invalid/app?password=" + pw,
		"keyword":  "host=db.example.invalid password=" + pw,
	} {
		t.Run(name, func(t *testing.T) {
			text := "unexpected argument " + dsn
			got := Known(text, dsn)
			assertNoStems(t, text, got)
		})
	}
}

// Known applied to a string it holds is how a caller renders that string
// itself. Every shape of secret has to come out, and a value with no
// secret has to come back unchanged.
func TestKnownAppliedToTheStringItHolds(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:" + sentinel + "@h:5432/db":                 "postgres://u:<redacted>@h:5432/db",
		"postgres://u:" + sentinel + " " + tailStem + "@h/db":     "postgres://u:<redacted>@h/db",
		"postgres://h/db?password=" + sentinel + " x&sslmode=off": "postgres://h/db?password=<redacted>&sslmode=off",
		"postgres://h/db?sslmode=off&SSLPassword=" + sentinel:     "postgres://h/db?sslmode=off&SSLPassword=<redacted>",
		// A raw `&` in the value: it ends at the parameter we recognise.
		"postgres://h/db?password=" + sentinel + "&" + tailStem + "&sslmode=off": "postgres://h/db?password=<redacted>&sslmode=off",
		"host=h user=u password=" + sentinel + " sslmode=require":                "host=h user=u password=<redacted> sslmode=require",
		// No `@`: a numeric port is believed, anything else is a secret
		// with no end marker and takes the rest of the string with it.
		"postgres://h:5432/db%ZZ":                  "postgres://h:5432/db%ZZ",
		"postgres://h:5432":                        "postgres://h:5432",
		"postgres://[::1]:5432/db%ZZ":              "postgres://[::1]:5432/db%ZZ",
		"postgres://u:" + sentinel + "#h:5432/db":  "postgres://u:<redacted>",
		"postgres://u:" + sentinel + "/x#h:5432":   "postgres://u:<redacted>",
		"postgres://u:12345#" + sentinel + "/db":   "postgres://u:<redacted>",
		"postgres://user@db.example.invalid/db%ZZ": "postgres://user@db.example.invalid/db%ZZ",
		// A query password with an `@` in it: the userinfo reading runs to
		// that `@` and eats the `password=` the query reading needs, so the
		// cut has to run through both. The host goes with it — the cost of
		// holding two readings of one ambiguous string, paid in diagnostic
		// rather than in credential.
		"postgres://h:5432/db?password=" + sentinel + "@" + tailStem: "postgres://h:<redacted>",
	} {
		if got := Known(in, in); got != want {
			t.Errorf("Known(%q, itself)\n  got:  %s\n  want: %s", in, got, want)
		}
	}
}

// libpq reads a DSN missing its `//` as keyword/value and quotes the
// whole string in `missing "=" after %q`. With no `://` to anchor on,
// Known cut nothing and the password printed whole.
func TestKnownCutsAPasswordFromADSNMissingItsSlashes(t *testing.T) {
	const host = "@db.example.invalid:5432/stellarindex"
	for pname, pw := range hostilePasswords {
		for shape, dsn := range map[string]string{
			"no slashes": "postgres:stellarindex:" + pw + host,
			"one slash":  "postgres:/stellarindex:" + pw + host,
			"no @":       "postgres:stellarindex:" + pw,
		} {
			if shape == "no @" && strings.Contains(pw, "@") {
				continue // then it does have one, and what follows it reads as the host, as with `://`
			}
			for _, held := range []string{dsn, "-dsn=" + dsn} {
				text := fmt.Sprintf(`failed to open database: missing "=" after %q in connection info string`, dsn)
				t.Run(pname+"/"+shape+"/"+held[:4], func(t *testing.T) {
					got := Known(text, held)
					assertNoStems(t, text, got)
					if !strings.Contains(got, `after "postgres:`) || !strings.Contains(got, "stellarindex:<redacted>") {
						t.Errorf("lost the scheme or the user:\n  in:  %s\n  out: %s", text, got)
					}
				})
			}
		}
	}

	dsn := "postgres:stellarindex:" + sentinel + host
	in := fmt.Sprintf(`missing "=" after %q in connection info string`, dsn)
	want := `missing "=" after "postgres:stellarindex:<redacted>@db.example.invalid:5432/stellarindex" in connection info string`
	if got := Known(in, dsn); got != want {
		t.Errorf("the cut is not exactly the password:\n  got:  %s\n  want: %s", got, want)
	}
}

// The `scheme:` reading must not arm on a value that merely has a colon:
// a host:port, a path, a DSN with no password. The text repeats each one
// and has to come back byte-identical.
func TestKnownIgnoresColonValuesWithNoPassword(t *testing.T) {
	values := []string{
		"localhost:8080",
		"/usr/local/share/with:colon",
		"postgres:db.example.invalid:5432/app",
		"postgres:user@db.example.invalid/app",
		`C:\migrations`,
		"hostaddr=::1 dbname=app",
	}
	in := "saw " + strings.Join(values, " and ") + " at 12:30"
	for _, v := range values {
		if got := Known(in, v); got != in {
			t.Errorf("Known armed on %q:\n  in:  %s\n  out: %s", v, in, got)
		}
	}
}

// Any password in a DSN missing its `//` is cut from libpq's echo of it.
func FuzzKnownCutsAPasswordFromADSNMissingItsSlashes(f *testing.F) {
	for _, pw := range hostilePasswords {
		f.Add(pw)
	}
	f.Fuzz(func(t *testing.T, middle string) {
		dsn := "postgres:stellarindex:" + sentinel + middle + tailStem + "@db.example.invalid/app"
		for _, text := range []string{
			fmt.Sprintf(`missing "=" after %q in connection info string`, dsn),
			"missing = after " + dsn,
		} {
			assertNoStems(t, text, Known(text, dsn))
		}
	})
}
