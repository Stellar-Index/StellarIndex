package middleware_test

import (
	"net/http"
)

// flushSpy is the test fixture: a ResponseWriter that also implements
// http.Flusher and records each Flush call. We hand it to the
// Logger middleware via httptest.NewRecorder() composition so we can
// observe whether the wrapper propagates Flush down to the underlying
// writer.
type flushSpy struct {
	http.ResponseWriter
	flushes int
}

func (f *flushSpy) Flush() { f.flushes++ }

// discardWriter is a minimal io.Writer for slog. Avoids polluting
// test output with the structured log line.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
