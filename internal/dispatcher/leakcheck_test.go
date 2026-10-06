package dispatcher

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the suite if a goroutine started by a test outlives it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
