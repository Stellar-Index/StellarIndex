package clickhouse

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

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

func gateSaturated(class, bound string) float64 {
	return testutil.ToFloat64(obs.ExplorerRefreshGateSaturatedTotal.WithLabelValues(class, bound))
}

// TestRefreshGate_SaturationIsCounted pins T404: every refusal is counted
// with the bound that tripped, and an admitted acquire is not. Not parallel,
// so the exact deltas on the shared counter are this test's alone.
func TestRefreshGate_SaturationIsCounted(t *testing.T) {
	const a, b = "t404_gate_a", "t404_gate_b"
	g := NewRefreshGate(2) // class cap = 1
	aClass, aGlobal, bGlobal := gateSaturated(a, "class"), gateSaturated(a, "global"), gateSaturated(b, "global")
	unclassed := gateSaturated("unclassed", "global")

	if !g.TryAcquireClass(a) {
		t.Fatal("first acquire of an idle gate refused")
	}
	if got := gateSaturated(a, "class") - aClass; got != 0 {
		t.Fatalf("admitted acquire counted %v saturations, want 0", got)
	}
	if g.TryAcquireClass(a) {
		t.Fatal("second same-class acquire must hit the class cap of 1")
	}
	if got := gateSaturated(a, "class") - aClass; got != 1 {
		t.Fatalf("class-cap refusal counted %v, want 1", got)
	}
	if !g.TryAcquire() {
		t.Fatal("unclassed acquire must take the last global slot")
	}
	if g.TryAcquireClass(b) {
		t.Fatal("global bound must refuse a fresh class once both slots are held")
	}
	if got := gateSaturated(b, "global") - bGlobal; got != 1 {
		t.Fatalf("global-bound refusal counted %v, want 1", got)
	}
	if g.TryAcquire() {
		t.Fatal("unclassed acquire on a full gate must be refused")
	}
	if got := gateSaturated("unclassed", "global") - unclassed; got != 1 {
		t.Fatalf("unclassed refusal counted %v, want 1", got)
	}
	if got := gateSaturated(a, "global") - aGlobal; got != 0 {
		t.Fatalf("class-cap refusal leaked %v into the global bound", got)
	}
}

// TestAccountStateCached_SaturationIsCounted drives the cold-miss path that
// returns ErrRefreshSaturated and requires the skipped refresh to be counted
// under its class; before, the only trace was the wrapped 503 error.
func TestAccountStateCached_SaturationIsCounted(t *testing.T) {
	r := &ExplorerReader{
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(1),
	}
	if !r.refreshGate.TryAcquire() {
		t.Fatal("could not acquire the only gate slot to set up saturation")
	}
	before := gateSaturated("account_state", "global")

	_, _, err := r.AccountStateCached(context.Background(),
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if !errors.Is(err, ErrRefreshSaturated) {
		t.Fatalf("saturated cold miss err = %v, want ErrRefreshSaturated", err)
	}
	if got := gateSaturated("account_state", "global") - before; got != 1 {
		t.Fatalf("saturated account-state refresh counted %v, want 1", got)
	}
}
