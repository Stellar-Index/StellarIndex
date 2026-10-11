package streampublish

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak around the SSE publisher test suite.
//
// A publisher loop that outlives its context, or a subscriber goroutine not reaped on disconnect,
// leaks silently under SSE churn; goleak fails the suite if any is still alive afterwards.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
