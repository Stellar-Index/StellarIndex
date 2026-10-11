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

// silenceOnce makes a second install (e.g. a test beside the real main) a no-op that returns the
// already-installed flush func.
var (
	silenceOnce  sync.Once
	silenceFlush func()
)

// SilenceSDKChecksumWarnings filters stderr (fd 2) through a pipe dropping the SDK's no-checksum
// WARNs, which MinIO triggers on every GetObject. Must run before config.LoadDefaultConfig, which
// binds os.Stderr into the SDK logger. The caller MUST run the returned flush (never nil) before
// exiting; os.Exit skips defers.
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
		// The first call panicked before assigning; a no-op is the safe answer.
		return func() {}
	}
	return silenceFlush
}

// installStderrFilter dup-and-replaces fd 2 and starts the filter goroutine. On a syscall error
// callers fail soft; the returned flush tears the filter down and waits for the drain.
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

	// Replace fd 2 with the pipe's writer end so every existing reference to it, including the
	// SDK logger bound to os.Stderr, goes through the filter.
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
		// Registered last so it unwinds first: an unrecovered panic in any goroutine kills the
		// process, and consume's input (the SDK's log lines) is not ours to control.
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
			// fd 2 is the real stderr again, so drop the extra crash-output duplicate to avoid a
			// double-printed traceback.
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
			// Drain the buffered pipe so every byte to forward has reached realStderr on return.
			wg.Wait()
			// fd 2 itself stays open; only our dup'd handle is no longer needed.
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
		// Oversized line: forward the chunks verbatim (the SDK lines targeted are short) until it
		// ends. Never bail: a giant line must not orphan the pipe.
		for errors.Is(err, bufio.ErrBufferFull) {
			_, _ = realStderr.Write(line)
			line, err = br.ReadSlice('\n')
		}
		if len(line) > 0 && !strings.Contains(string(line), checksumWarnSubstring) {
			// ReadSlice keeps the '\n'; only a final unterminated line at EOF lacks one, so
			// terminate it to keep the journal line well-formed.
			_, _ = realStderr.Write(line)
			if line[len(line)-1] != '\n' {
				_, _ = realStderr.Write([]byte{'\n'})
			}
		}
		if err != nil {
			// EOF (flush closed the writer) or a pipe read error: the writer side is done.
			return
		}
	}
}
