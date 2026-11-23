package clickhouse

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// wasm_disasm_tool.go — best-effort WAT disassembly + wasm-decompile pseudocode via the wabt
// toolchain. OPTIONAL: if the binaries aren't on PATH the metadata + native-parsed exports still
// ship, and the ToolNote explains the absence rather than 503-ing. Cache-Control (wasm_view.go)
// is not a server-side cache, so fork/exec cost is bounded by ExplorerReader.disasmCache, keyed
// by the immutable wasm hash.

// wasmToolTimeout bounds each external tool invocation. WAT/decompile of a
// ~50 KB module is sub-second; the timeout only fires on a pathological input.
const wasmToolTimeout = 10 * time.Second

// maxDisasmOutputBytes caps the WAT / decompile text retained so a large module can't blow the JSON response; bigger outputs are
// truncated with a marker.
const maxDisasmOutputBytes = 2 << 20 // 2 MiB

// maxDisasmInputBytes is well above the network's contract size limit (128 KiB), so only a
// module no ledger could hold is refused.
const maxDisasmInputBytes = 1 << 20

// maxDisasmStderrBytes bounds the tool's error text kept for the note.
const maxDisasmStderrBytes = 4 << 10

// buildWasmDisassembly fills info.Wat + info.Decompiled best-effort and appends to info.ToolNote; it never fails the caller.
// info.WasmHash is a content address, so a cached SUCCESSFUL run is served without fork/exec. Only full success populates r.disasmCache:
// caching a failure would pin a transient condition (tool not yet installed, a load-induced timeout) as a permanent false negative for
// that hash.
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

// runWasmTool runs a wabt binary against the wasm module and returns its text output plus a human note (empty on success). wabt reads the
// module from a real file path (this build treats "-" as a literal filename), so the input is staged through a temp file. Output is read
// from stdout through a bounded reader and the tool is killed at the cap: a small module can expand to gigabytes of WAT.
func runWasmTool(ctx context.Context, tool string, code []byte, extraArgs ...string) (string, string) {
	if len(code) > maxDisasmInputBytes {
		return "", tool + " skipped: module too large"
	}
	if _, err := exec.LookPath(tool); err != nil {
		return "", tool + " not on PATH (install wabt to enable)"
	}

	inPath, cleanup, err := stageWasmInput(code)
	if err != nil {
		return "", tool + " staging failed: " + err.Error()
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, wasmToolTimeout)
	defer cancel()

	args := append(append([]string{}, extraArgs...), inPath)

	// G204: tool is one of two fixed wabt binary names ("wasm2wat" /
	// "wasm-decompile"), never user input; args are temp paths we created.
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // see note above
	// The module is attacker-uploaded: the tool gets PATH only, never the API's secrets.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Dir = os.TempDir()
	// Bounds Wait after a kill: a descendant still holding stdout/stderr must not hang the request.
	cmd.WaitDelay = time.Second
	stderr := &cappedBuffer{limit: maxDisasmStderrBytes}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", tool + " failed: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return "", tool + " failed: " + truncate(err.Error(), 200)
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxDisasmOutputBytes+1))
	truncated := len(raw) > maxDisasmOutputBytes
	if truncated {
		cancel()
	}
	waitErr := cmd.Wait()
	if truncated {
		return string(raw[:maxDisasmOutputBytes]) + "\n;; … output truncated …\n", ""
	}
	if waitErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", tool + " timed out"
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return "", tool + " failed: " + truncate(msg, 200)
	}
	if readErr != nil {
		return "", tool + " output read failed: " + readErr.Error()
	}
	return string(raw), ""
}

// cappedBuffer keeps the first limit bytes written and discards the rest without failing the writer.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// stageWasmInput writes code to a temp file and returns its path plus a cleanup that removes it.
func stageWasmInput(code []byte) (string, func(), error) {
	in, err := os.CreateTemp("", "stellarindex-wasm-*.wasm")
	if err != nil {
		return "", nil, err
	}
	inPath := in.Name()
	if _, werr := in.Write(code); werr != nil {
		_ = in.Close()
		_ = os.Remove(inPath)
		return "", nil, werr
	}
	if cerr := in.Close(); cerr != nil {
		_ = os.Remove(inPath)
		return "", nil, cerr
	}
	return inPath, func() { _ = os.Remove(inPath) }, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
