package config_test

import (
	"path/filepath"
	"regexp"
	"testing"
)

// caddyReferrerPolicyPattern captures the leading `header` block token
// in front of `Referrer-Policy`, if any.
var caddyReferrerPolicyPattern = regexp.MustCompile(`(?m)^\s*(\S*)Referrer-Policy\s+"`)

// TestCaddyReferrerPolicyDoesNotOverrideTheAPI pins #605: the API sets
// `Referrer-Policy: no-referrer` itself
// (internal/api/v1/middleware/security_headers.go), because
// `/v1/auth/callback?token=…` and `/v1/signup/verify?token=…` are
// top-level navigations whose query carries a credential. The site
// block's `header` directive also deletes `-Server`, which — per Caddy's
// Caddyfile header semantics — defers the WHOLE block's header ops to
// write-time. An unconditional `Referrer-Policy "strict-…"` in that same
// block therefore wins at write-time and silently downgrades the API's
// no-referrer back to strict-origin-when-cross-origin. The `?` prefix
// (set-only-if-absent) is the fix; a bare field name regresses it.
func TestCaddyReferrerPolicyDoesNotOverrideTheAPI(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src := readCaddyfile(t, path)
			m := caddyReferrerPolicyPattern.FindStringSubmatch(src)
			if m == nil {
				t.Fatalf("%s: no `Referrer-Policy \"...\"` line found in the header block", path)
			}
			if m[1] != "?" {
				t.Errorf("%s: Referrer-Policy is set unconditionally (prefix %q) — "+
					"it must be `?Referrer-Policy` so the edge only fills in a default "+
					"when the API hasn't already set the header, not overwrite it at write-time",
					path, m[1])
			}
		})
	}
}
