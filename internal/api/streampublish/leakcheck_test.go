package streampublish

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak around the SSE publisher suite, since a publisher loop outliving its context or an unreaped subscriber
// goroutine leaks silently.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
