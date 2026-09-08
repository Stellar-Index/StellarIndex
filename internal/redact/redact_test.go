package redact

import (
	"strings"
	"testing"
)

// sentinel stands in for a production password. Named so a reviewer
// reading a failure cannot mistake it for a real credential.
const sentinel = "PLACEHOLDER-NOT-A-REAL-SECRET"

// carriers are the spellings a Postgres/Redis credential actually
// arrives in. Every one is a real form: the URL DSN our config accepts,
// the libpq keyword/value form database/sql also takes, the query
// parameter libpq honours, the managed-Redis URI every hosted vendor
// issues, and the same URL after fmt has quoted it into an error.
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

// A credential that survives either renderer reaches stderr, journald
// and Loki, where it is readable for the whole retention window by
// anyone with Grafana access. This is the property that matters, so it
// is asserted over every spelling rather than one.
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

// The other half of the contract. A redactor that eats the message is
// not safe, it is just useless: the operator reading a failed migration
// needs the host it dialled and the reason it failed, and if those are
// gone the next person removes the redaction to get their diagnostic
// back.
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

// ConnString is the renderer for values WE format, where the whole
// string is suspect. It keeps only the scheme — which is the operator's
// mistake in the branch that calls it — and says so when there is not
// even a scheme, rather than passing the value through.
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
