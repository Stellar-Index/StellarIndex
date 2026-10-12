package redact

import (
	"strings"
	"testing"
)

// tailStem is the part of a stand-in password that follows an embedded
// `@`. It is separate from [sentinel] so a failure says which half of
// the credential got out.
const tailStem = "PLACEHOLDER-TAIL-NOT-REAL"

// hostileCarriers are credentials naive patterns stop short on: ending the password at the FIRST `@` leaks the rest, and not
// crossing a space or quote prints it whole. They match what a Go caller sees: net/url's *url.Error renders the URL with %q, so
// quoted forms matter and an embedded `"` is backslash-escaped.
var hostileCarriers = map[string]string{
	"embedded @":               "postgres://u:" + sentinel + "@" + tailStem + "@db.example.invalid/db",
	"embedded @, quoted":       `parse "postgres://u:` + sentinel + `%ZZ@` + tailStem + `@db.example.invalid/db": invalid URL escape "%ZZ"`,
	"two embedded @":           "postgres://u:" + sentinel + "@x@" + tailStem + "@db.example.invalid/db",
	"quoted, space":            `parse "postgres://u:` + sentinel + ` ` + tailStem + `@db.example.invalid/db": net/url: invalid userinfo`,
	"quoted, single quote":     `parse "postgres://u:` + sentinel + `'` + tailStem + `@db.example.invalid/db": net/url: invalid userinfo`,
	"quoted, escaped quote":    `parse "postgres://u:` + sentinel + `\"` + tailStem + `@db.example.invalid/db": net/url: invalid userinfo`,
	"quoted, escaped bs+quote": `parse "postgres://u:` + sentinel + `\\\"` + tailStem + `@db.example.invalid/db": net/url: invalid userinfo`,
}

func TestCredentialsCutsTheWholePasswordNotItsFirstSegment(t *testing.T) {
	for name, in := range hostileCarriers {
		t.Run(name, func(t *testing.T) {
			got := Credentials(in)
			for _, stem := range []string{sentinel, tailStem} {
				if strings.Contains(got, stem) {
					t.Errorf("Credentials kept %s:\n  in:  %s\n  out: %s", stem, in, got)
				}
			}
			// The destructive branch fired, so prove what it LEFT: the
			// user, the host and the database are the diagnostic, and
			// the library's reason after the URL must be byte-identical.
			if keep := "postgres://u:<redacted>@db.example.invalid/db"; !strings.Contains(got, keep) {
				t.Errorf("Credentials lost %q:\n  in:  %s\n  out: %s", keep, in, got)
			}
			if i := strings.Index(in, `": `); i >= 0 && !strings.HasSuffix(got, in[i:]) {
				t.Errorf("Credentials changed the reason after the URL:\n  in:  %s\n  out: %s", in, got)
			}
			if twice := Credentials(got); twice != got {
				t.Errorf("second pass changed the output:\n  once:  %s\n  twice: %s", got, twice)
			}
		})
	}
}

// Two connection strings in one QUOTED message must each keep their own host; a span running to the last `@` inside the quotes
// would swallow the first host and everything between.
func TestCredentialsKeepsEachQuotedURLSeparate(t *testing.T) {
	in := `copy "postgres://a:` + sentinel + `@one.invalid/db" to "postgres://b:` + tailStem + `@two.invalid/db"`
	want := `copy "postgres://a:<redacted>@one.invalid/db" to "postgres://b:<redacted>@two.invalid/db"`
	if got := Credentials(in); got != want {
		t.Errorf("Credentials(%q)\n  got:  %s\n  want: %s", in, got, want)
	}
}
