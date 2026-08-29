package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoPaths points the lint at the real repo files from inside the package
// directory — same ../../../ idiom as scripts/ci/lint-openapi-urls.
func repoPaths() paths {
	return paths{
		workflowGlob: "../../../.github/workflows/*.yml",
		ciYML:        "../../../.github/workflows/ci.yml",
		makefile:     "../../../Makefile",
		config:       "../../../.golangci.yml",
		schemaDir:    "../../../scripts/ci",
	}
}

// TestRealRepo_IsClean is the regression guard for #317: it goes RED the
// moment ci.yml's golangci-lint-action step loses `verify: false` (the
// networked schema download comes back), the moment the pinned version and
// the vendored schema drift apart, or the moment .golangci.yml grows a key
// golangci-lint would silently ignore.
func TestRealRepo_IsClean(t *testing.T) {
	failures, summary, err := run(repoPaths())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(failures) > 0 {
		t.Fatalf("expected a clean repo, got %d failure(s):\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}
	for _, want := range []string{"4 of 4 checks passed", "verify:false", "golangci.v2.11.jsonschema.json"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
}

// TestRealRepo_ActionVerifyIsDisabled pins the exact ci.yml value rather than
// only the aggregate: `verify: false` on every golangci-lint-action step.
func TestRealRepo_ActionVerifyIsDisabled(t *testing.T) {
	found, failures, err := checkActionVerifyDisabled(repoPaths().workflowGlob)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if found != 1 {
		t.Errorf("expected exactly 1 %s step across .github/workflows, found %d", actionRepo, found)
	}
	if len(failures) > 0 {
		t.Errorf("golangci-lint-action still fetches its schema over the network: %s",
			strings.Join(failures, "; "))
	}
}

// TestRealRepo_VendoredSchemaMatchesPin — the vendored schema must be the one
// for the pinned release, and must be the real thing (the golangci schema is
// a closed object: additionalProperties=false is what catches typos).
func TestRealRepo_VendoredSchemaMatchesPin(t *testing.T) {
	p := repoPaths()
	wf, err := loadWorkflow(p.ciYML)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	minor, err := minorVersion(wf.Env[versionKey])
	if err != nil {
		t.Fatalf("minor version: %v", err)
	}
	schema, err := os.ReadFile(filepath.Join(p.schemaDir, "golangci."+minor+".jsonschema.json"))
	if err != nil {
		t.Fatalf("vendored schema for %s: %v", minor, err)
	}
	for _, want := range []string{`"$schema": "http://json-schema.org/draft-07/schema#"`, `"additionalProperties": false`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("vendored schema does not contain %s", want)
		}
	}
}

// TestValidateConfig_CatchesUnknownKeys is the non-vacuity proof for check 4:
// golangci-lint's own loader ACCEPTS these configs (verified against v2.11.4
// — `golangci-lint linters --config` exits 0 with a top-level `runn:` block),
// so the schema check is the only thing standing between a typo and a lint
// rule that quietly stops applying.
func TestValidateConfig_CatchesUnknownKeys(t *testing.T) {
	schema := mustReadFile(t, filepath.Join(repoPaths().schemaDir, "golangci.v2.11.jsonschema.json"))

	cases := map[string]struct {
		config string
		want   string
	}{
		"misspelt top-level section": {
			config: "version: \"2\"\nrunn:\n  timeout: 5m\n",
			want:   "top level: Additional property runn is not allowed",
		},
		"misspelt linter setting": {
			config: "version: \"2\"\nlinters:\n  settings:\n    gocyclo:\n      min-complexit: 20\n",
			want:   "linters.settings.gocyclo: Additional property min-complexit is not allowed",
		},
		"misspelt exclusion path key": {
			config: "version: \"2\"\nlinters:\n  exclusions:\n    ruless:\n      - path: _test\\.go\n",
			want:   "linters.exclusions: Additional property ruless is not allowed",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			violations, err := validateConfig(writeFile(t, "bad.yml", tc.config), schema)
			if err != nil {
				t.Fatalf("validateConfig: %v", err)
			}
			if len(violations) == 0 {
				t.Fatalf("expected a schema violation, got none")
			}
			if !strings.Contains(strings.Join(violations, "\n"), tc.want) {
				t.Errorf("violations %v do not mention %q", violations, tc.want)
			}
		})
	}
}

// TestValidateConfig_AcceptsTheRealConfig — the destructive branch above must
// not come at the cost of false positives on the config we actually ship.
func TestValidateConfig_AcceptsTheRealConfig(t *testing.T) {
	p := repoPaths()
	schema := mustReadFile(t, filepath.Join(p.schemaDir, "golangci.v2.11.jsonschema.json"))
	violations, err := validateConfig(p.config, schema)
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("real .golangci.yml reported invalid: %s", strings.Join(violations, "; "))
	}
}

// TestRun_ActionWithoutVerifyFalseFails — a workflow that leaves the action's
// networked verify on is exactly the #317 defect, and must fail the lint.
func TestRun_ActionWithoutVerifyFalseFails(t *testing.T) {
	failures := runFixture(t, workflowYAML("          args: --timeout=5m\n"), "GOLANGCI_LINT_VERSION := v2.11.4\n")
	assertFailureMentions(t, failures, "verify: false")
}

// TestRun_QuotedFalseIsAccepted — `verify: 'false'` is the same input to the
// action, and must not be reported as a failure.
func TestRun_QuotedFalseIsAccepted(t *testing.T) {
	failures := runFixture(t, workflowYAML("          verify: 'false'\n"), "GOLANGCI_LINT_VERSION := v2.11.4\n")
	if len(failures) > 0 {
		t.Errorf("quoted 'false' should satisfy the check, got: %s", strings.Join(failures, "; "))
	}
}

// TestRun_SecondWorkflowAdoptingTheActionIsCaught — the action is a shared
// primitive; a NEW workflow that adopts it with the default `verify: true`
// puts the golangci-lint.run fetch back on a different check. The lint walks
// every file under .github/workflows, not just ci.yml.
func TestRun_SecondWorkflowAdoptingTheActionIsCaught(t *testing.T) {
	p := repoPaths()
	p.ciYML = writeFile(t, "ci.yml", workflowYAML("          verify: false\n"))
	dir := filepath.Dir(p.ciYML)
	p.workflowGlob = filepath.Join(dir, "*.yml")
	p.makefile = writeFile(t, "Makefile", "GOLANGCI_LINT_VERSION := v2.11.4\n")
	if err := os.WriteFile(filepath.Join(dir, "nightly.yml"),
		[]byte(workflowYAML("          args: --timeout=5m\n")), 0o600); err != nil {
		t.Fatalf("write second workflow: %v", err)
	}

	failures, _, err := run(p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertFailureMentions(t, failures, "nightly.yml")
	assertFailureMentions(t, failures, "verify: false")
}

// TestRun_NoActionStepIsNotVacuous — if the action is renamed away, the lint
// must say so rather than report a clean run it did not verify.
func TestRun_NoActionStepIsNotVacuous(t *testing.T) {
	wf := "env:\n  GOLANGCI_LINT_VERSION: v2.11.4\njobs:\n  lint:\n    steps:\n      - name: go vet\n        run: go vet ./...\n"
	failures := runFixture(t, wf, "GOLANGCI_LINT_VERSION := v2.11.4\n")
	assertFailureMentions(t, failures, "must not pass vacuously")
}

// TestRun_VersionPinDriftFails — CI and `make lint` running different
// golangci-lint releases means one of them is being schema-checked against
// the wrong vendored copy.
func TestRun_VersionPinDriftFails(t *testing.T) {
	failures := runFixture(t, workflowYAML("          verify: false\n"), "GOLANGCI_LINT_VERSION := v2.10.0\n")
	assertFailureMentions(t, failures, "different golangci-lint releases")
}

// TestRun_MissingVendoredSchemaFails — bumping the pin without re-vendoring
// must fail loudly instead of validating against a stale schema.
func TestRun_MissingVendoredSchemaFails(t *testing.T) {
	wf := strings.Replace(workflowYAML("          verify: false\n"), "v2.11.4", "v2.99.0", 1)
	failures := runFixture(t, wf, "GOLANGCI_LINT_VERSION := v2.99.0\n")
	assertFailureMentions(t, failures, "no vendored schema for v2.99")
}

func TestMinorVersion(t *testing.T) {
	cases := map[string]string{"v2.11.4": "v2.11", "2.11.4": "v2.11", "v2.11": "v2.11"}
	for in, want := range cases {
		got, err := minorVersion(in)
		if err != nil {
			t.Fatalf("minorVersion(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("minorVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := minorVersion("latest"); err == nil {
		t.Error("minorVersion(\"latest\") should error, not guess a schema file")
	}
}

// workflowYAML builds a minimal ci.yml whose golangci-lint-action step
// carries the supplied `with:` line.
func workflowYAML(withLine string) string {
	return "env:\n  GOLANGCI_LINT_VERSION: v2.11.4\njobs:\n  lint:\n    steps:\n" +
		"      - name: golangci-lint\n        uses: golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a\n" +
		"        with:\n          version: v2.11.4\n" + withLine
}

// runFixture runs the whole lint against fixture ci.yml/Makefile files, the
// real vendored schema, and the real .golangci.yml.
func runFixture(t *testing.T, ciYML, makefile string) []string {
	t.Helper()
	p := repoPaths()
	p.ciYML = writeFile(t, "ci.yml", ciYML)
	p.workflowGlob = filepath.Join(filepath.Dir(p.ciYML), "*.yml")
	p.makefile = writeFile(t, "Makefile", makefile)
	failures, _, err := run(p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return failures
}

func assertFailureMentions(t *testing.T, failures []string, want string) {
	t.Helper()
	if len(failures) == 0 {
		t.Fatalf("expected a failure mentioning %q, got a clean run", want)
	}
	if !strings.Contains(strings.Join(failures, "\n"), want) {
		t.Errorf("failures %v do not mention %q", failures, want)
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
