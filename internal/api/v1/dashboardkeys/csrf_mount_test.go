package dashboardkeys

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardpricealerts"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardwebhooks"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

const crossSiteProblemType = "https://api.stellarindex.io/errors/cross-site-request-blocked"

// routePattern matches a ServeMux pattern literal: "METHOD /v1/...".
var routePattern = regexp.MustCompile(`^(GET|HEAD|OPTIONS|POST|PUT|PATCH|DELETE) (/v1/\S*)$`)

// TestMount_EveryDashboardWriteIsSameSiteGuarded is GH-804's mount-level
// net: every state-changing route registered by any dashboard* package
// must answer a cross-site Origin with the CSRF problem, never reach its
// handler. Routes are enumerated from the packages' source, not listed
// here, so a new `mux.HandleFunc("POST /v1/dashboard/...")` without the
// guard fails this test by construction, and a new dashboard package
// fails it until it is mounted below.
func TestMount_EveryDashboardWriteIsSameSiteGuarded(t *testing.T) {
	_, _, sc := newTestRig(t)
	mux := http.NewServeMux()
	mounted := mountAllDashboardPackages(t, mux)

	writes := dashboardRoutes(t, mounted)
	if len(writes) == 0 {
		t.Fatal("found no state-changing dashboard routes — the source scan is not looking at the right shape")
	}
	idPath := regexp.MustCompile(`\{[^}]+\}`)
	for _, route := range writes {
		method, path, _ := strings.Cut(route, " ")
		target := idPath.ReplaceAllString(path, "8f14e45f-ceea-467a-9575-1c1d1f6e0e5b")
		t.Run(route, func(t *testing.T) {
			req := sessionRequest(t, method, target, nil, sc)
			req.Host = "api.stellarindex.io"
			req.Header.Set("Origin", "https://evil.example")
			w := httptest.NewRecorder()
			defer func() {
				// The nil-backed stores panic once a handler runs.
				if r := recover(); r != nil {
					t.Fatalf("cross-site %s reached its handler (%v): route not same-site guarded", route, r)
				}
			}()
			mux.ServeHTTP(w, req)
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusForbidden || body["type"] != crossSiteProblemType {
				t.Fatalf("cross-site %s answered %d type=%v, want 403 %s (route not same-site guarded)",
					route, w.Code, body["type"], crossSiteProblemType)
			}
		})
	}
}

// mountAllDashboardPackages mounts every dashboard* package the way
// v1.Server does and returns the package directory names it covered. The
// stores are nil-backed: the guard must answer before any handler runs.
func mountAllDashboardPackages(t *testing.T, mux *http.ServeMux) map[string]bool {
	t.Helper()
	public := middleware.NewPublicRoutes()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	keys, _, _ := newTestRig(t)
	keys.Mount(mux, public)

	auth, err := dashboardauth.NewHandlers(&dashboardauth.Config{
		Accounts: struct{ platform.AccountStore }{},
		Users:    struct{ platform.UserStore }{},
		Tokens:   struct{ platform.TokenStore }{},
		Passkeys: struct {
			platform.WebAuthnCredentialStore
		}{},
		Sender:           &notify.NoopSender{},
		DashboardBaseURL: "https://stellarindex.example",
		EmailFrom:        "noreply@stellarindex.example",
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("dashboardauth.NewHandlers: %v", err)
	}
	auth.Mount(mux, public)

	hooks, err := dashboardwebhooks.NewHandlers(dashboardwebhooks.Config{
		Webhooks: struct{ platform.WebhookStore }{}, Logger: logger,
	})
	if err != nil {
		t.Fatalf("dashboardwebhooks.NewHandlers: %v", err)
	}
	hooks.Mount(mux, public)

	alerts, err := dashboardpricealerts.NewHandlers(dashboardpricealerts.Config{
		Alerts: struct{ platform.PriceAlertStore }{}, Logger: logger,
	})
	if err != nil {
		t.Fatalf("dashboardpricealerts.NewHandlers: %v", err)
	}
	alerts.Mount(mux, public)

	return map[string]bool{
		"dashboardauth": true, "dashboardkeys": true,
		"dashboardpricealerts": true, "dashboardwebhooks": true,
	}
}

// dashboardRoutes scans the non-test source of every sibling dashboard*
// package for route-pattern literals and returns the state-changing ones.
// A package that registers routes but is not in mounted fails the test.
func dashboardRoutes(t *testing.T, mounted map[string]bool) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("..", "dashboard*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var writes []string
	for _, dir := range dirs {
		routes := routeLiterals(t, dir)
		if len(routes) > 0 && !mounted[filepath.Base(dir)] {
			t.Errorf("package %s registers %v but is not mounted by this test — mount it", filepath.Base(dir), routes)
		}
		for _, r := range routes {
			switch m, _, _ := strings.Cut(r, " "); m {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				writes = append(writes, r)
			}
		}
	}
	sort.Strings(writes)
	return writes
}

func routeLiterals(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && routePattern.MatchString(s) {
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

// TestMount_CrossSiteWriteBlockedBeforeHandler is the C3-031 / C3-057
// regression at the mount point rather than the middleware unit: a
// cross-site page must not be able to mint or revoke a logged-in
// customer's API keys with their session cookie.
//
// It asserts the CSRF `type` URI specifically, not just "403" — the
// handler's own role/session checks also produce 403s, and a test that
// accepted any 403 would pass even with the guard removed.
func TestMount_CrossSiteWriteBlockedBeforeHandler(t *testing.T) {
	h, _, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	for _, tc := range []struct {
		name, method, target string
	}{
		{"mint", http.MethodPost, "/v1/dashboard/keys"},
		// A non-existent id is deliberate: if the guard were removed
		// the handler would answer 404, which fails the 403 assertion
		// below — so this case can't pass without the guard.
		{"revoke", http.MethodDelete, "/v1/dashboard/keys/8f14e45f-ceea-467a-9575-1c1d1f6e0e5b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sessionRequest(t, tc.method, tc.target, nil, sc)
			req.Host = "api.stellarindex.io"
			req.Header.Set("Origin", "https://evil.example")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v", err)
			}
			if got := body["type"]; got != "https://api.stellarindex.io/errors/cross-site-request-blocked" {
				t.Fatalf("problem type = %v, want the cross-site guard's (handler reached)", got)
			}
		})
	}

	// Reads stay reachable — the guard must not have been hung on the
	// whole route table.
	req := sessionRequest(t, http.MethodGet, "/v1/dashboard/keys", nil, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (reads are not gated); body = %s", w.Code, w.Body.String())
	}
}

// TestMount_SameSiteWriteReachesHandler proves the guard passes the
// legitimate same-origin dashboard write through to the handler.
func TestMount_SameSiteWriteReachesHandler(t *testing.T) {
	h, _, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:            "production",
		RateLimitPerMin: 1000,
	}, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
}
