package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublicClaimsMatchTheDeployment pins the handful of factual claims
// the README and the OpenAPI `info.description` make about this API —
// the description is served to every consumer through the rendered
// reference at docs.stellarindex.io, so a stale sentence there is a
// published falsehood, not an internal note.
//
// Each entry below was measured against the hosted deployment rather
// than reasoned about:
//
//   - "complete since-inception history": issue #349 concluded that any
//     full-history claim would be false. Daily OHLC for XLM/USD starts
//     2018-07-01 and has no bars between 2021-01-31 and 2026-03-12.
//   - "back to 2015" on /price/at and /price/changes: same measurement —
//     2015/2016/2017 return zero bars.
//   - "protocol 23": pubnet has been on 27 since the P27 upgrade;
//     /v1/ledgers reports protocol_version 27.
//   - "/healthz, /readyz, /version": those paths 404 on the hosted API.
//     The routes registered in server.go are /v1-prefixed, and /metrics
//     is loopback-only (never public).
//   - "SEP-10 web auth" listed flatly under What's shipped: the handlers
//     exist but answer 503 "no SEP-10 validator wired" without a server
//     signing seed, which the hosted deployment does not set.
//
// Assertions run over whitespace-collapsed text so a reflow of the
// surrounding prose can't quietly hollow the check out.
func TestPublicClaimsMatchTheDeployment(t *testing.T) {
	root := repoRootForClaims(t)

	cases := []struct {
		path string
		// forbidden are claims contradicted by the deployment.
		forbidden []string
		// required are the corrections, so a revert is caught too.
		required []string
	}{
		{
			path: "README.md",
			forbidden: []string{
				"complete since-inception history",
				"Stellar pubnet protocol 23",
				"SEP-10 web auth, SSE streams",
				"(`/healthz`, `/readyz`, `/version`, `/metrics`)",
			},
			required: []string{
				"not since-inception",
				"Stellar pubnet protocol 27",
				"`/v1/healthz`, `/v1/readyz`, `/v1/version`",
				"`/metrics` is loopback-only, never public",
				"is code-shipped but not enabled",
			},
		},
		{
			path: "openapi/stellar-index.v1.yaml",
			forbidden: []string{
				"complete since-inception history",
				"down to daily, back to 2015",
				"daily bars reach back to 2015",
			},
			required: []string{
				"not since-inception",
				"down to daily, back to 2018",
				"daily bars reach back to 2018",
			},
		},
		{
			// The rendered reference is a byte copy of the spec
			// (`make docs-api`); assert it carries the correction so a
			// spec-only edit that skips the regeneration is caught here
			// as well as by the CI drift check.
			path: "docs/reference/api/stellar-index.v1.yaml",
			forbidden: []string{
				"complete since-inception history",
				"down to daily, back to 2015",
			},
			required: []string{"not since-inception"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			text := strings.Join(strings.Fields(string(body)), " ")
			for _, claim := range tc.forbidden {
				if strings.Contains(text, claim) {
					t.Errorf("%s still publishes %q, which the deployment contradicts", tc.path, claim)
				}
			}
			for _, claim := range tc.required {
				if !strings.Contains(text, claim) {
					t.Errorf("%s no longer states %q", tc.path, claim)
				}
			}
		})
	}
}

// repoRootForClaims walks up from the package directory to the checkout
// root, so the test works under `go test ./...` from anywhere.
func repoRootForClaims(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root (go.mod) from cwd")
	return ""
}
