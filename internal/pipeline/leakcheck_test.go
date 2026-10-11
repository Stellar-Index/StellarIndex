package pipeline

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak around the suite: a sink worker outliving its Sink (a Close that returns before draining, a ctx that never cancels)
// leaks a DB/CH connection and consumes its channel forever, which ordinary assert tests cannot see.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
