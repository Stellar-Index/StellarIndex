package clickhouse

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunWasmTool_ChildGetsNoParentSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	// Writes its environment to stdout, where runWasmTool reads a wabt tool's output.
	script := "#!/bin/sh\nenv\n"
	if err := os.WriteFile(filepath.Join(bin, "fake-wabt"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "postgres://fixture-only")

	out, note := runWasmTool(context.Background(), "fake-wabt", []byte{0, 'a', 's', 'm'})
	if note != "" {
		t.Fatalf("tool run failed: %s", note)
	}
	if strings.Contains(out, "STELLARINDEX_POSTGRES_DSN") {
		t.Fatalf("wabt child inherited the API's environment: %q", out)
	}
	if !strings.Contains(out, "PATH="+bin) {
		t.Fatalf("wabt child lost PATH: %q", out)
	}
}

// A small module can make wasm2wat emit gigabytes: the tool must be stopped once the cap is
// reached, not run to completion and read back whole.
func TestRunWasmTool_StopsToolAtOutputCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	done := filepath.Join(bin, "finished")
	script := "#!/bin/sh\nout=/dev/stdout\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=\"$2\"; fi; shift; done\n" +
		"head -c 67108864 /dev/zero > \"$out\"\ntouch '" + done + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "fake-wabt"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, note := runWasmTool(context.Background(), "fake-wabt", []byte{0, 'a', 's', 'm'})
	if note != "" {
		t.Fatalf("capped run reported a failure: %s", note)
	}
	if !strings.HasPrefix(out[maxDisasmOutputBytes:], "\n;; … output truncated") {
		t.Fatalf("output not cut at %d bytes: len=%d", maxDisasmOutputBytes, len(out))
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("tool ran to completion: the output cap was applied after the whole output was produced")
	}
}

func TestRunWasmTool_RefusesOversizedModule(t *testing.T) {
	out, note := runWasmTool(context.Background(), "sh", make([]byte, maxDisasmInputBytes+1))
	if out != "" || !strings.Contains(note, "too large") {
		t.Fatalf("oversized module was disassembled: out=%d bytes note=%q", len(out), note)
	}
}
