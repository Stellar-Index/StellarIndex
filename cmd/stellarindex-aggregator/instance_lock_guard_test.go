package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// TestRunHoldsInstanceLock is a source-level tripwire: run() must take the Postgres instance lock before starting any
// goroutine and surface a lost lock in its exit error, or a second aggregator double-writes prices, freezes and
// alerts.
func TestRunHoldsInstanceLock(t *testing.T) {
	astguard.RunHoldsInstanceLock(t, "main.go", "AggregatorInstanceLockName")
}
