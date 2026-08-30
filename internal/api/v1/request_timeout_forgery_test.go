package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// TestRequestTimeout_StreamExemptionCannotBeForged is the wave-D
// UNAUTH-DOS-4 regression, and it derives its own subject set from the
// router so future routes are covered without a second edit.
//
// RequestTimeout exempts SSE endpoints by the `/stream` path suffix.
// It used to test that suffix against r.URL.Path — the DECODED path —
// while Go's mux routes on the ESCAPED form. Those disagree exactly
// when a wildcard segment contains a percent-encoded slash:
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
// Enumerating the routes from server.go rather than listing them here is
// the point: the forgery works against EVERY trailing-wildcard route,
// and new ones are added regularly. A hand-written table would pin
// today's routes and silently miss tomorrow's.
//
// Proven red against r.URL.Path: every trailing-wildcard route below
// loses its deadline.
func TestRequestTimeout_StreamExemptionCannotBeForged(t *testing.T) {
	wildcard, streams := routePatternsFromServer(t)
	if len(wildcard) == 0 {
		t.Fatal("no trailing-wildcard routes found — the pattern scan is broken, " +
			"and a guard that finds nothing to check silently passes forever")
	}

	for _, pattern := range wildcard {
		t.Run(pattern, func(t *testing.T) {
			// Every wildcard gets a concrete value — a leftover literal
			// "{name}" would make url.Parse treat RawPath as invalid and
			// fall back to re-encoding the decoded Path, which is a
			// property of the test URL rather than of the middleware.
			// The TRAILING one carries the forgery: a value whose decoded
			// form ends in /stream but whose escaped form does not.
			path := anyWildcard.ReplaceAllString(pattern, "x")
			path = strings.TrimSuffix(path, "/x") + "/x%2Fstream"
			probe := requestDeadlineAt(t, path)
			if !probe.saw {
				t.Errorf("%s served with NO request deadline when its wildcard was "+
					"set to x%%2Fstream — the SSE exemption was forged by a "+
					"percent-encoded slash. Key the exemption on r.URL.EscapedPath(), "+
					"which is what the mux itself routes on.", path)
			}
		})
	}

	// The control: real SSE routes must KEEP their exemption. A fix that
	// bounded the streams too would be worse than the bug.
	for _, pattern := range streams {
		t.Run(pattern, func(t *testing.T) {
			if probe := requestDeadlineAt(t, pattern); probe.saw {
				t.Errorf("%s got a request deadline — a genuine SSE stream would be "+
					"severed mid-flight", pattern)
			}
		})
	}
}

// requestDeadlineAt runs path through the RequestTimeout middleware and
// reports whether a deadline reached the handler.
func requestDeadlineAt(t *testing.T, path string) *deadlineProbe {
	t.Helper()
	probe := &deadlineProbe{}
	h := middleware.RequestTimeout(time.Second)(
		probe.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
	)
	req := httptest.NewRequest(http.MethodGet, path, nil)
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

// routePatternsFromServer parses server.go and returns the GET routes
// whose final segment is a {wildcard}, plus the real SSE routes.
func routePatternsFromServer(t *testing.T) (wildcard, streams []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		spec := strings.Trim(lit.Value, `"`)
		m := muxRoute.FindStringSubmatch(spec)
		if m == nil || m[1] != http.MethodGet {
			return true
		}
		path := m[2]
		if seen[path] {
			return true
		}
		seen[path] = true
		switch {
		case strings.HasSuffix(path, "/stream"):
			streams = append(streams, path)
		case trailingWildcard.MatchString(path):
			wildcard = append(wildcard, path)
		}
		return true
	})
	return wildcard, streams
}
