package clickhouse

import "testing"

// TestRefreshGate_ClassFairness pins the per-class cap (inventory #26
// item 5, second half): one class saturating its half-of-global cap
// must NOT stop other classes from acquiring, and the global bound
// must still hold across classes.
func TestRefreshGate_ClassFairness(t *testing.T) {
	g := NewRefreshGate(4) // class cap = 2

	if !g.TryAcquireClass("contract_detail") || !g.TryAcquireClass("contract_detail") {
		t.Fatal("class should admit up to its cap (2 of global 4)")
	}
	if g.TryAcquireClass("contract_detail") {
		t.Fatal("third same-class acquire must be refused (class cap) — pre-fix one class could hold every global slot")
	}
	// Other classes still have global headroom.
	if !g.TryAcquireClass("account_state") {
		t.Fatal("a different class must still be admitted while another class is saturated")
	}
	if !g.TryAcquireClass("asset_holders") {
		t.Fatal("global slots 4/4 in use across three classes — this acquire fills the last one")
	}
	// Global bound holds even for a fresh class.
	if g.TryAcquireClass("contracts_dir") {
		t.Fatal("global bound must still cap the total across classes")
	}
	// Release restores both levels.
	g.ReleaseClass("contract_detail")
	if !g.TryAcquireClass("contracts_dir") {
		t.Fatal("released global slot must be claimable by another class")
	}
}
