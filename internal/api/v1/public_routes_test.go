package v1

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestPublicOpenAPIRoutes_AnswerWithoutCredential pins #1314: every
// operation the spec declares `security: []` must answer an uncredentialed
// caller with something other than 401 under the credential-REQUIRED modes.
// Under auth_mode=sep10 the SEP-10 challenge/token routes 401'd, so the mode
// could not issue the credential it demanded; under apikey, signup, register
// and the emailed verification link 401'd.
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
