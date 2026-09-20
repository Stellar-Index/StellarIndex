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
// size bounds them before this fix — an attacker-chosen path or
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
