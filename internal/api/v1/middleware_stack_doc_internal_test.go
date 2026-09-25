package v1

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// documentedMiddlewareOrder reads the stack order from middleware/doc.go:
// the tab-indented comment block whose lines carry "→".
func documentedMiddlewareOrder(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("middleware", "doc.go"))
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "//\t") && strings.Contains(line, "→") {
			block = append(block, strings.TrimPrefix(line, "//\t"))
		}
	}
	var names []string
	for _, n := range strings.Split(strings.Join(block, " "), "→") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestMiddlewareStackMatchesPackageDoc: middleware/doc.go is the one
// document that claims to give the request-path order, and it drifted to
// half the stack. With every optional middleware wired, the stack the
// server builds must be exactly the documented list.
func TestMiddlewareStackMatchesPackageDoc(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	var optional middleware.Middleware = pass
	s := &Server{
		mux:                  http.NewServeMux(),
		publicRoutes:         middleware.NewPublicRoutes(),
		requestTimeout:       time.Second,
		cors:                 optional,
		auth:                 optional,
		keyPolicy:            optional,
		requireEmailVerified: optional,
		usageTracker:         optional,
		monthlyQuota:         optional,
		rateLimit:            optional,
		touchUsage:           optional,
		sessionAuth:          optional,
	}
	var built []string
	for _, e := range s.middlewareStack() {
		built = append(built, e.name)
	}
	documented := documentedMiddlewareOrder(t)
	if len(documented) == 0 {
		t.Fatal("parsed no names from middleware/doc.go — the parser is not finding the order block")
	}
	if !slices.Equal(built, documented) {
		t.Fatalf("middleware/doc.go order drifted from Server.middlewareStack:\n built:      %v\n documented: %v", built, documented)
	}
}
