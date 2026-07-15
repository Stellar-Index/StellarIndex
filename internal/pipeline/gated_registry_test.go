package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
)

func TestGatedMetaFor_blend(t *testing.T) {
	m, ok := GatedMetaFor(blend.SourceName)
	if !ok {
		t.Fatal("blend should be a gated source")
	}
	// Blend has MORE THAN ONE factory (it was redeployed) — both must be
	// present, else the missing factory's pools are silently dropped.
	if len(m.Factories) < 2 {
		t.Errorf("blend factories=%v, want at least 2 (V1 + V2)", m.Factories)
	}
	wantFac := map[string]bool{blend.MainnetPoolFactory: false, blend.MainnetPoolFactoryV1: false}
	for _, f := range m.Factories {
		if _, ok := wantFac[f]; ok {
			wantFac[f] = true
		}
	}
	for f, seen := range wantFac {
		if !seen {
			t.Errorf("blend factory set missing %q", f)
		}
	}
	if m.CreationSym != blend.EventDeploy {
		t.Errorf("creationSym=%q want %q", m.CreationSym, blend.EventDeploy)
	}
	if m.Genesis != blend.FactoryGenesisLedger {
		t.Errorf("genesis=%d want %d", m.Genesis, blend.FactoryGenesisLedger)
	}
	if m.NewDecoder == nil {
		t.Fatal("NewDecoder must be non-nil")
	}
	// The constructor must forward contractid (child-gate) options to a real, gated
	// decoder: a seeded pool's event matches, an unseeded one does not.
	const pool = "CDPOOLSEEDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	dec := m.NewDecoder(contractid.WithSeed([]string{pool}))
	if !dec.Matches(events.Event{Topic: []string{blend.TopicSymbolSupply}, ContractID: pool}) {
		t.Error("seeded pool event should match through the constructed decoder")
	}
	if dec.Matches(events.Event{Topic: []string{blend.TopicSymbolSupply}, ContractID: "COTHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}) {
		t.Error("unseeded contract event must not match")
	}
}

func TestGatedMetaFor_unknown(t *testing.T) {
	if _, ok := GatedMetaFor("not-a-source"); ok {
		t.Error("unknown source should not be gated")
	}
	if GatedFactories("not-a-source") != nil {
		t.Error("GatedFactories of unknown source should be nil")
	}
}

func TestGatedSourceNames_includesBlend(t *testing.T) {
	found := false
	for _, n := range GatedSourceNames() {
		if n == blend.SourceName {
			found = true
		}
	}
	if !found {
		t.Errorf("GatedSourceNames %v should include blend", GatedSourceNames())
	}
}

func TestGatedMetaFor_aquarius(t *testing.T) {
	m, ok := GatedMetaFor(aquarius.SourceName)
	if !ok {
		t.Fatal("aquarius should be a gated source")
	}
	if len(m.Factories) != 1 || m.Factories[0] != aquarius.MainnetRouter {
		t.Errorf("aquarius factories=%v, want the canonical router only (the parallel router deployment and the look-alike are deliberately excluded — docs/protocols/aquarius.md)", m.Factories)
	}
	if m.CreationSym != aquarius.EventAddPool {
		t.Errorf("creationSym=%q want %q", m.CreationSym, aquarius.EventAddPool)
	}
	if m.NewDecoder == nil {
		t.Fatal("NewDecoder must be non-nil")
	}
	// Constructor must forward contractid options: a warm-seeded pool's
	// trade matches, an unseeded one does not (curated in-code seed
	// aside, which the constructor always carries).
	const pool = "CDPOOLSEEDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	dec := m.NewDecoder(contractid.WithSeed([]string{pool}))
	tradeTopics := []string{aquarius.TopicSymbolTrade, "t1", "t2", "t3"}
	if !dec.Matches(events.Event{Topic: tradeTopics, ContractID: pool}) {
		t.Error("seeded pool trade should match through the constructed decoder")
	}
	if dec.Matches(events.Event{Topic: tradeTopics, ContractID: "COTHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}) {
		t.Error("unseeded contract trade must not match")
	}
}

func TestGatedMetaFor_defindex(t *testing.T) {
	m, ok := GatedMetaFor(defindex.SourceName)
	if !ok {
		t.Fatal("defindex should be a gated source")
	}
	// DeFindex has FOUR factories (redeployed protocol) — all must be
	// present, else the missing factory's events are silently dropped.
	if len(m.Factories) != len(defindex.MainnetFactories) {
		t.Errorf("defindex factories=%v, want all of %v", m.Factories, defindex.MainnetFactories)
	}
	if m.NewDecoder == nil {
		t.Fatal("NewDecoder must be non-nil")
	}
	const vault = "CDVAULTSEEDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	dec := m.NewDecoder(contractid.WithSeed([]string{vault}))
	vaultTopics := []string{defindex.TopicPrefixVault, defindex.TopicSymbolDeposit}
	if !dec.Matches(events.Event{Topic: vaultTopics, ContractID: vault}) {
		t.Error("seeded vault event should match through the constructed decoder")
	}
	if dec.Matches(events.Event{Topic: vaultTopics, ContractID: "COTHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}) {
		t.Error("unseeded contract event must not match")
	}
}
