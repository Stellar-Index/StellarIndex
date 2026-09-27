package timescale

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A DSN the parser rejects fails every Open* entry point before any
// network I/O, and the api/aggregator/indexer print that error to
// stderr (journald, Loki). The password must not survive into it in
// whole or in part: net/url quotes the fragment it choked on, which for
// a reserved character in the password is the password up to that
// character.
func TestOpenRejectsAMalformedDSNWithoutEchoingThePassword(t *testing.T) {
	cases := []struct {
		name, dsn string
		stems     []string // any fragment of the password that must not appear
	}{
		{"slash", "postgres://app:Zq7vK/wP9xR2@db.internal:5432/si", []string{"Zq7vK", "wP9xR2"}},
		{"hash", "postgres://app:Zq7vK#wP9xR2@db.internal:5432/si", []string{"Zq7vK", "wP9xR2"}},
		{"question", "postgres://app:Zq7vK?wP9xR2@db.internal:5432/si", []string{"Zq7vK", "wP9xR2"}},
		{"bad escape", "postgres://app:Zq7vK%zzwP9xR2@db.internal:5432/si", []string{"Zq7vK", "wP9xR2", "%zz"}},
		// pgx's own redactor masks only up to the first `/`, so the rest
		// of the password is printed after the mask.
		{"slash then at", "postgres://app:Zq7/vK@wP9xR2@db.internal/si", []string{"Zq7", "vK", "wP9xR2"}},
		// Parses as a URL (net/url splits userinfo at the LAST `@`); pgx
		// masks up to the FIRST one and rejects the sslmode.
		{"at, rejected option", "postgres://app:Zq7vK@wP9xR2@db.internal:5432/si?sslmode=bogus", []string{"Zq7vK", "wP9xR2"}},
		// pgx reads anything without a URL prefix as keyword/value, and
		// net/url accepts those strings as a bare path, so a URL
		// rendering of one prints it whole.
		{"keyword, rejected option", "host=db.internal user=app password=Zq7vKwP9xR2 sslmode=bogus", []string{"Zq7vK", "wP9xR2"}},
		{"keyword, quoted", "host=db.internal user=app password='Zq7 vKwP9xR2' sslmode=bogus", []string{"Zq7", "vKwP9xR2"}},
		// pgx's redactor stops the quoted value at the escaped quote.
		{"keyword, escaped quote", `host=db.internal user=app password='Zq7\' vKwP9xR2' sslmode=bogus`, []string{"Zq7", "vKwP9xR2"}},
		// An unquoted space ends the value; pgx masks the head and its
		// syntax error quotes the tail as a key.
		{"keyword, unquoted space", "host=db.internal user=app password=Zq7 vKwP9xR2 sslmode=disable", []string{"Zq7", "vKwP9xR2"}},
	}
	open := map[string]func(string) error{
		"Open": func(dsn string) error {
			_, err := Open(context.Background(), dsn)
			return err
		},
		"OpenServing": func(dsn string) error {
			_, err := OpenServing(context.Background(), dsn, 5*time.Second)
			return err
		},
		"OpenBackground": func(dsn string) error {
			_, err := OpenBackground(context.Background(), dsn, 5*time.Second)
			return err
		},
	}
	for _, tc := range cases {
		for entry, fn := range open {
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				err := fn(tc.dsn)
				if err == nil {
					t.Fatal("malformed DSN opened")
				}
				msg := err.Error()
				for _, s := range tc.stems {
					if strings.Contains(msg, s) {
						t.Errorf("error repeats password fragment %q: %s", s, msg)
					}
				}
				if !strings.HasPrefix(msg, "timescale: ") {
					t.Errorf("error lost its package prefix: %s", msg)
				}
			})
		}
	}
}

// A DSN that parses as a URL but that pgx still rejects keeps its
// diagnostic — the reason and the host — with only the password cut.
func TestOpenRejectsAParseableDSNKeepingTheDiagnostic(t *testing.T) {
	dsn := "postgres://app:Zq7vKwP9xR2@db.internal:5432/si?sslmode=bogus"
	_, err := Open(context.Background(), dsn)
	if err == nil {
		t.Fatal("invalid sslmode opened")
	}
	msg := err.Error()
	if strings.Contains(msg, "Zq7vKwP9xR2") {
		t.Errorf("error repeats the password: %s", msg)
	}
	for _, want := range []string{"sslmode", "db.internal"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lost diagnostic %q: %s", want, msg)
		}
	}
}

// A keyword/value DSN keeps the name of what was rejected, in this
// package's words, and nothing of the string itself.
func TestOpenRejectsAKeywordDSNNamingTheFailure(t *testing.T) {
	cases := map[string]string{
		"host=db.internal user=app password=Zq7vKwP9xR2 sslmode=bogus":        "an invalid sslmode",
		"host=db.internal user=app password=Zq7 vKwP9xR2 sslmode=disable":     "a syntax error",
		"host=db.internal port=nope user=app password=Zq7vKwP9xR2":            "an invalid port",
		"host=db.internal user=app password=Zq7vKwP9xR2 connect_timeout=soon": "an invalid connect_timeout",
	}
	for dsn, want := range cases {
		_, err := Open(context.Background(), dsn)
		if err == nil {
			t.Fatalf("%q opened", dsn)
		}
		if msg := err.Error(); !strings.Contains(msg, want) || strings.Contains(msg, "Zq7") || strings.Contains(msg, "db.internal") {
			t.Errorf("%q: got %q, want %q and no part of the string", dsn, msg, want)
		}
	}
}
