// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sameSiteRig wraps a handler that records whether it ran, behind the
// guard. `corsOrigins` non-nil also chains the real CORS middleware so
// the allow-list hand-off is exercised end to end rather than faked.
func sameSiteRig(corsOrigins []string) (http.Handler, *bool) {
	reached := new(bool)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	h = middleware.RequireSameSiteWrite(quietLogger())(h)
	if corsOrigins != nil {
		opts := middleware.CORSOptions{
			AllowedOrigins: corsOrigins,
			// Every rig origin is also credentialed here: these tests
			// exercise the write guard's origin-matching, not the
			// RSEC-X1 read/credentialed split (see
			// TestRequireSameSiteWrite_BlocksOriginThatIsCORSReadableButNotCredentialed).
			AllowCredentials:    true,
			CredentialedOrigins: corsOrigins,
			AllowedMethods:      []string{"GET", "POST", "DELETE", "OPTIONS"},
		}
		// CORS panics on wildcard+credentials (no browser honours the
		// combo), so the wildcard rig has to be the un-credentialed
		// public-read-API shape a real deployment would use.
		if len(corsOrigins) == 1 && corsOrigins[0] == "*" {
			opts.AllowCredentials = false
			opts.CredentialedOrigins = nil
		}
		h = middleware.CORS(opts)(h)
	}
	return h, reached
}

func TestRequireSameSiteWrite_BlocksCrossSitePost(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a cross-site write")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got := body["type"]; got != "https://api.stellarindex.io/errors/cross-site-request-blocked" {
		t.Errorf("problem type = %v", got)
	}
}

func TestRequireSameSiteWrite_AllowsAllowListedOrigin(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true (the explorer is allow-listed)", w.Code, *reached)
	}
}

// TestRequireSameSiteWrite_BlocksOriginThatIsCORSReadableButNotCredentialed
// is the RSEC-X1 regression: a same-site write guard must not trust an
// origin just because it's on the public CORS read allow-list. Only
// origins in CredentialedOrigins may bypass the guard.
func TestRequireSameSiteWrite_BlocksOriginThatIsCORSReadableButNotCredentialed(t *testing.T) {
	reached := new(bool)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	h = middleware.RequireSameSiteWrite(quietLogger())(h)
	h = middleware.CORS(middleware.CORSOptions{
		AllowedOrigins:      []string{"https://stellarindex.io", "https://docs.stellarindex.io"},
		AllowCredentials:    true,
		CredentialedOrigins: []string{"https://stellarindex.io"},
		AllowedMethods:      []string{"GET", "POST", "DELETE", "OPTIONS"},
	})(h)

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://docs.stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (docs.stellarindex.io is CORS-readable but not credentialed)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a write from a non-credentialed origin")
	}
}

func TestRequireSameSiteWrite_AllowsSameOriginWithoutCORS(t *testing.T) {
	// No CORS middleware at all: a deployment serving the dashboard
	// from the API's own origin must still work.
	h, reached := sameSiteRig(nil)

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true (same-origin write)", w.Code, *reached)
	}
}

func TestRequireSameSiteWrite_BlocksWhenNoOriginOrReferer(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodDelete, "https://api.stellarindex.io/v1/dashboard/keys/x", nil)
	req.Host = "api.stellarindex.io"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail closed with neither header)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a headerless write")
	}
}

func TestRequireSameSiteWrite_NullOriginFallsToRefererAndIsBlocked(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/auth/verify-code", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "null") // sandboxed iframe / opaque origin
	req.Header.Set("Referer", "https://evil.example/csrf")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Origin: null must not pass)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for an opaque-origin write")
	}
}

func TestRequireSameSiteWrite_RefererOnlyFromAllowListedSitePasses(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/auth/login", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Referer", "https://stellarindex.io/signin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true", w.Code, *reached)
	}
}

func TestRequireSameSiteWrite_SafeMethodsUntouched(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		*reached = false
		req := httptest.NewRequest(m, "https://api.stellarindex.io/v1/dashboard/keys", nil)
		req.Host = "api.stellarindex.io"
		req.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || !*reached {
			t.Errorf("%s: status = %d reached = %v, want 204/true (reads are not gated)", m, w.Code, *reached)
		}
	}
}

// TestRequireSameSiteWrite_WildcardCORSDoesNotOpenTheDashboard — a
// deployment that serves a public read API with `AllowedOrigins:["*"]`
// must not thereby let every site on the internet drive a cookie-
// authenticated write.
func TestRequireSameSiteWrite_WildcardCORSDoesNotOpenTheDashboard(t *testing.T) {
	h, reached := sameSiteRig([]string{"*"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (wildcard CORS is not a credentialed allow-list)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran under a wildcard CORS policy")
	}
}

// TestRequireSameSiteWrite_SiblingSubdomainIsBlocked is the gap
// SameSite=Lax structurally cannot close: `evil.stellarindex.io` is
// SAME-SITE with `api.stellarindex.io`, so Lax sends the session
// cookie. Only an origin allow-list rejects it.
func TestRequireSameSiteWrite_SiblingSubdomainIsBlocked(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/webhooks", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://uploads.stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (same-site sibling origin is still not allow-listed)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a same-site sibling origin")
	}
}

const deployedConfigTemplate = "../../../../configs/ansible/roles/archival-node/templates/stellarindex.toml.j2"

// servedSurfaces is every stellarindex.io host this project actually
// serves (Caddy, Cloudflare Pages). An origin outside it has no owner, and
// trusting an unowned host is trusting whoever registers it next.
var servedSurfaces = map[string]bool{
	"stellarindex.io":           true,
	"www.stellarindex.io":       true,
	"api.stellarindex.io":       true,
	"status.stellarindex.io":    true,
	"docs.stellarindex.io":      true,
	"testnet.stellarindex.io":   true,
	"futurenet.stellarindex.io": true,
}

var dashboardBaseURLRe = regexp.MustCompile(`(?m)^base_url\s*=\s*"\{\{\s*stellarindex_dashboard_base_url\s*\|\s*default\('([^']+)'\)`)

// deployedOriginList reads one `key = ["a", "b"]` line from the shipped
// archival-node config template.
func deployedOriginList(t *testing.T, tmpl, key string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + key + `\s*=\s*\[(.*)\]\s*$`).FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatalf("%s not found in %s", key, deployedConfigTemplate)
	}
	var out []string
	for _, q := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty in %s", key, deployedConfigTemplate)
	}
	return out
}

func readDeployedTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(deployedConfigTemplate)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDeployedOrigins_AreServedSurfaces: every origin the shipped config
// lets read the API cross-origin must be a host this project serves.
func TestDeployedOrigins_AreServedSurfaces(t *testing.T) {
	tmpl := readDeployedTemplate(t)
	for _, key := range []string{"allowed_origins", "credentialed_origins"} {
		for _, origin := range deployedOriginList(t, tmpl, key) {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "https" || u.Path != "" || !servedSurfaces[u.Host] {
				t.Errorf("%s lists %q, which is not an https origin of a served surface", key, origin)
			}
		}
	}
}

// TestDeployedCredentialedOrigins_AreOnlyTheDashboardSite pins the
// credentialed list to where the dashboard is actually served: the
// [api.dashboard] base_url origin and its www twin, nothing else.
func TestDeployedCredentialedOrigins_AreOnlyTheDashboardSite(t *testing.T) {
	tmpl := readDeployedTemplate(t)
	m := dashboardBaseURLRe.FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatalf("[api.dashboard] base_url default not found in %s", deployedConfigTemplate)
	}
	base, err := url.Parse(m[1])
	if err != nil || base.Host == "" {
		t.Fatalf("dashboard base_url %q does not parse: %v", m[1], err)
	}
	apex := strings.TrimPrefix(base.Host, "www.")
	want := map[string]bool{base.Scheme + "://" + apex: true, base.Scheme + "://www." + apex: true}
	for _, origin := range deployedOriginList(t, tmpl, "credentialed_origins") {
		if !want[origin] {
			t.Errorf("credentialed_origins lists %q; only the dashboard site %s (and its www twin) may carry the session cookie", origin, m[1])
		}
	}
}

// TestDeployedOrigins_ReadOnlyOriginCannotWrite runs the shipped lists
// through the real CORS + write guard: every CORS-readable origin that is
// not credentialed is refused a state-changing request, and every
// credentialed one is admitted.
func TestDeployedOrigins_ReadOnlyOriginCannotWrite(t *testing.T) {
	tmpl := readDeployedTemplate(t)
	allowed := deployedOriginList(t, tmpl, "allowed_origins")
	credentialed := deployedOriginList(t, tmpl, "credentialed_origins")
	isCred := map[string]bool{}
	for _, o := range credentialed {
		isCred[o] = true
	}
	reached := new(bool)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	h = middleware.RequireSameSiteWrite(quietLogger())(h)
	h = middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: allowed, AllowCredentials: true, CredentialedOrigins: credentialed,
		AllowedMethods: []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
	})(h)

	for _, origin := range allowed {
		if origin == "https://api.stellarindex.io" {
			continue // the API's own origin: same-origin, not cross-origin
		}
		*reached = false
		req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
		req.Host = "api.stellarindex.io"
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if isCred[origin] {
			if w.Code != http.StatusNoContent || !*reached {
				t.Errorf("credentialed %s: status = %d reached = %v, want 204/true", origin, w.Code, *reached)
			}
			continue
		}
		if w.Code != http.StatusForbidden || *reached {
			t.Errorf("read-only %s: status = %d reached = %v, want 403/false", origin, w.Code, *reached)
		}
	}
}
