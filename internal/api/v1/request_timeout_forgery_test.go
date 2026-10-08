package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// TestRequestTimeout_StreamExemptionCannotBeForged is a
// regression test, and it derives its own subject set from the
// router so future routes are covered without a second edit.
//
// RequestTimeout exempts SSE endpoints by path, and a path test can be
// forged two ways. A `/stream` suffix match is met by setting a trailing
// wildcard to "stream". And r.URL.Path is DECODED while Go's mux routes on
// the ESCAPED form; those disagree exactly when a wildcard segment
// contains a percent-encoded slash:
//
//	GET /v1/assets/native%2Fstream
//	  → routes to "GET /v1/assets/{asset_id}", asset_id="native/stream"
//	  → r.URL.Path       = "/v1/assets/native/stream"   (ends /stream)
//	  → r.URL.EscapedPath = "/v1/assets/native%2Fstream" (does not)
//
// So any route ending in a wildcard could be asked to run with NO
// request deadline at all — the one thing this middleware exists to
// guarantee.
//
// Enumerating the routes from source rather than listing them here is
// the point: the forgery works against EVERY trailing-wildcard route, of
// any method, on the one mux — server.go's own registrations and the
// sub-packages' Mount calls alike — and new ones are added regularly. A
// hand-written table would pin today's routes and silently miss
// tomorrow's.
//
// Proven red against r.URL.Path: every trailing-wildcard route below
// loses its deadline.
func TestRequestTimeout_StreamExemptionCannotBeForged(t *testing.T) {
	wildcard, streams := routePatternsFromTree(t)
	if len(wildcard) == 0 {
		t.Fatal("no trailing-wildcard routes found — the pattern scan is broken, " +
			"and a guard that finds nothing to check silently passes forever")
	}
	// Out-of-scope canaries: a sub-package route registered through
	// mux.Handle, and a non-GET route in server.go. A scan narrowed back to
	// server.go's GET HandleFunc calls would drop both.
	for _, must := range []mountedRoute{
		{http.MethodPatch, "/v1/dashboard/price-alerts/{id}"},
		{http.MethodDelete, "/v1/account/keys/{keyID}"},
	} {
		if !slices.Contains(wildcard, must) {
			t.Fatalf("route scan missed %s %s — it no longer covers every trailing-wildcard route on the v1 mux",
				must.method, must.path)
		}
	}

	for _, route := range wildcard {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			// Every wildcard gets a concrete value — a leftover literal
			// "{name}" would make url.Parse treat RawPath as invalid and
			// fall back to re-encoding the decoded Path, which is a
			// property of the test URL rather than of the middleware.
			// The TRAILING one carries the forgery: a value whose decoded
			// form ends in /stream but whose escaped form does not.
			path := anyWildcard.ReplaceAllString(route.path, "x")
			path = strings.TrimSuffix(path, "/x") + "/x%2Fstream"
			probe := requestDeadlineAt(t, route.method, path)
			if !probe.saw {
				t.Errorf("%s %s served with NO request deadline when its wildcard was "+
					"set to x%%2Fstream — the SSE exemption was forged by a "+
					"percent-encoded slash. Key the exemption on r.URL.EscapedPath(), "+
					"which is what the mux itself routes on.", route.method, path)
			}
			// The plain value needs no encoding at all.
			plain := strings.TrimSuffix(anyWildcard.ReplaceAllString(route.path, "x"), "/x") + "/stream"
			if probe := requestDeadlineAt(t, route.method, plain); !probe.saw {
				t.Errorf("%s %s served with NO request deadline when its wildcard was "+
					"set to \"stream\" — the SSE exemption must be the exact set of "+
					"SSE routes, not a path-suffix match.", route.method, plain)
			}
		})
	}

	// The control: real SSE routes must KEEP their exemption. A fix that
	// bounded the streams too would be worse than the bug.
	for _, pattern := range streams {
		t.Run(pattern, func(t *testing.T) {
			if probe := requestDeadlineAt(t, http.MethodGet, pattern); probe.saw {
				t.Errorf("%s got a request deadline — a genuine SSE stream would be "+
					"severed mid-flight", pattern)
			}
		})
	}
}

// requestDeadlineAt runs method+path through the RequestTimeout
// middleware and reports whether a deadline reached the handler.
func requestDeadlineAt(t *testing.T, method, path string) *deadlineProbe {
	t.Helper()
	probe := &deadlineProbe{}
	h := middleware.RequestTimeout(time.Second)(
		probe.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
	)
	req := httptest.NewRequest(method, path, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return probe
}

var (
	// The mux registration spec: a method, a space, then the path.
	muxRoute = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH) (/\S+)$`)
	// A pattern whose FINAL segment is a wildcard.
	trailingWildcard = regexp.MustCompile(`\{[^}]+\}$`)
	// Any wildcard segment.
	anyWildcard = regexp.MustCompile(`\{[^}]+\}`)
)

type mountedRoute struct{ method, path string }

// routePatternsFromTree walks every non-test source under internal/api/v1
// — server.go and each sub-package that mounts onto the same mux — and
// returns the routes, of any method, whose final segment is a {wildcard},
// plus the real SSE routes. Any call whose first argument is a mux spec
// literal counts, so HandleFunc, Handle and handlePublic are all seen.
func routePatternsFromTree(t *testing.T) (wildcard []mountedRoute, streams []string) {
	t.Helper()
	fset := token.NewFileSet()
	seen := map[mountedRoute]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, r := range muxSpecsIn(f) {
			if seen[r] {
				continue
			}
			seen[r] = true
			switch {
			case r.method == http.MethodGet && strings.HasSuffix(r.path, "/stream"):
				streams = append(streams, r.path)
			case trailingWildcard.MatchString(r.path):
				wildcard = append(wildcard, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api/v1: %v", err)
	}
	return wildcard, streams
}

// muxSpecsIn returns every "METHOD /path" string literal passed as the
// first argument of a call in f.
func muxSpecsIn(f *ast.File) []mountedRoute {
	var out []mountedRoute
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		spec, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if m := muxRoute.FindStringSubmatch(spec); m != nil {
			out = append(out, mountedRoute{m[1], m[2]})
		}
		return true
	})
	return out
}
