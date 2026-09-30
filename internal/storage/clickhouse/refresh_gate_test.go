package clickhouse

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestRefreshGate_ClassFairness pins the per-class cap (inventory #26
// item 5, second half): one client-keyed class saturating its
// quarter-of-global cap must NOT stop other classes from acquiring, the
// reserved slot stays out of client-keyed reach, and the global bound
// must still hold across classes.
func TestRefreshGate_ClassFairness(t *testing.T) {
	g := NewRefreshGate(8) // client-keyed class cap = 2; client-keyed classes share 7

	if !g.TryAcquireClass("contract_detail") || !g.TryAcquireClass("contract_detail") {
		t.Fatal("class should admit up to its cap (2 of global 8)")
	}
	if g.TryAcquireClass("contract_detail") {
		t.Fatal("third same-class acquire must be refused (class cap) — pre-fix one class could hold every global slot")
	}
	// Other classes still have global headroom.
	if !g.TryAcquireClass("account_state") {
		t.Fatal("a different class must still be admitted while another class is saturated")
	}
	for _, c := range []string{"asset_holders", "contract_detail_ch", "contract_detail_ix", "contract_detail_act"} {
		if !g.TryAcquireClass(c) {
			t.Fatalf("class %q refused with client-keyed slots still free", c)
		}
	}
	if g.TryAcquireClass("contract_detail_pos") {
		t.Fatal("client-keyed classes must not take the reserved last slot")
	}
	if !g.TryAcquireClass("contracts_dir") {
		t.Fatal("global slots 8/8 in use — this server-keyed acquire fills the reserved one")
	}
	// Global bound holds even for a fresh class.
	if g.TryAcquireClass("network_throughput") {
		t.Fatal("global bound must still cap the total across classes")
	}
	// Release restores both levels.
	g.ReleaseClass("contract_detail")
	if !g.TryAcquireClass("network_throughput") {
		t.Fatal("released global slot must be claimable by another class")
	}
}

// TestRefreshGate_TwoDrivenClassesLeaveRoom: two classes whose keys a
// client can mint, each driven until refused, must not hold the whole
// production pool — a third class still acquires.
func TestRefreshGate_TwoDrivenClassesLeaveRoom(t *testing.T) {
	g := NewRefreshGate(DefaultDetachedRefreshLimit)
	held := 0
	for progress := true; progress; {
		progress = false
		for _, c := range []string{"contract_detail_ev", "contract_detail_ch"} {
			if g.TryAcquireClass(c) {
				held++
				progress = true
			}
		}
	}
	if want := 2 * (DefaultDetachedRefreshLimit / 4); held != want {
		t.Fatalf("two driven classes hold %d of %d slots, want %d",
			held, DefaultDetachedRefreshLimit, want)
	}
	for _, c := range []string{"account_state", "asset_holders", "network_throughput"} {
		if !g.TryAcquireClass(c) {
			t.Fatalf("class %q refused while two driven classes hold %d slots", c, held)
		}
	}
}

// contractPagePanels are the client-keyed classes one cold contract page
// fans out to.
var contractPagePanels = []string{
	"contract_detail_ev", "contract_detail_ch", "contract_detail_ix",
	"contract_detail_act", "contract_detail_pos",
}

// TestRefreshGate_SecondColdPageUsesFreeSlots: with one cold page holding a
// slot in each of its five panel classes, a second cold page's fan-out
// must still land in the free client-keyed slots, not be refused because
// its classes are already active.
func TestRefreshGate_SecondColdPageUsesFreeSlots(t *testing.T) {
	g := NewRefreshGate(DefaultDetachedRefreshLimit)
	for _, c := range contractPagePanels {
		if !g.TryAcquireClass(c) {
			t.Fatalf("first cold page: panel %q refused on an idle gate", c)
		}
	}
	admitted := 0
	for _, c := range contractPagePanels {
		if g.TryAcquireClass(c) {
			admitted++
		}
	}
	// All client-keyed room left: the pool minus the first page and the reserved slot.
	if want := DefaultDetachedRefreshLimit - 1 - len(contractPagePanels); admitted != want {
		t.Fatalf("second cold page admitted %d panels, want %d", admitted, want)
	}
}

// TestRefreshGate_ClientClassesCannotTakeReserve fills the gate with every
// client-keyed class in adversarial order — one class to its cap first,
// then one slot in each of the others — and requires each server-keyed
// prewarm class to still acquire.
func TestRefreshGate_ClientClassesCannotTakeReserve(t *testing.T) {
	for _, prewarm := range []string{"network_throughput", "contracts_dir", "ops_directory"} {
		g := NewRefreshGate(DefaultDetachedRefreshLimit)
		clients := append([]string{"account_state", "asset_holders"}, contractPagePanels...)
		for range DefaultDetachedRefreshLimit {
			g.TryAcquireClass(clients[0])
		}
		for range 2 {
			for _, c := range clients[1:] {
				g.TryAcquireClass(c)
			}
		}
		if !g.TryAcquireClass(prewarm) {
			t.Fatalf("%q refused after client-keyed classes filled the gate", prewarm)
		}
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
