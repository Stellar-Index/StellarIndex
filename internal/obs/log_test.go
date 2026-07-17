package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns everything
// fn wrote to stderr. NewLogger binds its handler to os.Stderr at construction
// time, so fn must BOTH build the logger AND emit through it inside the swap.
//
// It mutates the process-global os.Stderr, so callers MUST NOT call
// t.Parallel(): non-parallel tests in a package run sequentially (never
// concurrently), which keeps the swap isolated.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	// Drain in a goroutine so a large write can't deadlock on the pipe buffer.
	out := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		out <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stderr = orig
	s := <-out
	_ = r.Close()
	return s
}

// TestNewLogger_StampsBinaryAttr — a non-empty binary name lands as the
// `binary` attribute on every emitted record (Loki filters per-binary on it);
// an empty binary name skips the attribute entirely rather than stamping "".
func TestNewLogger_StampsBinaryAttr(t *testing.T) {
	// Non-parallel: captureStderr mutates the process-global os.Stderr.
	line := captureStderr(t, func() {
		obs.NewLogger(config.ObsConfig{LogFormat: "json"}, "stellarindex-test").
			Info("probe-with-binary")
	})
	rec := decodeLogLine(t, line)
	if rec["binary"] != "stellarindex-test" {
		t.Errorf(`binary attr = %v, want "stellarindex-test"`, rec["binary"])
	}
	if rec["msg"] != "probe-with-binary" {
		t.Errorf(`msg = %v, want "probe-with-binary"`, rec["msg"])
	}

	bare := captureStderr(t, func() {
		obs.NewLogger(config.ObsConfig{LogFormat: "json"}, "").Info("probe-no-binary")
	})
	rec2 := decodeLogLine(t, bare)
	if v, present := rec2["binary"]; present {
		t.Errorf("empty binary name must not stamp a binary attr; got %v", v)
	}
}

// TestNewLogger_BinaryAttrNonEmpty — each real binary name reaches the wire as
// the `binary` attribute value verbatim.
func TestNewLogger_BinaryAttrNonEmpty(t *testing.T) {
	// Non-parallel: captureStderr mutates the process-global os.Stderr.
	for _, name := range []string{"stellarindex-indexer", "stellarindex-aggregator", "stellarindex-api"} {
		line := captureStderr(t, func() {
			obs.NewLogger(config.ObsConfig{LogFormat: "json"}, name).Info("probe")
		})
		rec := decodeLogLine(t, line)
		if rec["binary"] != name {
			t.Errorf("binary attr = %v, want %q", rec["binary"], name)
		}
	}
}

// TestNewLogger_LogLevelCaseInsensitive — operators sometimes write
// "DEBUG" / "warning" / "Error"; all should land on the matching slog.Level,
// and anything unrecognised (incl. "") falls back to info. Asserted by
// probing the configured handler's threshold rather than by re-reading the
// switch: the handler must be Enabled AT the wanted level and NOT below it.
func TestNewLogger_LogLevelCaseInsensitive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"Warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
	}
	for _, tc := range cases {
		name := tc.in
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := obs.NewLogger(config.ObsConfig{LogLevel: tc.in, LogFormat: "json"}, "test").Handler()
			if !h.Enabled(ctx, tc.want) {
				t.Errorf("level %q: handler should be enabled at %v", tc.in, tc.want)
			}
			if h.Enabled(ctx, tc.want-1) {
				t.Errorf("level %q: handler should NOT be enabled below %v", tc.in, tc.want)
			}
		})
	}
}

// TestNewLogger_LogFormatCaseInsensitive — "console"/"text" (any case) select
// the text handler; anything else (incl. "" and unknown) defaults to JSON.
// Asserted on the concrete handler type behind the logger.
func TestNewLogger_LogFormatCaseInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantText bool
	}{
		{"console", true},
		{"CONSOLE", true},
		{"text", true},
		{"Text", true},
		{"json", false},
		{"JSON", false},
		{"", false},
		{"unknown-format", false},
	}
	for _, tc := range cases {
		name := tc.in
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := obs.NewLogger(config.ObsConfig{LogFormat: tc.in}, "test").Handler()
			switch h.(type) {
			case *slog.TextHandler:
				if !tc.wantText {
					t.Errorf("format %q: got *slog.TextHandler, want *slog.JSONHandler", tc.in)
				}
			case *slog.JSONHandler:
				if tc.wantText {
					t.Errorf("format %q: got *slog.JSONHandler, want *slog.TextHandler", tc.in)
				}
			default:
				t.Errorf("format %q: unexpected handler type %T", tc.in, h)
			}
		})
	}
}

// decodeLogLine parses a single JSON log record, failing the test if the
// captured output isn't exactly one valid JSON object.
func decodeLogLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %q", err, line)
	}
	return rec
}
