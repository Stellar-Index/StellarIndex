// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package soroswap

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestDecode_PanicInTheCriticalSectionReleasesTheLock pins that any panic under the decoder lock releases it. The
// dispatcher recovers and skips the input, so a lock held across the panic would block the next Matches on RLock and
// freeze the cursor while /metrics still answers. The nil correlation buffer is a stand-in panic source.
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
