// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKeyHex is the body of an obviously fake plaintext key; it is never a real credential.
var fakeKeyHex = strings.Repeat("ab", 32)

func TestRequestInstanceRedactsEveryEncodingOfAKey(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"/v1/nope?key=sip%5F" + fakeKeyHex, "/v1/nope?key=sip_[redacted]"},
		{"/v1/nope?key=sip%5f" + fakeKeyHex, "/v1/nope?key=sip_[redacted]"},
		{"/v1/nope?key=sip_" + fakeKeyHex, "/v1/nope?key=sip_[redacted]"},
		{"/v1/nope?key=sip%255F" + fakeKeyHex, "/v1/nope?key=sip_[redacted]"},
		{"/v1/nope?a=x%3Ay&key=sip%5F" + fakeKeyHex, "/v1/nope?a=x:y&key=sip_[redacted]"},
		{
			"/v1/account/keys/sip%5F" + fakeKeyHex + "?id=sip%5F" + fakeKeyHex,
			"/v1/account/keys/sip_[redacted]?id=sip_[redacted]",
		},
		{"/v1/account/keys/sip%255F" + fakeKeyHex, "/v1/account/keys/sip_[redacted]"},
		// Neighbours with no key keep their escaped form byte for byte.
		{"/v1/assets/USDC%2FG?identifier=a%3Ab&limit=5", "/v1/assets/USDC%2FG?identifier=a%3Ab&limit=5"},
		{"/v1/nope?q=%zz", "/v1/nope?q=%zz"},
		{"/v1/nope?q=sip", "/v1/nope?q=sip"},
		{"/v1/nope?q=a+b", "/v1/nope?q=a+b"},
		// A stray `%` elsewhere, a key whose body is escaped, and a second key after a plain one.
		{"/v1/nope?q=100%&key=sip%5F" + fakeKeyHex, "/v1/nope?q=100%&key=sip_[redacted]"},
		{"/v1/nope?key=sip_" + strings.Repeat("%61%62", 32), "/v1/nope?key=sip_[redacted]"},
		{"/v1/nope?a=sip_" + fakeKeyHex + "&b=sip%5F" + fakeKeyHex, "/v1/nope?a=sip_[redacted]&b=sip_[redacted]"},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.target, nil)
		if got := RequestInstance(r); got != tc.want {
			t.Errorf("RequestInstance(%s) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

func TestRedactedPathRedactsADoubleEncodedKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/account/keys/sip%255F"+fakeKeyHex, nil)
	if got, want := RedactedPath(r), "/v1/account/keys/sip_[redacted]"; got != want {
		t.Errorf("RedactedPath = %q, want %q", got, want)
	}
}

// The path side, where an invalid escape cannot reach a request: Go refuses to parse it.
func TestRedactEscapedPathSide(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/v1/keys/100%/sip%5F" + fakeKeyHex, "/v1/keys/100%/sip_[redacted]"},
		{"/v1/keys/sip_" + strings.Repeat("%61%62", 32), "/v1/keys/sip_[redacted]"},
		{"/v1/keys/sip_" + fakeKeyHex + "/sip%5F" + fakeKeyHex, "/v1/keys/sip_[redacted]/sip_[redacted]"},
		{"/v1/a%2Fb/100%/x+y", "/v1/a%2Fb/100%/x+y"},
	} {
		if got := redactEscaped(tc.in, false); got != tc.want {
			t.Errorf("redactEscaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every problem `instance` must go through RequestInstance; a raw request URI echoes a key it never redacted.
func TestNoRawRequestURIInV1(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "redact_path.go" {
			return err
		}
		src, err := os.ReadFile(path) //nolint:gosec // walking this repo's own source tree
		if err != nil {
			return err
		}
		// .RequestURI covers both the request field and URL.RequestURI().
		for _, raw := range []string{".RequestURI", "URL.String()"} {
			if strings.Contains(string(src), raw) {
				t.Errorf("%s reads %s; use RequestInstance or RedactedPath", path, raw)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
