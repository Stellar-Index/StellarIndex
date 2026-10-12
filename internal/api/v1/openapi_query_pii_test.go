package v1

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A URL reaches access logs, proxies, CDNs, browser history and Referer, and only the edge log is ours to redact, so an
// address must never travel in a query string. The edge filter enumerates keys, so a new route is unfiltered by
// construction; these schema-driven tests pin that nothing declares or reads an address-bearing query parameter.

// personalParamPattern names the query keys that carry a person rather than a thing. It matches
// substrings, not equality, because the leak arrives as `billing_email` or `user_email` as readily
// as `email`.
var personalParamPattern = regexp.MustCompile(`(?i)(e[-_]?mail|phone|passport|national_id)`)

// TestOpenAPIDeclaresNoPersonalQueryParameter walks the published contract. A parameter in the
// spec reaches the SDK, Postman collection, explorer client and docs, so the spec is the earliest
// single place to see the whole class.
func TestOpenAPIDeclaresNoPersonalQueryParameter(t *testing.T) {
	spec := loadOpenAPISpec(t)
	if len(spec.Paths) == 0 {
		t.Fatal("spec declares no paths — the walk is broken, and a check over an empty set passes forever")
	}

	var offenders []string
	for path, item := range spec.Paths {
		for verb, op := range item {
			if op == nil {
				continue
			}
			for _, p := range op.Parameters {
				if p.In == "query" && personalParamPattern.MatchString(p.Name) {
					offenders = append(offenders, verb+" "+path+" ?"+p.Name)
				}
			}
		}
		// A templated path segment is in the URL exactly as much as a
		// query key is, so it fails the same way.
		if personalParamPattern.MatchString(path) {
			offenders = append(offenders, "path template "+path)
		}
	}
	for refName, p := range spec.Components.Parameters {
		if p.In == "query" && personalParamPattern.MatchString(p.Name) {
			offenders = append(offenders, "components.parameters."+refName+" ?"+p.Name)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("the contract puts personal data in a URL:\n  %s\n\n"+
			"A query value is logged by our edge, every proxy in front of it, the "+
			"operator's browser history and the Referer header — we control one of "+
			"those. Take the value in a request body, or carry an opaque token that "+
			"resolves to it server-side (see /v1/account/admin/lookup and "+
			"/v1/signup/verify for both shapes).",
			strings.Join(offenders, "\n  "))
	}
}

// queryReadPattern finds a handler reading a named query key.
var queryReadPattern = regexp.MustCompile(`Query\(\)(?:\.Get\(|\[)"([^"]+)"`)

// TestHandlersReadNoPersonalQueryParameter is the half the spec cannot cover: a staff route can
// exist in the server before it is in the contract, so a spec-only check stays green while the
// address is already in the access log. This reads the handlers themselves.
func TestHandlersReadNoPersonalQueryParameter(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	var offenders []string

	err := filepath.WalkDir(filepath.Join(root, "internal", "api"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // walking a repo-relative tree
		if err != nil {
			return err
		}
		scanned++
		for _, m := range queryReadPattern.FindAllStringSubmatch(string(src), -1) {
			if personalParamPattern.MatchString(m[1]) {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				offenders = append(offenders, rel+`: r.URL.Query().Get("`+m[1]+`")`)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no handler sources — the walk is broken")
	}

	if len(offenders) > 0 {
		t.Errorf("a handler reads personal data out of the URL:\n  %s\n\n"+
			"Reading it is what makes callers send it, and by the time the handler "+
			"sees the value the edge has already logged it. Take it in the request "+
			"body instead.",
			strings.Join(offenders, "\n  "))
	}
}

// repoRoot walks up from the working directory to the checkout root, identified by the OpenAPI document, like
// loadOpenAPISpec, so it works wherever `go test ./...` was invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "openapi", "stellar-index.v1.yaml")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root from cwd; this test must run inside the checkout")
	return ""
}
