package config_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

// credentialEnvRead matches a literal read of an env var whose name marks
// it as a credential. Reads through a variable (StorageConfig's *_env
// name-indirection) are deliberately out of reach: the NAME is in config.
var credentialEnvRead = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"([A-Z][A-Z0-9_]*(?:_KEY|_SECRET|_PASSWORD|_TOKEN|_DSN))"\s*\)`)

// rawCredentialEnvAllowed lists the reads that cannot go through the
// schema, keyed "<repo-relative file>:<VAR>", each with the reason.
var rawCredentialEnvAllowed = map[string]string{
	"cmd/stellarindex-sla-probe/main.go:STELLARINDEX_PROBE_API_KEY": "the probe takes no config file; the var is the documented default of -api-key, kept off the command line",
	"cmd/stellarindex-migrate/main.go:STELLARINDEX_POSTGRES_DSN":    "migrate runs without a config file; it reads the same var the schema declares for storage.postgres_dsn",
}

// TestNoRawCredentialEnvReadsOutsideConfig keeps every credential inside
// the config schema, where it is documented in docs/reference/config, set
// by ApplyEnvOverrides, and logged by LoadWithEnv as a path, never a value.
// A raw os.Getenv of a key elsewhere is invisible to all three.
func TestNoRawCredentialEnvReadsOutsideConfig(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	scanned := 0
	for _, top := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
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
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/config/") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for _, m := range credentialEnvRead.FindAllStringSubmatch(string(src), -1) {
				if _, ok := rawCredentialEnvAllowed[rel+":"+m[1]]; !ok {
					offenders = append(offenders, rel+":"+m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned %d Go files under cmd/internal/pkg; the walk did not run over the repo", scanned)
	}
	for _, o := range offenders {
		t.Errorf("raw credential env read %s: declare the field in internal/config (with an `env:` tag and an ApplyEnvOverrides arm) and read it from the loaded Config", o)
	}
}

// TestCredentialEnvVarsDeclaredInSchema pins the schema rows the external
// credentials resolve through.
func TestCredentialEnvVarsDeclaredInSchema(t *testing.T) {
	want := map[string]string{
		"external.coingecko.api_key":      "COINGECKO_API_KEY",
		"external.coingecko.demo_api_key": "COINGECKO_DEMO_API_KEY",
		"external.massive.api_key":        "MASSIVE_API_KEY",
		"external.dune.api_key":           "DUNE_API_KEY",
	}
	got := map[string]string{}
	for _, f := range cfg.Describe() {
		got[f.Path] = f.Env
	}
	for path, env := range want {
		if got[path] != env {
			t.Errorf("schema field %s env = %q, want %q", path, got[path], env)
		}
	}
}
