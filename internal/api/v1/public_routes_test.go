package v1

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

// specSecurityNoneOps returns every OpenAPI operation declared
// `security: []` as "METHOD /v1<path>", sorted.
func specSecurityNoneOps(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for path, methods := range doc.Paths {
		for method, node := range methods {
			var op struct {
				Security *[]map[string][]string `yaml:"security"`
			}
			if node.Kind != yaml.MappingNode || node.Decode(&op) != nil {
				continue
			}
			if op.Security != nil && len(*op.Security) == 0 {
				ops = append(ops, strings.ToUpper(method)+" /v1"+path)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

// isDashboardLoginOp reports whether op is one of the dashboard login entry
// points, which dashboardauth mounts (and pins in its own test) — this
// package cannot import it.
func isDashboardLoginOp(op string) bool {
	_, path, _ := strings.Cut(op, " ")
	return strings.HasPrefix(path, "/v1/auth/") && !strings.HasPrefix(path, "/v1/auth/sep10/")
}

// TestPublicOpenAPIRoutes_AnswerWithoutCredential pins that every `security: []` operation answers an uncredentialed
// caller
// with non-401 under credential-required modes; else sep10 can't issue its credential and apikey signup/verification fail.
func TestPublicOpenAPIRoutes_AnswerWithoutCredential(t *testing.T) {
	ops := specSecurityNoneOps(t)
	for _, mode := range []middleware.AuthMode{middleware.AuthModeAPIKey, middleware.AuthModeSEP10} {
		t.Run(string(mode), func(t *testing.T) {
			h := New(Options{Auth: middleware.Auth(middleware.AuthOptions{Mode: mode})}).Handler()
			serve := func(method, path string) int {
				r := httptest.NewRequest(method, path, strings.NewReader("{}"))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}
			// Control: a data route still demands a credential, or every
			// non-401 below would prove nothing.
			if code := serve(http.MethodGet, "/v1/assets"); code != http.StatusUnauthorized {
				t.Fatalf("control GET /v1/assets = %d, want 401 under %s", code, mode)
			}
			checked := 0
			for _, op := range ops {
				if isDashboardLoginOp(op) {
					continue
				}
				method, path, _ := strings.Cut(op, " ")
				if code := serve(method, path); code == http.StatusUnauthorized {
					t.Errorf("%s answered 401 to an uncredentialed caller under auth_mode=%s; "+
						"the spec declares it security: [] — mount it via handlePublic", op, mode)
				}
				checked++
			}
			// healthz, readyz, livez/lake, version, status, status/notices,
			// register, signup, signup/verify, sep10 challenge + token.
			if checked < 11 {
				t.Fatalf("checked %d security-none ops, want at least 11 — the spec walk found too few", checked)
			}
		})
	}
}

// TestPublicRoutes_MatchOpenAPISecurityNone keeps handlePublic and the spec's `security: []` in lockstep both ways: a
// spec-public route mounted gated 401s under apikey/sep10; a server-public route the spec doesn't declare is an
// undocumented hole.
func TestPublicRoutes_MatchOpenAPISecurityNone(t *testing.T) {
	// Credential-BLIND infra probes: exempted by
	// middleware.isUnauthenticatedInfraPath, not by handlePublic.
	infra := []string{"GET /v1/healthz", "GET /v1/readyz", "GET /v1/livez/lake", "GET /v1/version"}
	// Public but outside the /v1 spec by nature (RFC 9116 metadata).
	offSpec := []string{"GET /.well-known/security.txt"}

	var want []string
	for _, op := range specSecurityNoneOps(t) {
		if isDashboardLoginOp(op) || slices.Contains(infra, op) {
			continue
		}
		want = append(want, op)
	}
	want = append(want, offSpec...)
	sort.Strings(want)

	got := New(Options{}).publicRoutes.Patterns()
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("handlePublic routes != spec security:[] ops\n got: %v\nwant: %v", got, want)
	}
}

// TestTrailingSlashRedirectCarriesCORS: the trailing-slash 308 must carry Access-Control-Allow-Origin, otherwise browsers kill
// the cross-origin redirect and every trailing-slash API URL is as dead as the 404 it prevents.
func TestTrailingSlashRedirectCarriesCORS(t *testing.T) {
	s := New(Options{
		CORS: middleware.CORS(middleware.CORSOptions{
			AllowedOrigins: []string{"https://stellarindex.io"},
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/", nil)
	req.Header.Set("Origin", "https://stellarindex.io")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://stellarindex.io" {
		t.Fatalf("Access-Control-Allow-Origin = %q on the 308 — browsers kill this redirect (S-009)", got)
	}
}
