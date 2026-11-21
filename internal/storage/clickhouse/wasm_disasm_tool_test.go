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
	// Writes its environment to the -o path, as a wabt tool writes its output.
	script := "#!/bin/sh\nwhile [ \"$1\" != -o ]; do shift; done\nenv > \"$2\"\n"
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
