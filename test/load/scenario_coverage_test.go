package load

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// explorerMount matches the ADR-0038 explorer mounts in the v1 server: every
// route served by explorerHandler reads the ClickHouse lake.
var explorerMount = regexp.MustCompile(`HandleFunc\("GET /v1(/[^"]+)",\s*s\.explorerHandler\.\w+\)`)

// scenarioURL matches the path of a `${baseUrl}/...` request in a k6
// scenario. Each segment is a literal or a single `${...}` placeholder.
var scenarioURL = regexp.MustCompile(`\$\{baseUrl\}((?:/(?:[A-Za-z0-9_.\-]+|\$\{[^}]*\}))+)`)

// TestLoadScenariosExerciseEveryExplorerRoute fails when a lake-backed
// explorer route is mounted without any k6 scenario generating traffic
// for it, so the heaviest read class cannot silently drop out of load
// testing again.
func TestLoadScenariosExerciseEveryExplorerRoute(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "api", "v1", "server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	var routes []string
	for _, m := range explorerMount.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1])
	}
	// Known case: the parser must see the movements route, or it is
	// matching nothing and the assertion below is vacuous.
	if !contains(routes, "/accounts/{g_strkey}/movements") {
		t.Fatalf("parsed %d explorer mounts from server.go and none is the movements route; "+
			"the mount block moved or its shape changed — update explorerMount", len(routes))
	}

	files, err := filepath.Glob(filepath.Join("scenarios", "[0-9]*.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no numbered k6 scenarios found (err=%v)", err)
	}
	covered := map[string]bool{}
	requests := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range scenarioURL.FindAllStringSubmatch(string(body), -1) {
			requests++
			if r, ok := mostSpecificRoute(routes, m[1]); ok {
				covered[r] = true
			}
		}
	}

	var missing []string
	for _, r := range routes {
		if !covered[r] {
			missing = append(missing, "/v1"+r)
		}
	}
	sort.Strings(missing)
	t.Logf("%d explorer routes, %d scenario request paths across %d files, %d routes uncovered",
		len(routes), requests, len(files), len(missing))
	if len(missing) > 0 {
		t.Errorf("ClickHouse-backed explorer routes with no k6 load scenario (add them to "+
			"test/load/scenarios/08-explorer-lake.js):\n  %s", strings.Join(missing, "\n  "))
	}
}

// mostSpecificRoute returns the route a request path would be served by,
// preferring the pattern with the most literal segments as net/http's
// ServeMux does (`/accounts/stats` beats `/accounts/{g_strkey}`).
func mostSpecificRoute(routes []string, path string) (string, bool) {
	req := strings.Split(strings.TrimPrefix(path, "/"), "/")
	best, bestLiterals := "", -1
	for _, r := range routes {
		pat := strings.Split(strings.TrimPrefix(r, "/"), "/")
		if n, ok := matchSegments(pat, req); ok && n > bestLiterals {
			best, bestLiterals = r, n
		}
	}
	return best, bestLiterals >= 0
}

// matchSegments reports whether req fits pat and how many of pat's segments
// are literals. A `{name}` pattern segment takes any value; a `${...}`
// request placeholder only fills a `{name}` segment.
func matchSegments(pat, req []string) (int, bool) {
	if len(pat) != len(req) {
		return 0, false
	}
	literals := 0
	for i, p := range pat {
		switch {
		case strings.HasPrefix(p, "{"):
		case p == req[i]:
			literals++
		default:
			return 0, false
		}
	}
	return literals, true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
