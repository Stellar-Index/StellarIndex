package pipeline

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak around the whole pipeline test suite (W8.15).
//
// The sink fans out a pool of long-lived worker goroutines (sink.go
// persistWorker / the retry loops) plus the AWS-SDK download goroutine.
// A worker that outlives the Sink it belongs to — a Close() that returns
// before its goroutines drain, a ctx that never propagates cancellation —
// is a goroutine leak: it holds a DB/CH connection and keeps consuming
// its input channel forever. That class is invisible to ordinary assert
// tests (they check the row that WAS written, not the goroutine still
// running afterwards). goleak.VerifyTestMain snapshots the goroutine set
// after every test in the package finishes and fails the suite if any
// non-runtime goroutine is still alive, so a leaked sink worker reds CI
// instead of only manifesting as an fd/connection exhaustion in prod.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
