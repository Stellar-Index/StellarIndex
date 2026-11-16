// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Neither `path` nor `user_agent` is allow-listed the way QueryShape's
// parameters are, and nothing but Go's ~1 MB default header/request-line
// size would bound them without a cap — an attacker-chosen path or
// User-Agent is exactly the journal-flooding channel QueryShape was
// already hardened against for query parameters.
func TestLoggerCapsPathAndUserAgentLength(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	longSegment := strings.Repeat("a", 5000)
	h := Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+longSegment, nil)
	req.Header.Set("User-Agent", strings.Repeat("b", 5000))
	h.ServeHTTP(httptest.NewRecorder(), req)

	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		if rec["msg"] == "http request" {
			break
		}
	}

	// A generous bound, well above any legitimate route/User-Agent and
	// far below the 5000-byte fixtures: this fails when the field is
	// logged verbatim and passes once it's capped, independent of the
	// exact cap chosen.
	const wantMaxLen = 1000

	path, _ := rec["path"].(string)
	if len(path) > wantMaxLen {
		t.Errorf("logged path length %d is unbounded — a 5000-byte path must not reach "+
			"the journal verbatim (want <= %d)", len(path), wantMaxLen)
	}
	ua, _ := rec["user_agent"].(string)
	if len(ua) > wantMaxLen {
		t.Errorf("logged user_agent length %d is unbounded — a 5000-byte User-Agent must "+
			"not reach the journal verbatim (want <= %d)", len(ua), wantMaxLen)
	}
}

// A plaintext API key in the request path (pasted where a key id belongs) must not reach the journal.
func TestLoggerRedactsAKeySecretInThePath(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	secret := "sip_" + strings.Repeat("ab", 32)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/v1/account/keys/"+secret, nil))

	if strings.Contains(buf.String(), secret[8:]) {
		t.Errorf("access log carries the plaintext key: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "/v1/account/keys/sip_[redacted]") {
		t.Errorf("access log lost the route: %s", buf.String())
	}
}

func TestRequestInstanceRedactsAKeySecret(t *testing.T) {
	secret := "sip_" + strings.Repeat("ab", 32)
	for target, want := range map[string]string{
		"/v1/account/keys/" + secret:                     "/v1/account/keys/sip_[redacted]",
		"/v1/account/keys/sip%5F" + secret[4:]:           "/v1/account/keys/sip_[redacted]",
		"/v1/admin/keys/" + secret + "?identifier=a%3Ab": "/v1/admin/keys/sip_[redacted]?identifier=a%3Ab",
		"/v1/assets/USDC%2FG?limit=5":                    "/v1/assets/USDC%2FG?limit=5",
	} {
		r := httptest.NewRequest(http.MethodDelete, target, nil)
		if got := RequestInstance(r); got != want {
			t.Errorf("RequestInstance(%s) = %q, want %q", target, got, want)
		}
		if got := RedactedPath(r); strings.Contains(got, secret[8:]) {
			t.Errorf("RedactedPath(%s) = %q carries the key", target, got)
		}
	}
}
