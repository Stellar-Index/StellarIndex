// lint-golangci-config validates .golangci.yml against a VENDORED JSON
// Schema, and guards the wiring that keeps that validation offline.
//
// Why (#317): golangci-lint-action defaults `verify: true`, which makes the
// action run `golangci-lint config verify`. That command downloads
// https://golangci-lint.run/jsonschema/golangci.v<major>.<minor>.jsonschema.json
// on every invocation, so a hiccup at a third-party site turns the REQUIRED
// `lint` check red on a diff that touched no Go at all — observed 2026-08-28
// on PR #275 (`read: connection reset by peer`), green on rerun. Reproduced
// locally against v2.11.4 with the network blocked:
//
//	$ HTTPS_PROXY=http://127.0.0.1:9 golangci-lint config verify
//	The command is terminated due to an error: [.golangci.yml] validate:
//	compile schema: failing loading "https://golangci-lint.run/jsonschema/
//	golangci.v2.11.jsonschema.json" … connect: connection refused   (exit 3)
//
// The schema check itself is worth keeping, so this is NOT a `verify: false`
// and walk away. golangci-lint's own config loader silently IGNORES unknown
// keys — verified against v2.11.4, where a top-level `runn:` block is
// accepted by `golangci-lint run`/`linters` and rejected ONLY by
// `config verify`. (The converse holds too — an unknown linter NAME is
// rejected by `run` and passes the schema — so the two checks are
// complementary, not redundant.) A misspelt setting is therefore a lint
// rule that quietly stops applying, which is exactly the class
// .golangci.yml's own header ("we don't weaken lint just to unblock
// something") exists to prevent. So the check moves off the network
// instead of off the tree: the schema is vendored next to this program and
// validated locally, on every PR, with no third-party dependency.
//
// Four checks, all deterministic and network-free:
//
//  1. Every golangci-lint-action step in ci.yml sets `verify: false` — one
//     that doesn't would fetch the schema again and make this lint
//     decoration. Fails too when no such step exists at all, so the check
//     can never pass vacuously.
//  2. The golangci-lint version pinned in ci.yml (GOLANGCI_LINT_VERSION) and
//     in the Makefile agree — CI and `make lint` must schema-check against
//     the same release the vendored copy belongs to.
//  3. A vendored schema exists for that pinned MINOR. A version bump that
//     forgets to re-vendor fails here, rather than silently validating a new
//     config against an old schema.
//  4. .golangci.yml validates against that schema.
//
// Re-vendor after a version bump (the tag's `golangci.next.jsonschema.json`
// is byte-identical to the golangci-lint.run copy the CLI would fetch —
// verified for v2.11.4, sha256
// 985af311f9448d5b0964c3eda502204326dcf35d8f757192684cddc9b6615676):
//
//	curl -fsSL -o scripts/ci/golangci.v<maj>.<min>.jsonschema.json \
//	  https://raw.githubusercontent.com/golangci/golangci-lint/<tag>/jsonschema/golangci.next.jsonschema.json
//
// Usage (from the repo root):
//
//	go run ./scripts/ci/lint-golangci-config
//
// Exits 0 when all four checks pass, 1 on any failure.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/xeipuuv/gojsonschema"
	"gopkg.in/yaml.v3"
)

// actionRepo is the action whose `verify` input pulls the schema over the
// network. Matched on the repo part only — the pin is a SHA, and the
// SHA-pinning policy is lint-actions-pinning.sh's job, not this lint's.
const actionRepo = "golangci/golangci-lint-action"

// versionKey is the env var ci.yml and the Makefile both use to pin the
// golangci-lint release.
const versionKey = "GOLANGCI_LINT_VERSION"

// paths are the files this lint reads. Overridable so the tests can point
// each check at a fixture without a temp repo — same idiom as
// check-verify-parity.sh's CI_YML/VERIFY_SH overrides.
type paths struct {
	// workflowGlob covers EVERY workflow, not just ci.yml: the action is a
	// shared primitive, and a second workflow adopting it with the default
	// `verify: true` would put the fetch back on a different check.
	workflowGlob string
	ciYML        string
	makefile     string
	config       string
	schemaDir    string
}

func defaultPaths() paths {
	return paths{
		workflowGlob: ".github/workflows/*.yml",
		ciYML:        ".github/workflows/ci.yml",
		makefile:     "Makefile",
		config:       ".golangci.yml",
		schemaDir:    "scripts/ci",
	}
}

func main() {
	failures, summary, err := run(defaultPaths())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint-golangci-config: FAULT — %v\n", err)
		os.Exit(1)
	}
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "lint-golangci-config: FAIL — %d problem(s):\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		fmt.Fprintf(os.Stderr, "\nSee the header of scripts/ci/lint-golangci-config/main.go (#317).\n")
		os.Exit(1)
	}
	fmt.Println(summary)
}

// run executes the four checks and returns one message per failure. A
// non-nil error means a check could not be EVALUATED (missing/malformed
// input) — that is a fault, not a clean run.
func run(p paths) (failures []string, summary string, err error) {
	// 1. no workflow may leave the action's networked verify on.
	steps, verifyFailures, err := checkActionVerifyDisabled(p.workflowGlob)
	if err != nil {
		return nil, "", err
	}
	failures = append(failures, verifyFailures...)

	wf, err := loadWorkflow(p.ciYML)
	if err != nil {
		return nil, "", err
	}

	// 2. ci.yml and the Makefile must pin the same release.
	ciVersion := wf.Env[versionKey]
	if ciVersion == "" {
		return nil, "", fmt.Errorf("%s: no %s in the workflow env block", p.ciYML, versionKey)
	}
	makeVersion, err := makefileVersion(p.makefile)
	if err != nil {
		return nil, "", err
	}
	if ciVersion != makeVersion {
		failures = append(failures, fmt.Sprintf(
			"%s pins %s=%s but %s pins %s — CI and `make lint` would run different golangci-lint releases, and only one of them matches the vendored schema",
			p.ciYML, versionKey, ciVersion, p.makefile, makeVersion))
	}

	// 3. a vendored schema must exist for the pinned minor.
	minor, err := minorVersion(ciVersion)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", p.ciYML, err)
	}
	schemaPath := filepath.Join(p.schemaDir, fmt.Sprintf("golangci.%s.jsonschema.json", minor))
	schema, err := os.ReadFile(schemaPath) //nolint:gosec // path is derived from the repo's own pinned version, not user input
	if err != nil {
		failures = append(failures, fmt.Sprintf(
			"golangci-lint is pinned at %s but no vendored schema for %s: %v — re-vendor it (see the header of scripts/ci/lint-golangci-config/main.go)",
			ciVersion, minor, err))
		return failures, "", nil
	}

	// 4. the config must validate against it.
	violations, err := validateConfig(p.config, schema)
	if err != nil {
		return nil, "", err
	}
	for _, v := range violations {
		failures = append(failures, fmt.Sprintf("%s: %s", p.config, v))
	}

	summary = fmt.Sprintf(
		"lint-golangci-config: OK — 4 of 4 checks passed (%d golangci-lint-action step(s) in %s, all with verify:false; %s pinned in %s + %s; %s validates against %s)",
		steps, p.workflowGlob, ciVersion, p.ciYML, p.makefile, p.config, schemaPath)
	return failures, summary, nil
}

// workflow is the slice of a GitHub Actions workflow this lint cares about.
type workflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Steps []struct {
			Name string         `yaml:"name"`
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(path string) (*workflow, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo path (or a test fixture), not user input
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &wf, nil
}

// checkActionVerifyDisabled walks every workflow matching glob and returns
// how many golangci-lint-action steps were found plus a message per step that
// leaves the networked `verify` on. Zero steps across all workflows is itself
// a failure: this lint's premise is that the action runs somewhere with its
// verify disabled, and a rename must not read as "clean".
func checkActionVerifyDisabled(glob string) (int, []string, error) {
	files, err := filepath.Glob(glob)
	if err != nil {
		return 0, nil, fmt.Errorf("glob %s: %w", glob, err)
	}
	sort.Strings(files)

	var (
		found    int
		failures []string
	)
	for _, file := range files {
		wf, err := loadWorkflow(file)
		if err != nil {
			return 0, nil, err
		}
		for _, jobName := range sortedKeys(wf.Jobs) {
			for _, step := range wf.Jobs[jobName].Steps {
				if repoOf(step.Uses) != actionRepo {
					continue
				}
				found++
				if !isFalse(step.With["verify"]) {
					failures = append(failures, fmt.Sprintf(
						"%s: job %q step %q uses %s without `verify: false` — the action would run `golangci-lint config verify`, which downloads the schema from golangci-lint.run and reddens this required check whenever that site hiccups (#317). The offline equivalent is this lint.",
						file, jobName, step.Name, actionRepo))
				}
			}
		}
	}
	if found == 0 {
		failures = append(failures, fmt.Sprintf(
			"no %s step found in %s — this lint's premise (the action runs, with its networked verify disabled) no longer holds; it must not pass vacuously",
			actionRepo, glob))
	}
	return found, failures, nil
}

// repoOf strips the `@<ref>` pin from a `uses:` value.
func repoOf(uses string) string {
	repo, _, _ := strings.Cut(uses, "@")
	return repo
}

// isFalse accepts both YAML forms an action input can take: a bare `false`
// (decoded as a bool) and a quoted `'false'` (a string), which the action's
// own core.getBooleanInput treats identically.
func isFalse(v any) bool {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "false")
	default:
		return false
	}
}

var makefileVersionRE = regexp.MustCompile(`(?m)^` + versionKey + `\s*[:?]?=\s*(\S+)`)

func makefileVersion(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo path (or a test fixture), not user input
	if err != nil {
		return "", fmt.Errorf("read makefile: %w", err)
	}
	m := makefileVersionRE.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%s: no %s assignment", path, versionKey)
	}
	return string(m[1]), nil
}

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.\d+)?`)

// minorVersion turns a pinned release (v2.11.4) into the schema's version
// segment (v2.11) — golangci-lint publishes one schema per MINOR.
func minorVersion(version string) (string, error) {
	m := semverRE.FindStringSubmatch(version)
	if m == nil {
		return "", fmt.Errorf("cannot read a major.minor out of %s=%q", versionKey, version)
	}
	return fmt.Sprintf("v%s.%s", m[1], m[2]), nil
}

// validateConfig checks the golangci-lint config against the vendored schema
// and returns one message per violation. This is the same validation
// `golangci-lint config verify` performs, minus the download.
func validateConfig(configPath string, schema []byte) ([]string, error) {
	raw, err := os.ReadFile(configPath) //nolint:gosec // fixed repo path (or a test fixture), not user input
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	// The schema is JSON Schema; yaml.v3 decodes mappings into
	// map[string]any, so a round-trip through JSON is lossless here.
	asJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode %s as JSON: %w", configPath, err)
	}
	result, err := gojsonschema.Validate(
		gojsonschema.NewBytesLoader(schema),
		gojsonschema.NewBytesLoader(asJSON),
	)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", configPath, err)
	}
	if result.Valid() {
		return nil, nil
	}
	violations := make([]string, 0, len(result.Errors()))
	for _, e := range result.Errors() {
		where := e.Field()
		if where == "(root)" {
			where = "top level"
		}
		violations = append(violations, fmt.Sprintf("%s: %s", where, e.Description()))
	}
	sort.Strings(violations)
	return violations, nil
}

// sortedKeys keeps failure output deterministic across map iterations.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
