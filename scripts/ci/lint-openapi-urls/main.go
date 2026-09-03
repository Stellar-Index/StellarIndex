// Lint enforcing ADR-0018's URL discipline on the OpenAPI spec.
//
// The three API consistency surfaces — closed-bucket, tip,
// observations — are distinguishable only by URL. ADR-0018
// §"URL discipline as the contract enforcer" says:
//
//	"Query parameters MUST NOT change a surface's consistency
//	 contract. ?freshness=tip on /v1/price is prohibited by this
//	 ADR — tip semantics require the /v1/price/tip URL."
//
// This linter walks every operation's query parameters and rejects
// any that look like they're selecting a consistency tier rather
// than refining a request within one. Two rules:
//
//  1. Forbidden parameter NAMES — `freshness`, `consistency`,
//     `surface`, `tier`. These are the literal pattern the ADR
//     prohibits, regardless of what their enum says.
//
//  2. Forbidden parameter ENUMS — any query parameter whose enum
//     contains TWO OR MORE values from a known set of consistency-tier
//     names (`closed`, `tip`, `latest`, `raw`, `observations`,
//     `bucketed`, `live`). One value is fine — `enum: [tip]` on a
//     param could be a stub. Two+ means the param is selecting BETWEEN
//     surfaces.
//
// Single-value enums and unrelated enums (e.g. `vwap|twap`,
// `native|classic|soroban|fiat`) pass cleanly.
//
// A third rule guards the other half of the spec's URLs — the
// `servers:` block:
//
//  3. Every server entry must name a host this project actually
//     serves. A server entry is not prose: every generated client,
//     Postman import and try-it console offers it as a selectable
//     target. `https://api.staging.stellarindex.io/v1` shipped here
//     with no DNS record whatsoever, so anyone importing the spec got
//     a dead entry in their picker.
//
// Usage:
//
//	go run ./scripts/ci/lint-openapi-urls openapi/stellar-index.v1.yaml
//
// Exits 0 on clean, 1 on any rule failure (with a list of offending
// path/parameter pairs to stderr).
package main

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	forbiddenNames = map[string]bool{
		"freshness":   true,
		"consistency": true,
		"surface":     true,
		"tier":        true,
	}

	tierEnumValues = map[string]bool{
		"closed":       true,
		"tip":          true,
		"latest":       true,
		"raw":          true,
		"observations": true,
		"bucketed":     true,
		"live":         true,
	}

	// servedHosts is the set of public hosts a `servers:` entry may
	// name. Adding one is a deliberate edit here, which is the point:
	// declaring a server promises every importer of this spec that the
	// host answers. Loopback entries (see isLoopback) are exempt —
	// they are meant to be edited by the reader, not called as-is.
	servedHosts = map[string]bool{
		"api.stellarindex.io": true,
	}
)

// param is the subset of an OpenAPI 3.1 parameter object we need to
// reason about. Inline params and `$ref` params share this shape
// after one resolution pass.
type param struct {
	Ref    string `yaml:"$ref"`
	Name   string `yaml:"name"`
	In     string `yaml:"in"`
	Schema struct {
		Type string   `yaml:"type"`
		Enum []string `yaml:"enum"`
	} `yaml:"schema"`
}

// operation captures the parameter list at one (path, verb).
type operation struct {
	Parameters []param `yaml:"parameters"`
}

// server is one entry of the top-level `servers:` block.
type server struct {
	URL         string `yaml:"url"`
	Description string `yaml:"description"`
}

// spec is the trimmed view we walk.
type spec struct {
	Servers    []server                        `yaml:"servers"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		Parameters map[string]param `yaml:"parameters"`
	} `yaml:"components"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: lint-openapi-urls <openapi-spec>")
		os.Exit(2)
	}
	specPath := os.Args[1]
	data, err := os.ReadFile(specPath) //nolint:gosec // CI tool — operator-supplied spec path is the whole point
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", specPath, err)
		os.Exit(2)
	}
	var s spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", specPath, err)
		os.Exit(2)
	}

	// Fail-closed on an empty path set: parseable YAML with no `paths:` block
	// would otherwise walk zero operations and print "comply" (exit 0) — a
	// vacuous pass that hides a wrong file or a spec that lost its paths.
	// Mirrors verify-launch-ready's zero-rows guard.
	if len(s.Paths) == 0 {
		fmt.Fprintf(os.Stderr, "openapi-urls: %s declared zero paths — refusing to pass vacuously (wrong file or missing `paths:` block?)\n", specPath)
		os.Exit(2)
	}

	// Same guard for `servers:`. Losing the block does not fail any
	// consumer loudly — clients fall back to the spec's own location or
	// to a relative base — so it would go unnoticed, and rule 3 would
	// pass over zero entries while doing so.
	if len(s.Servers) == 0 {
		fmt.Fprintf(os.Stderr, "openapi-urls: %s declared zero servers — refusing to pass vacuously (missing `servers:` block?)\n", specPath)
		os.Exit(2)
	}

	violations := lint(&s)
	if len(violations) == 0 {
		fmt.Println("openapi-urls: servers and query parameters both comply")
		os.Exit(0)
	}

	sort.Strings(violations)
	fmt.Fprintln(os.Stderr, "openapi-urls: URL violations found:")
	fmt.Fprintln(os.Stderr)
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, "  "+v)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Query parameters MUST NOT change a surface's consistency contract.")
	fmt.Fprintln(os.Stderr, "Use a separate URL (/v1/price vs /v1/price/tip vs /v1/observations) instead.")
	fmt.Fprintln(os.Stderr, "See docs/adr/0018-api-consistency-surfaces.md.")
	fmt.Fprintln(os.Stderr, "A `servers:` entry must name a host that answers — see servedHosts in this linter.")
	os.Exit(1)
}

// lint walks every (path, verb, query parameter) tuple and returns a
// slice of human-readable violation strings.
func lint(s *spec) []string {
	var violations []string

	violations = append(violations, checkServers(s.Servers)...)

	for path, verbs := range s.Paths {
		for verb, op := range verbs {
			for _, p := range op.Parameters {
				resolved, ok := resolve(s, p)
				if !ok {
					// $ref to a non-existent component — that's a
					// separate spec-validity problem, surfaced by
					// Spectral. Skip here.
					continue
				}
				if resolved.In != "query" {
					continue
				}
				for _, msg := range checkParam(resolved) {
					violations = append(violations,
						fmt.Sprintf("%s %s — query param %q: %s",
							strings.ToUpper(verb), path, resolved.Name, msg))
				}
			}
		}
	}
	return violations
}

// checkServers runs rule 3 over the `servers:` block: one violation
// per entry naming a host outside servedHosts.
func checkServers(servers []server) []string {
	var violations []string
	for _, srv := range servers {
		u, err := url.Parse(srv.URL)
		if err != nil || u.Host == "" {
			violations = append(violations,
				fmt.Sprintf("servers — %q is not a parseable absolute URL; a client cannot dial it", srv.URL))
			continue
		}
		host := u.Hostname()
		if isLoopback(host) || servedHosts[strings.ToLower(host)] {
			continue
		}
		violations = append(violations,
			fmt.Sprintf("servers — %q names host %q, which this project does not serve; a server entry is offered as a selectable target by every generated client, so an unserved host hands each importer a dead endpoint. Deploy it and add the host to servedHosts, or drop the entry",
				srv.URL, host))
	}
	return violations
}

// isLoopback reports whether host is the self-hosted/dev placeholder
// shape. These entries exist to be edited by the reader, so they are
// exempt from the served-host list.
func isLoopback(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// resolve handles inline params (returned as-is) and `$ref` params
// (looked up in components.parameters). Returns ok=false when a
// `$ref` points at a missing component.
func resolve(s *spec, p param) (param, bool) {
	if p.Ref == "" {
		return p, true
	}
	const prefix = "#/components/parameters/"
	if !strings.HasPrefix(p.Ref, prefix) {
		return param{}, false
	}
	name := strings.TrimPrefix(p.Ref, prefix)
	target, ok := s.Components.Parameters[name]
	return target, ok
}

// checkParam runs both rules and returns one message per failure
// (zero messages = clean). Multiple rules can fire on the same param.
func checkParam(p param) []string {
	var msgs []string
	if forbiddenNames[strings.ToLower(p.Name)] {
		msgs = append(msgs,
			fmt.Sprintf("name is on the prohibited list (%v) — these names imply selecting between consistency surfaces, which must be done by URL not by query parameter",
				sortedKeys(forbiddenNames)))
	}
	if hits := tierHits(p.Schema.Enum); len(hits) >= 2 {
		msgs = append(msgs,
			fmt.Sprintf("enum contains multiple consistency-tier values %v — a single query parameter selecting between tiers is exactly the pattern ADR-0018 prohibits",
				hits))
	}
	return msgs
}

// tierHits returns the subset of `enum` values that match a known
// consistency-tier name. Order-stable so error messages are
// deterministic.
func tierHits(enum []string) []string {
	var hits []string
	for _, v := range enum {
		if tierEnumValues[strings.ToLower(v)] {
			hits = append(hits, v)
		}
	}
	return hits
}

// sortedKeys returns a deterministic, alphabetised list of map keys
// for use in error messages.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
