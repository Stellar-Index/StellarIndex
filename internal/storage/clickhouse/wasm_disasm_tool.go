package clickhouse

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// wasm_disasm_tool.go — best-effort WAT disassembly + wasm-decompile pseudocode via
// the wabt toolchain (wasm2wat / wasm-decompile). These are OPTIONAL: if the
// binaries aren't on PATH the metadata + native-parsed exports still ship, and
// the ToolNote explains the absence. The contract pages' "see the code" view
// degrades gracefully rather than 503-ing on a missing system dependency.
//
// Deployment: wabt isn't installed on r1 by default but is in apt
// (`apt install wabt`). Until it's installed these fields are empty; once it
// is, they populate with no code change. `Cache-Control: max-age=86400`
// (wasm_view.go) only bounds re-fetches a CACHING CLIENT or a CDN chooses to
// honour — it is not itself a server-side cache, and a deployment with no CDN
// in front of it (CacheControlWithCDN(false), L3.14) pays the header with no
// effect at all. The fork/exec cost that actually recurs per request is
// bounded HERE instead: ExplorerReader.disasmCache (wasm_disasm_cache.go)
// keys a successful run by wasm hash — content-addressed and immutable — so
// wasm2wat/wasm-decompile run at most once per contract per process, not once
// per request.

// wasmToolTimeout bounds each external tool invocation. WAT/decompile of a
// ~50 KB module is sub-second; the timeout only fires on a pathological input.
const wasmToolTimeout = 10 * time.Second

// maxDisasmOutputBytes caps the WAT / decompile text we retain so a large
// module can't blow the JSON response (and the explorer can lazy-load the rest
// via a future raw-WAT endpoint if needed). ~2 MB is generous for human
// inspection; bigger outputs are truncated with a marker.
const maxDisasmOutputBytes = 2 << 20 // 2 MiB

// buildWasmDisassembly fills info.Wat + info.Decompiled best-effort and appends
// to info.ToolNote. It never fails the caller — a missing tool or a tool error
// just leaves the corresponding field empty with an explanatory note.
//
// info.WasmHash (already set by the caller) is a content address, so a
// previously-cached SUCCESSFUL run is served without paying fork/exec again.
// A miss (never cached, or the prior run failed) computes and — only on full
// success — populates r.disasmCache for the next request. Caching a failure
// would pin a transient condition (tool not yet installed, a load-induced
// timeout) as a permanent false negative for that hash, so failures are
// always retried instead.
func (r *ExplorerReader) buildWasmDisassembly(ctx context.Context, info *ContractWasmInfo, code []byte) {
	if cached, ok := r.disasmCache.get(info.WasmHash); ok {
		info.Wat = cached.wat
		info.Decompiled = cached.decompiled
		info.ToolNote += cached.toolNote
		return
	}

	wat, watNote := runWasmTool(ctx, "wasm2wat", code, "--no-check")
	dec, decNote := runWasmTool(ctx, "wasm-decompile", code)

	notes := make([]string, 0, 2)
	if watNote != "" {
		notes = append(notes, "wat: "+watNote)
	}
	if decNote != "" {
		notes = append(notes, "decompile: "+decNote)
	}
	var note string
	if len(notes) > 0 {
		note = strings.Join(notes, "; ")
	}

	info.Wat = wat
	info.Decompiled = dec
	info.ToolNote += note

	if watNote == "" && decNote == "" {
		r.disasmCache.put(info.WasmHash, wasmDisasmEntry{wat: wat, decompiled: dec, toolNote: note}, time.Now())
	}
}

// runWasmTool runs a wabt binary against the wasm module and returns its text
// output. The second return is a human note: empty on success, else why the
// stage is absent.
//
// wabt's wasm2wat / wasm-decompile read the module from a real file path (they
// do NOT read stdin via "-"; this build treats "-" as a literal filename) and
// "-o <path>" writes the result there ("-o -" is silently empty), so we stage
// both the input and the output through temp files. The temp files are removed
// before returning; the cost is two tiny writes per (immutable, day-cached)
// contract, amortised to ~nothing.
func runWasmTool(ctx context.Context, tool string, code []byte, extraArgs ...string) (string, string) {
	if _, err := exec.LookPath(tool); err != nil {
		return "", tool + " not on PATH (install wabt to enable)"
	}

	inPath, outPath, cleanup, err := stageWasmTempFiles(code)
	if err != nil {
		return "", tool + " staging failed: " + err.Error()
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, wasmToolTimeout)
	defer cancel()

	args := append([]string{}, extraArgs...)
	args = append(args, "-o", outPath, inPath)

	// G204: tool is one of two fixed wabt binary names ("wasm2wat" /
	// "wasm-decompile"), never user input; args are temp paths we created.
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // see note above
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", tool + " timed out"
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", tool + " failed: " + truncate(msg, 200)
	}

	// G304: outPath is from os.CreateTemp above — a path we created, not
	// caller-influenced.
	raw, err := os.ReadFile(outPath) //nolint:gosec // see note above
	if err != nil {
		return "", tool + " output read failed: " + err.Error()
	}
	text := string(raw)
	if len(text) > maxDisasmOutputBytes {
		text = text[:maxDisasmOutputBytes] + "\n;; … output truncated …\n"
	}
	return text, ""
}

// stageWasmTempFiles writes code to a temp input file and returns the input +
// (empty) output paths plus a cleanup that removes both. The output path is
// created+closed so the tool can write to it.
func stageWasmTempFiles(code []byte) (inPath, outPath string, cleanup func(), err error) {
	in, err := os.CreateTemp("", "stellarindex-wasm-*.wasm")
	if err != nil {
		return "", "", nil, err
	}
	inPath = in.Name()
	if _, werr := in.Write(code); werr != nil {
		_ = in.Close()
		_ = os.Remove(inPath)
		return "", "", nil, werr
	}
	_ = in.Close()

	out, err := os.CreateTemp("", "stellarindex-wasm-*.out")
	if err != nil {
		_ = os.Remove(inPath)
		return "", "", nil, err
	}
	outPath = out.Name()
	_ = out.Close()

	return inPath, outPath, func() {
		_ = os.Remove(inPath)
		_ = os.Remove(outPath)
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
