package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// realisticGStrkey is a syntactically-valid 56-char G-prefixed strkey fixture; accounts.NewObserver only checks non-
// empty, and strict validation happens upstream at config.Validate.
const realisticGStrkey = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty pins the safe default: no supply opt-in means no registration and no error.
func TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty(t *testing.T) {
	disp := dispatcher.New()
	registered, err := RegisterSupplyEntryDecoders(disp, config.SupplyConfig{}, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("registered = %v, want empty (no watched accounts)", registered)
	}
}

// TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched: the accounts observer attaches when SDFReserveAccounts is non-empty.
func TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
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

// TestRegisterSupplyEntryDecoders_AccountsRegistersForIssuersOnly pins that issuer home_domain observation is
// decoupled from the SDF reserve list: watching an issuer must not require sdf_reserve_accounts, which would subtract
// its XLM from supply.
func TestRegisterSupplyEntryDecoders_AccountsRegistersForIssuersOnly(t *testing.T) {
	disp := dispatcher.New()
	meta := config.MetadataConfig{WatchedIssuerAccounts: []string{realisticGStrkey}}
	registered, err := RegisterSupplyEntryDecoders(disp, config.SupplyConfig{}, meta)
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 || registered[0] != "accounts" {
		t.Fatalf("registered = %v, want [accounts]", registered)
	}
}

func TestAccountObserverWatchSet_UnionDeduped(t *testing.T) {
	const issuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	sup := config.SupplyConfig{SDFReserveAccounts: []string{realisticGStrkey}}
	meta := config.MetadataConfig{WatchedIssuerAccounts: []string{issuer, realisticGStrkey}}
	got := AccountObserverWatchSet(sup, meta)
	want := []string{realisticGStrkey, issuer}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AccountObserverWatchSet = %v, want %v", got, want)
	}
	if len(sup.SDFReserveAccounts) != 1 {
		t.Fatalf("SDFReserveAccounts mutated to %v; issuers must never join the reserve list", sup.SDFReserveAccounts)
	}
}

// TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey: an empty string in SDFReserveAccounts must fail at registration, not mid-ingest.
func TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey, ""},
	}
	_, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err == nil {
		t.Fatal("expected error for empty G-strkey in watched-accounts list")
	}
}

// TestRegisterSupplyEntryDecoders_ClassicTrioRegisters: trustlines, claimable_balances and liquidity_pools all attach together, since Algorithm 2 sums all three per asset.
func TestRegisterSupplyEntryDecoders_ClassicTrioRegisters(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		WatchedClassicAssets: []string{"USDC-" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	wantSet := map[string]bool{
		"trustlines":         true,
		"claimable_balances": true,
		"liquidity_pools":    true,
	}
	gotSet := map[string]bool{}
	for _, name := range registered {
		gotSet[name] = true
	}
	for name := range wantSet {
		if !gotSet[name] {
			t.Errorf("registered set missing %q (got %v)", name, registered)
		}
	}
}

// TestRegisterSupplyEntryDecoders_SACRegistersWhenWrappers: the SAC observer attaches from sac_wrappers alone, independent of WatchedClassicAssets.
func TestRegisterSupplyEntryDecoders_SACRegistersWhenWrappers(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SACWrappers: map[string]string{realisticCStrkey: "USDC:" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 || registered[0] != "sac_balances" {
		t.Errorf("registered = %v, want [sac_balances]", registered)
	}
}

// TestRegisterSupplyEntryDecoders_FullConfigRegistersFive: with every watched set populated, every
// LedgerEntryChangeDecoder is registered. sep41_supply is an event-stream Decoder, outside this helper.
func TestRegisterSupplyEntryDecoders_FullConfigRegistersFive(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts:   []string{realisticGStrkey},
		WatchedClassicAssets: []string{"USDC-" + realisticGStrkey},
		SACWrappers:          map[string]string{realisticCStrkey: "USDC:" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 5 {
		t.Errorf("registered count = %d, want 5 (accounts + trustlines + claimable + lp + sac); got %v",
			len(registered), registered)
	}
}

// realisticCStrkey is a 56-char C-prefixed Soroban contract id.
// Mirrors the supply-package test fixture pattern. Public ledger
// entry — not a credential.
const realisticCStrkey = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUOZWS4HG3B5UPHHC2QQA"

// TestRegisterSupplyEventDecoders_NoOpWhenEmpty pins the safe default: empty WatchedSEP41Contracts attaches no
// decoder.
func TestRegisterSupplyEventDecoders_NoOpWhenEmpty(t *testing.T) {
	disp := dispatcher.New()
	registered, err := RegisterSupplyEventDecoders(disp, config.SupplyConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEventDecoders: %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("registered = %v, want empty", registered)
	}
}

// TestRegisterSupplyEventDecoders_RegistersWhenWatched: sep41_supply and sep41_transfers attach when WatchedSEP41Contracts is set.
func TestRegisterSupplyEventDecoders_RegistersWhenWatched(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		WatchedSEP41Contracts: []string{realisticCStrkey},
	}
	registered, err := RegisterSupplyEventDecoders(disp, cfg)
	if err != nil {
		t.Fatalf("RegisterSupplyEventDecoders: %v", err)
	}
	want := []string{"sep41_supply", "sep41_transfers"}
	if len(registered) != len(want) {
		t.Fatalf("registered = %v, want %v", registered, want)
	}
	for i, w := range want {
		if registered[i] != w {
			t.Errorf("registered[%d] = %q, want %q", i, registered[i], w)
		}
	}
}
