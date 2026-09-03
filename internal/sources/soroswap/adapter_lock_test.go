// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package soroswap

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestDecode_PanicInTheCriticalSectionReleasesTheLock is the RV2 H1
// regression.
//
// The dispatcher does not crash on a decoder panic any more: it
// RECOVERS, counts it, skips that one input and carries on
// (internal/dispatcher/panic_guard.go, #371 F1). That contract turns a
// mutex held across the panicking statement into something worse than
// the original crash — the lock is never released, the NEXT event's
// Matches blocks on RLock, and the dispatch goroutine deadlocks for the
// life of the process with /metrics still answering, the unit still
// `active` and the cursor frozen. systemd sees nothing to restart.
//
// Decode used to hold d.mu across `d.buf.absorb(...)` with a bare
// Unlock on the line below, which is exactly that shape. The panic
// source here (a nil correlation buffer — the one pointer dereferenced
// inside the critical section) is a stand-in: the property under test
// is that ANY panic raised while the decoder lock is held still leaves
// the lock released, because "absorb cannot panic today" is a claim
// about today's body, not an invariant anyone will re-check.
func TestDecode_PanicInTheCriticalSectionReleasesTheLock(t *testing.T) {
	pair := makeContractStrkey(t, 0x20)
	tok0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	tok1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pair: {Token0: tok0, Token1: tok1},
	}))
	// The dereference inside the critical section. Set AFTER
	// construction so everything else about this decoder is production
	// shaped.
	d.buf = nil

	swap := makeSwapEvent(t, pair, big.NewInt(1_000), big.NewInt(2_000))

	panicked := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				panicked = true // stand in for dispatchOne's recover
			}
		}()
		_, _ = d.Decode(swap)
	}()
	if !panicked {
		t.Fatal("setup: Decode did not panic inside the critical section; " +
			"this test can only prove the release path if the panic actually fires")
	}

	// The dispatcher moves straight on to the next event, whose Matches
	// takes the READ lock. With the lock still held this never returns.
	done := make(chan bool, 1)
	go func() { done <- d.Matches(swap) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Matches blocked after a recovered panic: the decoder mutex was never released, " +
			"so the dispatch goroutine is deadlocked — ingest wedges silently (RV2 H1)")
	}

	// SeedPair takes the WRITE lock; a leaked read lock would block it
	// too. Covers the second converted critical section.
	seeded := make(chan struct{})
	go func() {
		d.SeedPair(pair, tok0, tok1)
		close(seeded)
	}()
	select {
	case <-seeded:
	case <-time.After(5 * time.Second):
		t.Fatal("SeedPair blocked after a recovered panic: the decoder write lock was never released")
	}
}
