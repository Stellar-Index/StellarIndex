package redact

import (
	"strings"
	"testing"
)

// sentinel stands in for a production password. Named so a reviewer
// reading a failure cannot mistake it for a real credential.
const sentinel = "PLACEHOLDER-NOT-A-REAL-SECRET"

// carriers are the real spellings a Postgres/Redis credential arrives in: URL DSN, libpq keyword/value, query parameter,
// managed-Redis URI, and the same URL after fmt quoted it.
var carriers = map[string]string{
	"postgres URL":          "postgres://stellarindex:" + sentinel + "@db.example.invalid:5432/stellarindex?sslmode=disable",
	"postgresql URL":        "postgresql://u:" + sentinel + "@db.example.invalid/stellarindex",
	"managed redis URI":     "rediss://default:" + sentinel + "@redis.example.invalid:6380",
	"keyword/value DSN":     "host=db.example.invalid user=stellarindex password=" + sentinel + " sslmode=require",
	"libpq query parameter": "postgres://db.example.invalid:5432/stellarindex?password=" + sentinel + "&sslmode=require",
	"quoted into an error":  `parse "postgres://u:` + sentinel + `@db.example.invalid/db": invalid URL escape`,
	"password with symbols": "postgres://u:" + sentinel + "-%ZZ+/=@db.example.invalid/db",
	"quoted keyword value":  "host=db.example.invalid password='" + sentinel + " with a space' sslmode=require",
}

// A credential surviving either renderer reaches stderr, journald and Loki for the whole retention window, so this asserts the
// property over every spelling.
func TestNeitherRendererEmitsTheSecret(t *testing.T) {
	for name, in := range carriers {
		t.Run(name, func(t *testing.T) {
			if got := Credentials(in); strings.Contains(got, sentinel) {
				t.Errorf("Credentials kept the secret:\n  in:  %s\n  out: %s", in, got)
			}
			if got := ConnString(in); strings.Contains(got, sentinel) {
				t.Errorf("ConnString kept the secret:\n  in:  %s\n  out: %s", in, got)
			}
		})
	}
}

// The other half of the contract: a redactor that eats the message is useless; the operator needs the dialled host and the
// failure reason, or the next person removes the redaction to get them back.
func TestCredentialsKeepsTheDiagnostic(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"URL keeps user, host, database and options",
			"postgres://stellarindex:" + sentinel + "@db.example.invalid:5432/stellarindex?sslmode=disable",
			"postgres://stellarindex:<redacted>@db.example.invalid:5432/stellarindex?sslmode=disable",
		},
		{
			"keyword form keeps every other keyword",
			"host=db.example.invalid user=stellarindex password=" + sentinel + " sslmode=require",
			"host=db.example.invalid user=stellarindex password=<redacted> sslmode=require",
		},
		{
			"query parameter keeps the rest of the query",
			"postgres://db.example.invalid/stellarindex?password=" + sentinel + "&sslmode=require",
			"postgres://db.example.invalid/stellarindex?password=<redacted>&sslmode=require",
		},
		{
			"the surrounding error text is untouched",
			`parse "postgres://u:` + sentinel + `@db.example.invalid/db": invalid URL escape "%ZZ"`,
			`parse "postgres://u:<redacted>@db.example.invalid/db": invalid URL escape "%ZZ"`,
		},
		{
			"two DSNs in one message are both handled",
			"from postgres://a:" + sentinel + "@one.invalid/db to postgres://b:" + sentinel + "@two.invalid/db",
			"from postgres://a:<redacted>@one.invalid/db to postgres://b:<redacted>@two.invalid/db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Credentials(tc.in); got != tc.want {
				t.Errorf("Credentials(%q)\n  got:  %s\n  want: %s", tc.in, got, tc.want)
			}
		})
	}
}

// Over-redaction has a cost too: a scrubber that mangles ordinary text
// makes every log line suspect and gets switched off. Nothing here
// carries a secret, so nothing here may change.
func TestCredentialsLeavesSecretlessTextAlone(t *testing.T) {
	for _, in := range []string{
		"postgres://db.example.invalid:5432/stellarindex?sslmode=disable", // no userinfo
		"postgres://stellarindex@db.example.invalid/stellarindex",         // user, no password
		"https://api.stellarindex.io/errors/account-store-unavailable",
		"dial tcp 127.0.0.1:5432: connect: connection refused",
		"password_env=STELLARINDEX_REDIS_PASSWORD", // a NAME, not a value
		"",
	} {
		if got := Credentials(in); got != in {
			t.Errorf("Credentials changed secretless text:\n  in:  %s\n  out: %s", in, got)
		}
	}
}

// Redaction runs at the process's write, so a value that passed through
// an already-redacting layer must survive unchanged rather than
// collecting a second marker.
func TestCredentialsIsIdempotent(t *testing.T) {
	for name, in := range carriers {
		once := Credentials(in)
		if twice := Credentials(once); twice != once {
			t.Errorf("%s: second pass changed the output:\n  once:  %s\n  twice: %s", name, once, twice)
		}
	}
}

// ConnString renders values WE format, where the whole string is suspect: it keeps only the scheme and says so when there is
// none, never passing the value through.
func TestConnString(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:" + sentinel + "@h/db":       "postgres://<redacted>",
		"mysql://u:" + sentinel + "@h:3306/d":      "mysql://<redacted>",
		"rediss://default:" + sentinel + "@h:6380": "rediss://<redacted>",
		"127.0.0.1":    "<redacted>",
		sentinel:       "<redacted>",
		"://no-scheme": "<redacted>",
		"":             "(empty)",
	} {
		if got := ConnString(in); got != want {
			t.Errorf("ConnString(%q) = %q, want %q", in, got, want)
		}
	}
}

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
