package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// TestRunHoldsInstanceLock is a source-level tripwire: run() must take the
// aggregator's Postgres instance lock before it starts any goroutine, and
// surface a lost lock in its exit error. Without it nothing stops a second
// aggregator double-writing prices, freezes and alerts.
func TestRunHoldsInstanceLock(t *testing.T) {
	astguard.RunHoldsInstanceLock(t, "main.go", "AggregatorInstanceLockName")
}
