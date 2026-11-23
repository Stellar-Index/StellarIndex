package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const configCheckValid = `
[region]
id = "r2"

[stellar]
network = "pubnet"

[storage]
postgres_dsn = "postgres://stellarindex@127.0.0.1/stellarindex"
s3_access_key_env = "CONFIG_CHECK_TEST_UNSET_ACCESS_KEY"
`

func writeConfigCheckFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runConfigCheck goes through the dispatch table and exit-code mapping, as `stellarindex-ops config-check` does.
func runConfigCheck(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	run, ok := subcommands["config-check"]
	if !ok {
		t.Fatal("config-check is not in the subcommands table")
	}
	var stdout, stderr bytes.Buffer
	configCheckOut = &stdout
	t.Cleanup(func() { configCheckOut = os.Stdout })
	code := dispatchExitCode("config-check", run(append([]string{"config-check"}, args...)), &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConfigCheck(t *testing.T) {
	t.Run("unknown key is refused by name, never by value", func(t *testing.T) {
		p := writeConfigCheckFile(t, configCheckValid+`
[api.dashboard]
config_check_key_from_a_newer_release = "SEAL_VALUE_MUST_NOT_PRINT"
`)
		code, stdout, stderr := runConfigCheck(t, "-config", p)
		if code == 0 {
			t.Fatalf("exit 0 for a file the daemons refuse to boot on; stdout=%q", stdout)
		}
		if !strings.Contains(stderr, "api.dashboard.config_check_key_from_a_newer_release") {
			t.Errorf("stderr does not name the unknown key: %q", stderr)
		}
		if strings.Contains(stdout+stderr, "SEAL_VALUE_MUST_NOT_PRINT") {
			t.Errorf("output carries the key's value: %q", stdout+stderr)
		}
	})

	t.Run("valid file exits 0", func(t *testing.T) {
		code, stdout, stderr := runConfigCheck(t, "-config", writeConfigCheckFile(t, configCheckValid))
		if code != 0 {
			t.Fatalf("exit %d for a valid file: %s", code, stderr)
		}
		if !strings.Contains(stdout, "ok") {
			t.Errorf("stdout = %q, want an ok line", stdout)
		}
	})

	t.Run("shipped example config exits 0", func(t *testing.T) {
		code, _, stderr := runConfigCheck(t, "-config", filepath.Join("..", "..", "configs", "example.toml"))
		if code != 0 {
			t.Fatalf("exit %d for configs/example.toml: %s", code, stderr)
		}
	})

	// The role validates in its own environment, where the daemons' secrets are absent: a check that read them
	// would refuse a good file there, or accept a file only because of a value the daemon may not see.
	t.Run("absent or invalid env secrets do not change the verdict", func(t *testing.T) {
		t.Setenv("CONFIG_CHECK_TEST_UNSET_ACCESS_KEY", "")
		t.Setenv("STELLARINDEX_POSTGRES_DSN", "not a dsn")
		code, _, stderr := runConfigCheck(t, "-config", writeConfigCheckFile(t, configCheckValid))
		if code != 0 {
			t.Fatalf("exit %d: the check read the environment: %s", code, stderr)
		}
	})

	t.Run("missing -config is a failure", func(t *testing.T) {
		if code, _, _ := runConfigCheck(t); code == 0 {
			t.Fatal("exit 0 with no -config")
		}
	})

	t.Run("unreadable path is a failure", func(t *testing.T) {
		if code, _, _ := runConfigCheck(t, "-config", filepath.Join(t.TempDir(), "absent.toml")); code == 0 {
			t.Fatal("exit 0 for a file that does not exist")
		}
	})
}
