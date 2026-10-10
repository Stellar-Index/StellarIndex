package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// TestRunHoldsInstanceLock is a source-level tripwire: run() must take the
// indexer's Postgres instance lock before it starts any goroutine, and
// surface a lost lock in its exit error. Without it nothing stops a second
// indexer racing the cursors and double-writing every sink.
func TestRunHoldsInstanceLock(t *testing.T) {
	astguard.RunHoldsInstanceLock(t, "main.go", "IndexerInstanceLockName")
}
