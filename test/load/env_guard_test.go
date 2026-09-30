package load

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvRefusesProductionTargets loads scenarios/lib/env.js under node with
// each K6_TARGET and asserts the init-time production guard's verdict.
func TestEnvRefusesProductionTargets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	src, err := os.ReadFile(filepath.Join("scenarios", "lib", "env.js"))
	if err != nil {
		t.Fatalf("read env.js: %v", err)
	}
	// .mjs so every node version parses the ES module without a package.json.
	mod := filepath.Join(t.TempDir(), "env.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		target  string
		refused bool
	}{
		{"https://api.stellarindex.io/v1", true},
		{"https://API.StellarIndex.io/v1", true},
		{"https://Rates.Stellar.ORG/v1", true},
		{"https://api.staging.stellarindex.io/v1", false},
		{"https://k6-compile-check.invalid/v1", false},
	}
	for _, tc := range cases {
		script := `globalThis.__ENV = {K6_TARGET: process.env.T, STELLARINDEX_LOAD_API_KEY: 'k'};
try { await import(process.env.M); console.log('ACCEPTED'); }
catch (e) { console.log(e.message); }`
		cmd := exec.Command(node, "--input-type=module", "-e", script)
		cmd.Env = append(os.Environ(), "T="+tc.target, "M=file://"+filepath.ToSlash(mod))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: node: %v\n%s", tc.target, err, out)
		}
		got := strings.Contains(string(out), "Refusing to load-test production target")
		if got != tc.refused {
			t.Errorf("K6_TARGET=%s: refused=%v, want %v (output: %s)", tc.target, got, tc.refused, out)
		}
		if !tc.refused && !strings.Contains(string(out), "ACCEPTED") {
			t.Errorf("K6_TARGET=%s: module did not load: %s", tc.target, out)
		}
	}
}
