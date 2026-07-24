package timescale

import "testing"

// TestReDeriveResolverGuard_ACRIT1 locks in the fail-closed guard for the
// audit-2026-07-24 critical: a re-derive entry point that stamps a positive
// derive_generation but never calls InstallUSDVolumeResolution would silently
// overwrite correct trades.usd_volume with NULL (projected-rebuild + the main
// on-chain backfill both shipped this). The guard makes that unrepresentable at
// the trade-write choke point. This test asserts the three cases directly on the
// guard (no DB needed — it is a pure field check).
func TestReDeriveResolverGuard_ACRIT1(t *testing.T) {
	// Live ingest (generation 0): never guarded, resolvers optional.
	live := &Store{}
	if err := live.reDeriveResolverGuard(); err != nil {
		t.Fatalf("live ingest (gen 0) must pass unguarded: %v", err)
	}

	// Re-derive mode (gen > 0) WITHOUT InstallUSDVolumeResolution: the exact
	// destructive combination — must fail closed.
	bug := &Store{}
	bug.SetDeriveGeneration(1)
	if err := bug.reDeriveResolverGuard(); err == nil {
		t.Fatal("re-derive mode with no InstallUSDVolumeResolution must be refused (A-CRIT-1)")
	}

	// Re-derive mode WITH Install having run (even the no-pegs no-op path, which
	// wires no resolver): must pass — the guard tracks "Install called", so a
	// legitimate off-chain-only re-derive is not falsely blocked.
	ok := &Store{}
	ok.SetDeriveGeneration(1)
	ok.usdVolumeResolutionInstalled = true // what InstallUSDVolumeResolution sets
	if err := ok.reDeriveResolverGuard(); err != nil {
		t.Fatalf("re-derive mode after Install (even no-pegs) must pass: %v", err)
	}
}
