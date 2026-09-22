package clickhouse

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBuildWasmDisassembly_BestEffort(t *testing.T) {
	r := &ExplorerReader{disasmCache: newWasmDisasmCache()}
	info := ContractWasmInfo{WasmHash: "besteffort-hash"}
	r.buildWasmDisassembly(context.Background(), &info, contractRegisterWasm)

	_, hasWat2 := exec.LookPath("wasm2wat")
	if hasWat2 == nil {
		// Tooling present (dev box / CI with wabt): WAT must render and start
		// with the module header.
		if !strings.HasPrefix(strings.TrimSpace(info.Wat), "(module") {
			t.Errorf("wat does not look like WAT: %.40q", info.Wat)
		}
	} else {
		// Tooling absent (e.g. a stock r1 without `apt install wabt`): the
		// field is empty and the note explains why. The endpoint still ships
		// metadata + exports — this is the graceful-degradation contract.
		if info.Wat != "" {
			t.Errorf("expected empty wat without wasm2wat, got %d bytes", len(info.Wat))
		}
		if !strings.Contains(info.ToolNote, "not on PATH") {
			t.Errorf("expected a 'not on PATH' note, got %q", info.ToolNote)
		}
	}
}

// TestBuildWasmDisassembly_CacheHitSkipsToolRun proves the fork/exec cost is
// paid once per wasm hash, not once per request: a cache entry seeded for a
// hash is served verbatim on the next call for that same hash, WITHOUT
// running wasm2wat/wasm-decompile again. The seeded text ("SEEDED …") could
// never come out of a real tool run, so the assertion is decisive regardless
// of whether wabt is installed on the box running the test.
//
// Pre-fix, buildWasmDisassembly ignored any cache and always ran the tools
// live, so a seeded entry was never read back and this fails.
func TestBuildWasmDisassembly_CacheHitSkipsToolRun(t *testing.T) {
	r := &ExplorerReader{disasmCache: newWasmDisasmCache()}
	const hash = "cachehit-hash"
	r.disasmCache.put(hash, wasmDisasmEntry{
		wat:        "SEEDED WAT OUTPUT",
		decompiled: "SEEDED DECOMPILE OUTPUT",
		toolNote:   "",
	}, time.Now())

	info := ContractWasmInfo{WasmHash: hash}
	r.buildWasmDisassembly(context.Background(), &info, contractRegisterWasm)

	if info.Wat != "SEEDED WAT OUTPUT" {
		t.Errorf("Wat = %q, want the cached entry served verbatim (no live tool run)", info.Wat)
	}
	if info.Decompiled != "SEEDED DECOMPILE OUTPUT" {
		t.Errorf("Decompiled = %q, want the cached entry served verbatim (no live tool run)", info.Decompiled)
	}
}

// TestBuildWasmDisassembly_NilCacheIsPermanentMiss — a zero-value reader
// (as some tests build) must not panic; it behaves as an always-miss cache.
func TestBuildWasmDisassembly_NilCacheIsPermanentMiss(t *testing.T) {
	r := &ExplorerReader{}
	info := ContractWasmInfo{WasmHash: "nilcache-hash"}
	r.buildWasmDisassembly(context.Background(), &info, contractRegisterWasm) // must not panic
}

func TestRunWasmTool_MissingTool(t *testing.T) {
	out, note := runWasmTool(context.Background(), "definitely-not-a-real-tool-xyz", contractRegisterWasm)
	if out != "" {
		t.Errorf("expected empty output for missing tool, got %d bytes", len(out))
	}
	if !strings.Contains(note, "not on PATH") {
		t.Errorf("expected 'not on PATH' note, got %q", note)
	}
}
