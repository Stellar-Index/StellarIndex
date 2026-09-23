package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
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
		{
			// RLT-220: the /rwa/assets requirement-4 prose named only two
			// bases while the same operation's 200 example already listed
			// a third (sep1_isin_declaration, internal/rwa BasisSep1ISIN).
			// The narrative and its own example disagreed with each other.
			path: "openapi/stellar-index.v1.yaml",
			forbidden: []string{
				"`definition.anchor_classes`) or `oracle_rwa_feed` (an independent oracle publishes a net-asset-value feed for an instrument of this code, per ADR-0028). The oracle arm is keyed",
			},
			required: []string{
				"or `sep1_isin_declaration` (the bound entry's `anchor_asset` is a well-formed ISIN)",
			},
		},
		{
			path: "docs/reference/api/stellar-index.v1.yaml",
			forbidden: []string{
				"`definition.anchor_classes`) or `oracle_rwa_feed` (an independent oracle publishes a net-asset-value feed for an instrument of this code, per ADR-0028). The oracle arm is keyed",
			},
			required: []string{
				"or `sep1_isin_declaration` (the bound entry's `anchor_asset` is a well-formed ISIN)",
			},
		},
		{
			// T495 / RLT-159: §7.1 documented a Starter/Pro/Business/
			// Enterprise ladder (1k/10k/50k) that platform.Tier no longer
			// has, and the account override is a floor
			// (auth/apikey_postgres.go), not a replacement limit.
			path: "docs/reference/api-design.md",
			forbidden: []string{
				"Pro | staff-set",
				"Business | staff-set",
				"Enterprise | staff-set override",
				"1,000 / 10,000 / 50,000 / per-contract",
				"higher or lower per-account limit",
				"the override is the real limit",
				"100,000 ceiling above no longer applies",
			},
			required: []string{
				"| Free | `POST /v1/signup` (every registered account's default) | **1,000** | **" + thousands(platform.TierFree.MaxRateLimitPerMin()) + "** |",
				"| Partner | staff-set `tier` on `PATCH /v1/admin/accounts/{id}` | **1,000** | **" + thousands(platform.TierPartner.MaxRateLimitPerMin()) + "** |",
				"is an account-wide **floor**",
				"It can only raise a limit, never lower one",
			},
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

// TestErrorDocExampleMatchesRateLimitResponse holds the api-design.md
// §11 problem example to the 429 the rate-limit middleware really
// writes: same `type` URL and status, and no body field the response
// does not carry (RLT-159: the doc showed `errors/rate-limit-exceeded`
// and a `retry_after` body field; the code emits `errors/rate-limited`
// and carries the delay only in the Retry-After header).
func TestErrorDocExampleMatchesRateLimitResponse(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootForClaims(t), "docs/reference/api-design.md"))
	if err != nil {
		t.Fatalf("read api-design.md: %v", err)
	}
	var documented map[string]any
	if err := json.Unmarshal([]byte(errorsSectionExample(t, string(body))), &documented); err != nil {
		t.Fatalf("§11 example is not valid JSON: %v", err)
	}
	emitted := emittedRateLimitProblem(t)

	for _, field := range []string{"type", "status"} {
		if documented[field] != emitted[field] {
			t.Errorf("§11 example %s = %v, the 429 response carries %v", field, documented[field], emitted[field])
		}
	}
	for field := range documented {
		if _, ok := emitted[field]; !ok {
			t.Errorf("§11 example documents body field %q, which the 429 response does not carry", field)
		}
	}
}

// errorsSectionExample returns the JSON object in the first fenced
// block of api-design.md's "## 11. Errors" section.
func errorsSectionExample(t *testing.T, doc string) string {
	t.Helper()
	_, section, ok := strings.Cut(doc, "## 11. Errors")
	if !ok {
		t.Fatal("api-design.md has no \"## 11. Errors\" section")
	}
	_, fenced, ok := strings.Cut(section, "```")
	if !ok {
		t.Fatal("§11 has no fenced example")
	}
	fenced, _, _ = strings.Cut(fenced, "```")
	start, end := strings.Index(fenced, "{"), strings.LastIndex(fenced, "}")
	if start < 0 || end < start {
		t.Fatal("§11 fenced example carries no JSON object")
	}
	return fenced[start : end+1]
}

// emittedRateLimitProblem drives the real rate-limit middleware past a
// one-request budget and returns the decoded 429 problem body.
func emittedRateLimitProblem(t *testing.T) map[string]any {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	keyFn := func(*http.Request) string { return "doc-example" }
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := middleware.RateLimit(ratelimit.New(rdb, 1, time.Minute), keyFn, nil, nil)(ok)

	var w *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price", nil))
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", w.Code)
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	return problem
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

// thousands renders n with comma grouping, the way the docs print limits.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
