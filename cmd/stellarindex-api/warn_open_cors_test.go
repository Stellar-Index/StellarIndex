package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnOpenCORS_WildcardWithExtraEntries pins that warnOpenCORS must
// treat AllowedOrigins as wildcard-open whenever "*" appears ANYWHERE in
// the list, matching middleware.CORS's own `allowed["*"]` set-membership
// check (internal/api/v1/middleware/cors.go). A
// `len(allowedOrigins) == 1 && allowedOrigins[0] == "*"` would silently skip
// the SECURITY warning for a config like ["*", "https://evil.com"], even
// though CORS() echoes "*" to every origin regardless of the extra entry.
func TestWarnOpenCORS_WildcardWithExtraEntries(t *testing.T) {
	warnLogged := func(allowedOrigins []string, authMode string) bool {
		var buf bytes.Buffer
		lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		warnOpenCORS(lg, allowedOrigins, authMode)
		return strings.Contains(buf.String(), "SECURITY")
	}

	// Exact single-wildcard case: must warn.
	if !warnLogged([]string{"*"}, "apikey") {
		t.Error(`warnOpenCORS([]string{"*"}, "apikey"): expected SECURITY warning`)
	}

	// Wildcard alongside another explicit entry: CORS() still allows
	// every origin (wildcard := allowed["*"]) so this must ALSO warn.
	if !warnLogged([]string{"*", "https://evil.com"}, "apikey") {
		t.Error(`warnOpenCORS([]string{"*", "https://evil.com"}, "apikey"): expected SECURITY warning, wildcard is still in effect`)
	}

	// No wildcard present at all: must stay quiet.
	if warnLogged([]string{"https://stellarindex.io"}, "apikey") {
		t.Error(`warnOpenCORS without "*": must not warn`)
	}
}
