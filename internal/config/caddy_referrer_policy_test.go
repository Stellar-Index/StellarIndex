package config_test

import (
	"path/filepath"
	"regexp"
	"testing"
)

// caddyReferrerPolicyPattern captures the leading `header` block token
// in front of `Referrer-Policy`, if any.
var caddyReferrerPolicyPattern = regexp.MustCompile(`(?m)^\s*(\S*)Referrer-Policy\s+"`)

// TestCaddyReferrerPolicyDoesNotOverrideTheAPI: the API sets `Referrer-Policy: no-referrer` itself since
// /v1/auth/callback?token= carries a credential. The site `header` block deletes `-Server`, deferring the whole block to
// write-time, so an unconditional Referrer-Policy would win; `?` (set only if absent) prevents it.
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
