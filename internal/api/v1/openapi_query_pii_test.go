package v1

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A URL is the least private thing an HTTP request has. It is written
// to our edge's access log, to every proxy and CDN in front of it, to
// the operator's browser history, and it leaves on the Referer header
// of whatever the page loads next. Only the first of those is ours to
// redact.
//
// THE DEFECT THESE PIN. `GET /v1/account/admin/lookup?email=<customer>`
// put a real customer's address in all four, and the app layer had
// deliberately kept that same address OUT of the audit row — the URL
// defeated the care taken one layer down. The remediation moved the
// term into a POST body, and the edge grew a redaction filter.
//
// Neither of those is what stops it happening AGAIN, which is what
// these two tests are for. The edge filter is a field enumeration: it
// masks the query keys someone thought of, so the next route to put an
// address in a URL is unfiltered by construction — the same way the
// original filter covered `token` and was already blind to `email` when
// the look-up shipped. The tests below are the schema-driven half:
// nothing may DECLARE an address-bearing query parameter, and no
// handler may READ one, whether or not anybody remembered to extend a
// list at the edge.

// personalParamPattern names the query keys that carry a person rather
// than a thing. Substring matching, not equality, because the leak
// arrives as `billing_email` or `user_email` at least as readily as
// `email` — the enumeration that missed the first one is the failure
// mode being closed here.
var personalParamPattern = regexp.MustCompile(`(?i)(e[-_]?mail|phone|passport|national_id)`)

// TestOpenAPIDeclaresNoPersonalQueryParameter walks the published
// contract. A parameter that reaches the spec has reached the SDK, the
// Postman collection, the generated explorer client and the docs
// site — so the spec is the earliest place the whole class is visible
// in one file.
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

// TestHandlersReadNoPersonalQueryParameter is the half the spec cannot
// cover. The look-up that leaked was a staff route: it existed in the
// server before it existed in the contract, so a spec-only check would
// have gone green while the address was already in the access log. This
// reads the handlers themselves.
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

// repoRoot walks up from the test's working directory to the checkout
// root, identified by the OpenAPI document. Same approach as
// loadOpenAPISpec, which has to work from wherever `go test ./...` was
// invoked.
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
