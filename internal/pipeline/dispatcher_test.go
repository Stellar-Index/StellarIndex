package pipeline

import (
	"testing"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/dispatcher"
)

// realisticGStrkey is a syntactically-valid 56-char G-prefixed
// strkey reused as a fixture below. The accounts.NewObserver
// constructor only checks for non-empty strings; stricter strkey
// validation happens upstream at config.Validate.
const realisticGStrkey = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty pins the
// safe default: an operator who hasn't opted into supply-side
// observation has no behaviour change. RegisterSupplyEntryDecoders
// returns an empty slice + nil error; the dispatcher remains as
// BuildDispatcher left it.
func TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty(t *testing.T) {
	disp := dispatcher.New()
	registered, err := RegisterSupplyEntryDecoders(disp, config.SupplyConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("registered = %v, want empty (no watched accounts)", registered)
	}
}

// TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched
// confirms the accounts observer attaches when SDFReserveAccounts
// is non-empty. The returned slice carries the source name for boot-
// log clarity. Dispatcher state is exercised via the observer's
// public name.
func TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg)
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 {
		t.Fatalf("registered = %v, want exactly 1 (accounts)", registered)
	}
	if registered[0] != "accounts" {
		t.Errorf("registered[0] = %q, want 'accounts'", registered[0])
	}
}

// TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey reaches into
// accounts.NewObserver's defensive guard: an SDFReserveAccounts
// list that survived config.Validate but contains an empty string
// (e.g. a programmatic builder typo) must surface as a hard error
// at registration time. Operators see a misconfiguration before
// the indexer starts processing ledgers, not later.
func TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey, ""},
	}
	_, err := RegisterSupplyEntryDecoders(disp, cfg)
	if err == nil {
		t.Fatal("expected error for empty G-strkey in watched-accounts list")
	}
}
