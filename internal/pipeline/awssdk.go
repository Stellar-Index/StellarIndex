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

// checksumWarnSubstring marks every aws-sdk-go-v2 "Response has no supported checksum" line to drop; substring match is safe enough.
const checksumWarnSubstring = "Response has no supported checksum"

// silenceOnce guards against accidental double-install (e.g. a test
// calling the function alongside the real main). The second call is
// a no-op and returns the already-installed flush func.
var (
	silenceOnce  sync.Once
	silenceFlush func()
)

// SilenceSDKChecksumWarnings filters stderr (fd 2) through a pipe dropping the SDK's no-checksum WARNs, which MinIO triggers on every GetObject.
// Must run before config.LoadDefaultConfig, which binds os.Stderr into the SDK logger. Fail-soft and sync.Once-guarded.
// The caller MUST run the returned flush (never nil) before exiting; os.Exit skips defers.
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

// installStderrFilterTo is the test seam: consume drains the pipe reader to completion; flush restores real stderr onto fd 2, closes the writer and waits for consume.
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

	// pw is kept so flush() can close it explicitly: that close, after dup2 restores real stderr, is what signals EOF to the reader.

	realStderr := os.NewFile(uintptr(savedFD), "stderr-original")

	// A fatal panic writes its traceback to fd 2 by raw syscall, bypassing the pipe reader; SetCrashOutput points the runtime's crash writer at the real stderr.
	// Fail-soft: on error, crash text still lands in the pipe.
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
			// Restore real stderr on fd 2 BEFORE closing the pipe writer, so writes between the two land on the real fd.
			// dup2 closes the pipe writer's fd-2 reference; the reader sees EOF only once pw is closed too, next.
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

// filteringForwarder drops lines containing checksumWarnSubstring and forwards the rest to realStderr.
// INVARIANT: exit only when the pipe reports EOF/error. A reader giving up early (oversized line) fills the 64 KiB pipe and blocks every stderr write in the process,
// so oversized lines are forwarded in raw chunks.
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
