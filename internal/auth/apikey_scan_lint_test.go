// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNoNewAPIKeyKeyspaceWalk runs scripts/ci/lint-apikey-scan.sh — the
// ban on walking the `apikey:*` keyspace outside the one sanctioned
// index build (class finding K051) — and its fixture self-test.
//
// Why a Go test wraps a shell gate: a script under scripts/ci runs only
// where something names it, and a control that exists but is never
// invoked is its own finding class (K023). `go test ./...` runs in
// every gate this repo has, so the ban holds from the commit that adds
// it, whether or not verify.sh and ci.yml have been taught the script's
// name yet.
//
// It does not skip when bash is missing: a skipped control reports
// green over nothing.
func TestNoNewAPIKeyKeyspaceWalk(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	for _, script := range []string{
		"scripts/ci/lint-apikey-scan.sh",
		"scripts/ci/lint-apikey-scan-test.sh",
	} {
		t.Run(filepath.Base(script), func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join(repoRoot, script))
			cmd.Dir = repoRoot
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed (%v):\n%s", script, err, out)
			}
			t.Logf("%s", out)
		})
	}
}
