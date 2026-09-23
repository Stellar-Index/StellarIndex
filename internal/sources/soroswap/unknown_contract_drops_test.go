package soroswap

import (
	"math/big"
	"testing"
)

// TestDecoder_UnknownContractDrops_countsSameAsSkippedUnknownPair pins
// GH-1307: a completed swap+sync whose pair has no token mapping is
// dropped with (nil, nil) — indistinguishable from "not a trade" to
// the caller — and must be surfaced through the dispatcher's duck-typed
// reporter interface (mirroring EvictedOrphans) so
// internal/pipeline.emitDispatcherMetricDeltas can wire it to
// obs.SourceDecodeErrorsTotal. Before the fix, Decoder had no
// UnknownContractDrops method at all: this drop had zero non-test
// callers reading it.
func TestDecoder_UnknownContractDrops_countsSameAsSkippedUnknownPair(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x20)

	if _, err := d.Decode(makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200))); err != nil {
		t.Fatalf("Decode swap: %v", err)
	}
	if _, err := d.Decode(makeSyncEvent(t, pair)); err != nil {
		t.Fatalf("Decode sync: %v", err)
	}

	if got := d.UnknownContractDrops(); got != 1 {
		t.Errorf("UnknownContractDrops() = %d, want 1 (one completed swap+sync dropped for want of a pair mapping)", got)
	}
	if got := d.SkippedUnknownPair(); got != 1 {
		t.Errorf("SkippedUnknownPair() = %d, want 1", got)
	}
}

// TestDecoder_EvictedOrphans_excludesBareSyncEvictions pins GH-1308: a
// bare `sync` (deposit/withdraw/skim traffic, README Q2) that ages out
// of the correlation buffer with no preceding swap must NOT inflate
// EvictedOrphans — that counter is the real-loss signal (a swap that
// never got its sync) and must stay legible. Before the fix, both
// classes fed the single `evictedOrphans` counter.
func TestDecoder_EvictedOrphans_excludesBareSyncEvictions(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21)

	// A bare sync — no swap ever arrives for this group key.
	syncOnly := makeSyncEvent(t, pair)
	syncOnly.TxHash = "baresynctx"
	syncOnly.LedgerClosedAt = "2026-04-23T12:00:00Z"
	if _, err := d.Decode(syncOnly); err != nil {
		t.Fatalf("Decode bare sync: %v", err)
	}

	// An event more than defaultOrphanMaxAge (5m) later sweeps the
	// buffer and evicts the bare sync as an orphan-class entry.
	sweeper := makeSyncEvent(t, pair)
	sweeper.TxHash = "sweepertx"
	sweeper.LedgerClosedAt = "2026-04-23T12:10:00Z"
	if _, err := d.Decode(sweeper); err != nil {
		t.Fatalf("Decode sweeper: %v", err)
	}

	if got := d.EvictedBareSync(); got != 1 {
		t.Errorf("EvictedBareSync() = %d, want 1", got)
	}
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans() = %d, want 0 (a bare sync is LP traffic, not a lost trade)", got)
	}
}

// TestDecoder_EvictedOrphans_countsSwapWithoutSync is the counterpart
// to the bare-sync test above: a swap that never gets its sync IS the
// real loss class and must still land in EvictedOrphans.
func TestDecoder_EvictedOrphans_countsSwapWithoutSync(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x22)

	swapOnly := makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200))
	swapOnly.TxHash = "swaponlytx"
	swapOnly.LedgerClosedAt = "2026-04-23T12:00:00Z"
	if _, err := d.Decode(swapOnly); err != nil {
		t.Fatalf("Decode swap-only: %v", err)
	}

	sweeper := makeSyncEvent(t, pair)
	sweeper.TxHash = "sweepertx2"
	sweeper.LedgerClosedAt = "2026-04-23T12:10:00Z"
	if _, err := d.Decode(sweeper); err != nil {
		t.Fatalf("Decode sweeper: %v", err)
	}

	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans() = %d, want 1 (a swap with no sync is a real lost trade)", got)
	}
	if got := d.EvictedBareSync(); got != 0 {
		t.Errorf("EvictedBareSync() = %d, want 0", got)
	}
}
