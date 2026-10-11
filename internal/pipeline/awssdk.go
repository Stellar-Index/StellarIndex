package pipeline

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// checksumWarnSubstring is the marker every aws-sdk-go-v2 line we want
// to drop contains. The full line emitted by the SDK reads
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
// a no-op and returns the already-installed flush func.
var (
	silenceOnce  sync.Once
	silenceFlush func()
)

// SilenceSDKChecksumWarnings wraps the process's stderr (fd 2) with a
// filtering pipe that drops lines containing
// "Response has no supported checksum"; all else passes through.
//
// aws-sdk-go-v2 WARNs on every GetObject lacking a checksum header and
// MinIO never sends one. The env-var off switch does not help:
// go-stellar-sdk/support/datastore/s3.go hardcodes
// `ChecksumMode: types.ChecksumModeEnabled`.
//
//   - Must run BEFORE config.LoadDefaultConfig, which binds os.Stderr
//     into the SDK logger; call it first in main().
//   - Fail-soft on pipe/dup2 error; sync.Once-guarded.
//
// The caller MUST run the returned flush (never nil) before exiting or
// short-lived processes lose buffered output. os.Exit skips defers.
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
	if err := dupOnto(int(pw.Fd()), int(os.Stderr.Fd())); err != nil {
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

	// A fatal panic/runtime throw freezes the world and writes its
	// traceback directly to the OS fd 2 via a raw syscall, bypassing
	// the pipe's reader goroutine entirely (which may never be
	// scheduled again before exit). SetCrashOutput duplicates
	// realStderr's fd so the runtime's crash writer lands on the real
	// stderr independent of the filter/forwarder below. Fail-soft:
	// if this errors, crash text still lands in the pipe as before.
	_ = debug.SetCrashOutput(realStderr, debug.CrashOptions{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Registered last so it unwinds FIRST: an unrecovered panic in ANY
		// goroutine kills the whole process, and consume's caller-supplied
		// body (the SDK's own log lines, in production) is not something
		// we control the shape of.
		defer worker.Recover(nil, "pipeline-stderr-filter")
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
			_ = dupOnto(savedFD, int(os.Stderr.Fd()))
			_ = pw.Close()
			// fd 2 is the real stderr again as of the dup2 above, so
			// the runtime's own "standard error" crash output already
			// covers it; drop the extra duplicate to avoid a
			// double-printed traceback after flush.
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
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
// INVARIANT: this loop may exit ONLY when the pipe itself reports
// EOF/error — i.e. flush() closed the writer. A reader that gives up
// early (bufio.Scanner with a 1 MiB cap returns false on a single
// oversized line) leaves the pipe silently without its only reader.
// The 64 KiB pipe then fills and EVERY stderr write in the process
// blocks forever — log flood → whole-binary seizure (an
// aquarius-replay PG error flood did exactly this, freezing ingest
// for ~28 min; even SIGQUIT's traceback couldn't flush). Oversized
// lines are therefore forwarded in raw chunks instead of terminating
// the drain.
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
