package pipeline

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
)

// checksumWarnSubstring is the marker every aws-sdk-go-v2 line we want
// to drop contains. The full line shipped by the SDK reads
//
//	SDK 2026/05/24 14:39:14 WARN Response has no supported checksum. Not validating response payload.
//
// — substring match is good enough; the SDK never logs the same
// phrase for anything else, and we accept the (vanishingly small)
// risk of dropping a future unrelated line that happens to contain
// it.
const checksumWarnSubstring = "Response has no supported checksum"

// silenceOnce guards against accidental double-install (e.g. a test
// calling the function alongside the real main). The second call is
// a no-op and returns the previously-installed flush func.
var (
	silenceOnce  sync.Once
	silenceFlush func()
)

// SilenceSDKChecksumWarnings wraps the process's stderr (fd 2) with a
// filtering pipe that drops lines containing
// "Response has no supported checksum" before they reach the real
// stderr. Everything else is forwarded byte-for-byte.
//
// Why this exists: aws-sdk-go-v2 logs a WARN on every S3 GetObject
// response that lacks one of its supported checksum headers. MinIO
// (our colo galexie backend) never sends those headers, so *every*
// ledger read triggers the line. stellarindex-indexer's live tail
// reads ~1 ledger/5s so the noise is trivial; verify-archive's
// 12-way parallel walk does ~50k ledgers/s and floods journald
// (~22k WARN/30s observed during r1 bootstrap, ballooning
// /tmp/va-full.log to 1.65 GB and burying the real verify-archive
// failure under noise journald then rate-dropped).
//
// The previous attempt (rc.72: QuietS3ChecksumWarnings) set
// AWS_RESPONSE_CHECKSUM_VALIDATION=when_required so the SDK's
// default-config layer would skip the validation attempt entirely.
// That fix is a no-op for our use because
// go-stellar-sdk/support/datastore/s3.go:161 hardcodes
//
//	ChecksumMode: types.ChecksumModeEnabled
//
// on every GetObjectInput, overriding whatever the env-var default
// produced. The upstream-respect path is to fix that line in
// go-stellar-sdk; until that lands, stderr filtering is the
// reliable workaround.
//
// Mechanism:
//
//  1. dup the current fd 2 to a fresh fd (the "real stderr").
//  2. create a pipe; dup2 the write end onto fd 2 so every
//     subsequent write — including the SDK's default logger which
//     was bound to os.Stderr at config-load time — flows into our
//     reader.
//  3. spin a goroutine that scans the reader line-by-line, drops
//     lines containing checksumWarnSubstring, and forwards the
//     rest to the real stderr.
//
// Constraints honoured:
//
//   - Must run BEFORE config.LoadDefaultConfig — the SDK captures
//     os.Stderr into logging.NewStandardLogger at that point. Call
//     this from the first line of main().
//   - Fail-soft: any error in pipe/dup2 logs to the original
//     stderr and returns; the binary keeps running with noisy
//     stderr, never crashes at startup over a logging filter.
//   - sync.Once-guarded; second call is a no-op (returns the
//     same flush from the first install).
//   - The goroutine drains the pipe continuously, so a slow real
//     stderr (e.g. journald rate-limit) can't deadlock the
//     writer side beyond the pipe buffer.
//
// # Drain-on-exit (rc.78)
//
// Returns a `flush func()` the caller MUST run before the process
// exits. Without it, short-lived processes lose output: the
// consumer goroutine reads from the pipe in the background and is
// killed mid-buffer when the runtime tears down. This first
// manifest in rc.77 as `stellarindex-ops backfill -dry-run`
// printing only its first line and `stellarindex-ops backfill`
// errors printing nothing at all.
//
// The flush func:
//
//  1. dup2's the saved real-stderr fd back onto fd 2, so any
//     subsequent writes bypass the pipe.
//  2. closes the pipe writer, signalling EOF to the reader.
//  3. waits on the consumer goroutine to finish draining
//     (sync.WaitGroup) before returning.
//
// Crucial design constraint: Go's `os.Exit` does NOT run deferred
// functions. So `defer flush()` only fires when main() returns
// normally. The canonical caller shape is therefore:
//
//	func main() { os.Exit(realMain()) }
//	func realMain() int {
//	    flush := pipeline.SilenceSDKChecksumWarnings()
//	    defer flush()  // runs because realMain returns normally
//	    // ...
//	    return 0  // or 1 on error
//	}
//
// This way every error path (return 1 from realMain) still
// triggers the defer before main calls os.Exit with the int.
//
// Fail-soft install still returns a non-nil flush — it's a no-op
// when no pipe was installed, so callers can defer
// unconditionally.
func SilenceSDKChecksumWarnings() (flush func()) {
	silenceOnce.Do(func() {
		f, err := installStderrFilter()
		if err != nil {
			fmt.Fprintf(os.Stderr, "SilenceSDKChecksumWarnings: install failed, continuing with raw stderr: %v\n", err)
			silenceFlush = func() {}
			return
		}
		silenceFlush = f
	})
	if silenceFlush == nil {
		// Second-or-later call before the first finished installing
		// (unreachable in practice; sync.Once orders them) or the
		// first call panicked before assigning. Either way, a no-op
		// is the safe answer.
		return func() {}
	}
	return silenceFlush
}

// installStderrFilter is the testable core. It dup-and-replaces fd
// 2, then launches the filter goroutine. Returns an error if any
// syscall fails; callers should fail-soft. The returned flush
// func tears the filter down and waits for the goroutine to drain.
func installStderrFilter() (func(), error) {
	return installStderrFilterTo(filteringForwarder)
}

// installStderrFilterTo is the test seam: the consumer function
// receives the pipe reader + the real-stderr writer and is
// responsible for draining the reader to completion. The returned
// flush func dup2's the saved real-stderr back onto fd 2, closes
// the pipe writer, and waits on the consumer goroutine (via the
// internal WaitGroup) to return.
func installStderrFilterTo(consume func(r io.Reader, realStderr *os.File)) (func(), error) {
	// Duplicate fd 2 to a fresh fd so we can keep writing to the
	// real stderr after we overwrite fd 2 with the pipe.
	savedFD, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		return nil, fmt.Errorf("dup fd 2: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(savedFD)
		return nil, fmt.Errorf("pipe: %w", err)
	}

	// Replace fd 2 with the pipe's writer end. Every existing
	// reference to fd 2 (including the aws-sdk-go-v2 default
	// logger bound to os.Stderr) is redirected through us from
	// here on.
	if err := syscall.Dup2(int(pw.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		_ = syscall.Close(savedFD)
		return nil, fmt.Errorf("dup2 onto fd 2: %w", err)
	}

	// pw still holds a duplicate FD pointing at the pipe's writer
	// end; we keep it around so flush() can close it deterministically.
	// Closing it before flush would mean fd 2 (the dup'd target) is
	// the sole writer reference — that's fine for ongoing writes,
	// but flush() needs an explicit close to signal EOF to the
	// reader after dup2'ing the real stderr back onto fd 2.

	realStderr := os.NewFile(uintptr(savedFD), "stderr-original")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		consume(pr, realStderr)
	}()

	var flushOnce sync.Once
	flush := func() {
		flushOnce.Do(func() {
			// Restore real stderr on fd 2 BEFORE closing the pipe
			// writer. Order matters: any goroutine that wakes up
			// between these two calls and writes to os.Stderr
			// should land on the real fd, not a half-closed pipe.
			// dup2 closes the existing fd-2 target as part of its
			// atomic replacement — which is the pipe writer we
			// installed in installStderrFilterTo. That close is
			// what signals EOF to the reader, *provided* pw (the
			// remaining handle) is also closed; do that next.
			_ = syscall.Dup2(savedFD, int(os.Stderr.Fd()))
			_ = pw.Close()
			// Wait for the consumer goroutine to drain whatever
			// was buffered in the pipe before we returned to the
			// caller. After this point, every byte the goroutine
			// was going to forward has reached realStderr.
			wg.Wait()
			// Close our handle on the dup'd real-stderr now that
			// no one needs it (fd 2 itself remains open as a
			// fresh dup2 target).
			_ = realStderr.Close()
		})
	}
	return flush, nil
}

// filteringForwarder reads `r` line-by-line, drops every line that
// contains checksumWarnSubstring, and forwards the rest verbatim to
// `realStderr`.
//
// INVARIANT (2026-07-10 indexer-seizure incident): this loop may exit
// ONLY when the pipe itself reports EOF/error — i.e. flush() closed
// the writer. The previous implementation used bufio.Scanner with a
// 1 MiB cap; a single oversized line made Scan() return false, the
// goroutine returned, and the pipe silently lost its only reader.
// From then on the 64 KiB pipe filled and EVERY stderr write in the
// process blocked forever — log flood → whole-binary seizure (the
// aquarius-replay PG error flood triggered exactly this, freezing
// ingest for ~28 min; even SIGQUIT's traceback couldn't flush).
// Oversized lines are now forwarded in raw chunks instead of
// terminating the drain.
func filteringForwarder(r io.Reader, realStderr *os.File) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadSlice('\n')
		// Oversized line: ReadSlice fills the buffer without finding
		// '\n'. Forward the chunks verbatim (no filtering — the SDK
		// lines this filter targets are short) until the line ends.
		// Never bail: a giant line must not orphan the pipe.
		for errors.Is(err, bufio.ErrBufferFull) {
			_, _ = realStderr.Write(line)
			line, err = br.ReadSlice('\n')
		}
		if len(line) > 0 && !strings.Contains(string(line), checksumWarnSubstring) {
			// ReadSlice keeps the trailing '\n' (absent only on a
			// final unterminated line at EOF — terminate it so the
			// journal line stays well-formed, matching the previous
			// Scanner behavior).
			_, _ = realStderr.Write(line)
			if line[len(line)-1] != '\n' {
				_, _ = realStderr.Write([]byte{'\n'})
			}
		}
		if err != nil {
			// EOF (flush closed the writer) or a genuine pipe read
			// error — either way the writer side is done with us.
			return
		}
	}
}
