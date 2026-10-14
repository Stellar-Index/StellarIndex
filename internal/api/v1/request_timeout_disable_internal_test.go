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
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

func stackHasRequestTimeout(s *Server) bool {
	for _, e := range s.middlewareStack() {
		if e.name == "RequestTimeout" {
			return true
		}
	}
	return false
}

// TestRequestTimeout_ExplicitDisableOmitsMiddleware: api.request_timeout = 0 disables the blanket deadline (config
// validation skips ordering checks on that basis), so New must not map every zero to the 15s default.
func TestRequestTimeout_ExplicitDisableOmitsMiddleware(t *testing.T) {
	s := New(Options{DisableRequestTimeout: true, RequestTimeout: 20 * time.Second})
	if s.requestTimeout != 0 || stackHasRequestTimeout(s) {
		t.Errorf("DisableRequestTimeout: requestTimeout = %v, RequestTimeout middleware mounted = %v; want 0 and not mounted",
			s.requestTimeout, stackHasRequestTimeout(s))
	}
}

// TestRequestTimeout_UnsetKeepsDefault is the fail-safe half: a zero-value
// Options (every test server, any caller that forgets the field) must keep
// the default bound rather than silently run without one.
func TestRequestTimeout_UnsetKeepsDefault(t *testing.T) {
	s := New(Options{})
	if s.requestTimeout != defaultRequestTimeout || !stackHasRequestTimeout(s) {
		t.Errorf("zero Options: requestTimeout = %v, middleware mounted = %v; want %v and mounted",
			s.requestTimeout, stackHasRequestTimeout(s), defaultRequestTimeout)
	}
}

// TestRequestTimeout_StreamExemptionCannotBeForged: RequestTimeout exempts SSE by path, but r.URL.Path is DECODED while the mux
// routes on the escaped form, so `/v1/assets/native%2Fstream` ends in /stream and runs with NO deadline. Any trailing-wildcard
// route is forgeable; subjects are enumerated from the router so new routes are covered. Red against r.URL.Path.
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
			// Every wildcard gets a concrete value: a leftover literal "{name}" makes url.Parse treat RawPath as invalid and re-encode
			// Path. The trailing one carries the forgery: decoded form ends in /stream, escaped form does not.
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

// TestRequestTimeout_BoundsThePreHandlerStack: RequestTimeout must wrap outside the credential middleware (Auth through
// UsageTracker/SessionAuth), not just above CaptureRoute, or their Redis/Postgres calls are bounded only by go-redis's 3s
// default. Auth (outermost) and KeyPolicy (innermost exposed) must both see a context already carrying the deadline.
func TestRequestTimeout_BoundsThePreHandlerStack(t *testing.T) {
	authProbe, policyProbe := &deadlineProbe{}, &deadlineProbe{}

	srv := New(Options{
		Auth:           authProbe.middleware,
		KeyPolicy:      policyProbe.middleware,
		RequestTimeout: 3 * time.Second,
	})

	rec := httptest.NewRecorder()
	// An existing, always-mounted route — no new registration, so the
	// route↔OpenAPI inventory lint stays satisfied.
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if !authProbe.saw {
		t.Error("the Auth middleware saw NO deadline: the whole credential/quota/limit block " +
			"runs outside RequestTimeout, so its Redis/Postgres I/O is unbounded by it")
	}
	if !policyProbe.saw {
		t.Error("the KeyPolicy middleware saw NO deadline")
	}
	if authProbe.saw && (authProbe.remaining <= 0 || authProbe.remaining > 3*time.Second) {
		t.Errorf("Auth deadline is %v out, want (0, 3s] — it must be the request timeout, not something looser",
			authProbe.remaining)
	}
}

// TestRequestTimeout_StreamingStaysExempt: SSE routes are long-lived by design; moving the timeout outward must not sever
// them. The probe sits in Auth, ahead of the mux, so the SSE handler itself isn't exercised.
func TestRequestTimeout_StreamingStaysExempt(t *testing.T) {
	probe := &deadlineProbe{}
	srv := New(Options{Auth: probe.middleware, RequestTimeout: 3 * time.Second})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ledger/stream", nil))
	if probe.saw {
		t.Error("an SSE path inherited a request deadline — a real stream would be severed mid-flight")
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

// routePatternsFromTree walks non-test sources under internal/api/v1 and returns routes (any method) ending in a
// {wildcard} plus the real SSE routes; any call with a mux-spec literal first argument counts (HandleFunc, Handle,
// handlePublic).
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
		if route, _, ok := muxSpecCall(n); ok {
			out = append(out, route)
		}
		return true
	})
	return out
}

// deadlineProbe records whether the request context already carries a
// deadline by the time this middleware runs.
type deadlineProbe struct {
	saw       bool
	remaining time.Duration
}

func (p *deadlineProbe) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dl, ok := r.Context().Deadline()
		p.saw = ok
		if ok {
			p.remaining = time.Until(dl)
		}
		next.ServeHTTP(w, r)
	})
}
