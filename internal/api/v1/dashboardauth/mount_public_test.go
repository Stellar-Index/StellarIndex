package dashboardauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// loginEntryPoints are the routes a caller with no session, API key or
// SEP-10 JWT must be able to reach to obtain a dashboard session.
var loginEntryPoints = []string{
	"GET /v1/auth/callback",
	"POST /v1/auth/login",
	"POST /v1/auth/logout",
	"POST /v1/auth/passkey/begin-login",
	"POST /v1/auth/passkey/finish-login",
	"POST /v1/auth/verify-code",
}

// TestMount_LoginEntryPointsArePublic pins #1314 for the dashboard half:
// under auth_mode=apikey or sep10 the Auth middleware wraps the whole mux,
// so a login route mounted without the public mark 401s the very caller it
// exists to log in. Session-gated routes must NOT be marked public.
func TestMount_LoginEntryPointsArePublic(t *testing.T) {
	rig := newPasskeyRig(t)
	mux := http.NewServeMux()
	pub := middleware.NewPublicRoutes()
	rig.h.Mount(mux, pub)

	got := pub.Patterns()
	sort.Strings(got)
	if !slices.Equal(got, loginEntryPoints) {
		t.Fatalf("public patterns = %v, want exactly %v", got, loginEntryPoints)
	}

	gated := []string{
		"POST /v1/account/admin/lookup",
		"POST /v1/auth/passkey/begin-register",
		"GET /v1/auth/passkey/credentials",
	}
	for _, mode := range []middleware.AuthMode{middleware.AuthModeAPIKey, middleware.AuthModeSEP10} {
		h := middleware.Chain(mux, pub.Mark(), middleware.Auth(middleware.AuthOptions{Mode: mode}))
		serve := func(op string) int {
			method, path, _ := strings.Cut(op, " ")
			r := httptest.NewRequest(method, path, strings.NewReader("{}"))
			r.Header.Set("Origin", "http://"+r.Host)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		for _, op := range loginEntryPoints {
			if code := serve(op); code == http.StatusUnauthorized {
				t.Errorf("%s under auth_mode=%s: 401 to an uncredentialed caller", op, mode)
			}
		}
		for _, op := range gated {
			if code := serve(op); code != http.StatusUnauthorized {
				t.Errorf("%s under auth_mode=%s: status %d, want 401 — a session-gated route was marked public", op, mode, code)
			}
		}
	}
}

// TestMount_LoginEntryPointsMatchOpenAPISecurityNone keeps the list above
// equal to the spec's `security: []` operations under /v1/auth/ (SEP-10
// excluded — internal/api/v1 mounts and pins those).
func TestMount_LoginEntryPointsMatchOpenAPISecurityNone(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	for range 8 {
		if body, err = os.ReadFile(filepath.Join(dir, "openapi", "stellar-index.v1.yaml")); err == nil { //nolint:gosec // repo-relative path
			break
		}
		dir = filepath.Dir(dir)
	}
	if err != nil {
		t.Fatal("could not locate openapi/stellar-index.v1.yaml from cwd")
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Security *[]map[string][]string `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var spec []string
	for path, methods := range doc.Paths {
		if !strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/auth/sep10/") {
			continue
		}
		for method, op := range methods {
			if op.Security != nil && len(*op.Security) == 0 {
				spec = append(spec, strings.ToUpper(method)+" /v1"+path)
			}
		}
	}
	sort.Strings(spec)
	if !slices.Equal(spec, loginEntryPoints) {
		t.Errorf("spec security:[] /v1/auth ops = %v, want %v", spec, loginEntryPoints)
	}
}
