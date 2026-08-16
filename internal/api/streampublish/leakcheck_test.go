package streampublish

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak around the SSE publisher test suite (W8.15).
//
// The publisher runs a long-lived fan-out goroutine per stream plus the
// per-subscriber pump goroutines. A subscriber whose goroutine is not
// reaped on disconnect, or a publisher loop that outlives its context,
// is a goroutine leak that silently accumulates under real SSE churn.
// goleak.VerifyTestMain snapshots the goroutine set after the package's
// tests finish and fails the suite if any publisher/subscriber goroutine
// is still alive, turning a leak into a red test instead of a slow
// memory climb in the serving binary.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
