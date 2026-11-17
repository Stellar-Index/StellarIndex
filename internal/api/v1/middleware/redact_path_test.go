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

// Every problem `instance` must go through RequestInstance; a raw RequestURI echoes a key it never redacted.
func TestNoRawRequestURIInV1(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path) //nolint:gosec // walking this repo's own source tree
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "URL.RequestURI()") {
			t.Errorf("%s uses URL.RequestURI(); use RequestInstance or RedactedPath", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
