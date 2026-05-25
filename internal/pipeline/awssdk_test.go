package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestFilteringForwarder exercises the line-filter directly, without
// touching real fd 2. The forwarder reads from any io.Reader and
// writes filtered lines to its supplied *os.File; an os.Pipe lets
// us assert on what comes out.
func TestFilteringForwarder(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantOut string
	}{
		{
			name:    "drops the SDK checksum WARN",
			input:   "SDK 2026/05/24 14:39:14 WARN Response has no supported checksum. Not validating response payload.\n",
			wantOut: "",
		},
		{
			name:    "passes through unrelated SDK lines",
			input:   "SDK 2026/05/24 14:39:14 INFO Loaded config from /etc/foo\n",
			wantOut: "SDK 2026/05/24 14:39:14 INFO Loaded config from /etc/foo\n",
		},
		{
			name:    "mixed stream: drops only matching lines",
			input:   "line one\nSDK WARN Response has no supported checksum. trailing\nline three\n",
			wantOut: "line one\nline three\n",
		},
		{
			name:    "no trailing newline still flushes the line",
			input:   "single line no newline",
			wantOut: "single line no newline\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pipe acts as the "real stderr"; we read from the
			// reader side after the forwarder finishes.
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				filteringForwarder(strings.NewReader(tc.input), pw)
				_ = pw.Close()
			}()

			var out bytes.Buffer
			if _, err := io.Copy(&out, pr); err != nil {
				t.Fatalf("copy: %v", err)
			}
			wg.Wait()

			if got := out.String(); got != tc.wantOut {
				t.Fatalf("filtered output mismatch\n got: %q\nwant: %q", got, tc.wantOut)
			}
		})
	}
}

// TestInstallStderrFilterTo exercises the full install path:
// dup-replace fd 2, then verify a write to fd 2 is routed through
// the supplied consumer. Uses the returned flush to tear the
// filter down deterministically instead of the old dup2-restore
// trick.
func TestInstallStderrFilterTo(t *testing.T) {
	// Save the real fd 2 so we can restore it after the test —
	// otherwise subsequent tests' t.Log/t.Error output goes to
	// the pipe and vanishes if flush misbehaves.
	originalStderrCopy, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Fatalf("dup original stderr: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Dup2(originalStderrCopy, int(os.Stderr.Fd()))
		_ = syscall.Close(originalStderrCopy)
	})

	// Capture the routed bytes via a channel; the consume callback
	// reads the entire pipe and sends the result down the channel.
	got := make(chan []byte, 1)
	consume := func(r io.Reader, _ *os.File) {
		buf, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("readall: %v", err)
		}
		got <- buf
	}

	flush, err := installStderrFilterTo(consume)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Write something to fd 2 — the SDK's default logger
	// ultimately writes to fd 2 via os.Stderr, which after dup2
	// is the pipe's write end.
	want := "hello from fd 2\n"
	if _, err := syscall.Write(int(os.Stderr.Fd()), []byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// flush() restores fd 2, closes the pipe writer, and waits
	// for the consumer to drain — so by the time it returns the
	// channel is guaranteed to have received the buffer.
	flush()

	buf := <-got
	if string(buf) != want {
		t.Fatalf("routed bytes mismatch\n got: %q\nwant: %q", string(buf), want)
	}
}

// TestSilenceSDKChecksumWarnings_FlushDrainsPipe is the regression
// test for the rc.77 short-lived-process bug: without flush the
// consumer goroutine is killed mid-buffer when the runtime tears
// down, so multi-line output (and even single-line output once the
// pipe-buffer ceiling is hit) gets lost on os.Exit. After flush
// returns, every byte written to fd 2 since install MUST have
// reached the real-stderr-side.
//
// The test uses installStderrFilterTo with a custom consume
// callback that writes to a captured bytes.Buffer (via a sync
// guard since the goroutine writes concurrently with the test
// goroutine until flush returns). Asserts that all 5 lines arrived
// with their original ordering preserved.
func TestSilenceSDKChecksumWarnings_FlushDrainsPipe(t *testing.T) {
	originalStderrCopy, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Fatalf("dup original stderr: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Dup2(originalStderrCopy, int(os.Stderr.Fd()))
		_ = syscall.Close(originalStderrCopy)
	})

	var (
		mu       sync.Mutex
		captured bytes.Buffer
	)
	consume := func(r io.Reader, _ *os.File) {
		// Forward via filteringForwarder semantics so the test
		// exercises the same code path the production wrap uses,
		// but redirect to the in-memory buffer instead of real
		// stderr. We don't need filtering in this test — just
		// forward every byte verbatim so we can compare exactly.
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				captured.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}

	flush, err := installStderrFilterTo(consume)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Write 5 distinct lines via fmt.Fprintf(os.Stderr, ...). The
	// SDK's default logger writes to os.Stderr too, so this
	// matches the production write path.
	want := ""
	for i := 1; i <= 5; i++ {
		line := fmt.Sprintf("line %d of 5: payload bytes for the drain-on-exit regression test\n", i)
		want += line
		if _, err := fmt.Fprint(os.Stderr, line); err != nil {
			t.Fatalf("fprintf line %d: %v", i, err)
		}
	}

	// The critical assertion: after flush returns, every byte
	// must have arrived. Pre-fix this race-checked: the goroutine
	// was killed by the runtime mid-Read and the captured buffer
	// held only the first ~64 bytes (or nothing at all). Post-fix
	// the wg.Wait inside flush guarantees the goroutine ran the
	// full forwarder loop.
	flush()

	mu.Lock()
	got := captured.String()
	mu.Unlock()

	if got != want {
		t.Fatalf("flush did not drain pipe\n got: %q\nwant: %q", got, want)
	}
}
