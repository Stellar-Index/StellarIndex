// This file is an EXTERNAL test package on purpose. The registries it
// checks (the projector, the pipeline sink, the gated registry) import the
// source package, so an in-package test could not reach them without an
// import cycle.
package sushiswap_v3_test

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const source = sushiswap_v3.SourceName

// TestRegistration_ConfigAcceptsTheSource — an enabled_sources entry the
// config rejects means the source can never be turned on.
func TestRegistration_ConfigAcceptsTheSource(t *testing.T) {
	if _, ok := config.KnownSources[source]; !ok {
		t.Fatalf("%s missing from config.KnownSources — enabling it would fail config validation", source)
	}
}

// TestRegistration_DispatcherBuildsTheDecoder — the live ingest path.
func TestRegistration_DispatcherBuildsTheDecoder(t *testing.T) {
	if _, err := pipeline.BuildDispatcher([]string{source}, config.OracleConfig{}, nil); err != nil {
		t.Fatalf("BuildDispatcher(%s): %v", source, err)
	}
}

// TestRegistration_ProjectorBuildsTheSource — the projector is the ONE
// writer for this domain (ADR-0031/0032). A source with no buildSource case
// is skipped by the sink AND never projected: its rows land nowhere.
func TestRegistration_ProjectorBuildsTheSource(t *testing.T) {
	reg, err := projector.BuildRegistry([]string{source}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry(%s): %v", source, err)
	}
	if len(reg.Sources) != 1 {
		t.Fatalf("registry has %d sources, want 1", len(reg.Sources))
	}
	got := reg.Sources[0]
	if got.Name != source {
		t.Fatalf("source name = %s, want %s", got.Name, source)
	}
	// The contract-id prefilter is not optional here. Without it the
	// per-cycle lake read is unscoped, and this source's own `mint` and
	// `burn` symbols are 33% and 12% of all pubnet contract events — a
	// far-behind catch-up window would stream the CAP-67 firehose and
	// wedge the source.
	if len(got.ContractIDs) == 0 {
		t.Fatal("projector source has no contract-id prefilter")
	}
	if !slices.Contains(got.ContractIDs, sushiswap_v3.MainnetFactory) {
		t.Error("the prefilter omits the factory — no new pool could ever register")
	}
	for pool := range sushiswap_v3.MainnetPools {
		if !slices.Contains(got.ContractIDs, pool) {
			t.Errorf("the prefilter omits curated pool %s", pool)
		}
	}
	// The topic-exclusion the other DEX sources use would drop this
	// source's own mint/burn events, so it must stay unset.
	if len(got.ExcludeTopic0Syms) != 0 {
		t.Errorf("ExcludeTopic0Syms = %v — it would drop this source's own mint/burn events",
			got.ExcludeTopic0Syms)
	}
}

// TestRegistration_SinkProjectsTheTradeEvent — the sink arm that decides
// whether the projector or the dispatcher goroutine owns the write.
func TestRegistration_SinkProjectsTheTradeEvent(t *testing.T) {
	if !pipeline.IsProjectedEvent(sushiswap_v3.TradeEvent{}) {
		t.Fatal("TradeEvent is not recognised as projected — the projector would write nothing and the dispatcher would double-write")
	}
}

// TestRegistration_GatedRegistryCarriesTheFactory — the ADR-0035 warm. A
// missing entry means the identity gate never resumes from
// protocol_contracts across a restart, so a pool created after the curated
// table was frozen has its events silently dropped.
func TestRegistration_GatedRegistryCarriesTheFactory(t *testing.T) {
	meta, ok := pipeline.GatedMetaFor(source)
	if !ok {
		t.Fatalf("%s is not registered as a gated source", source)
	}
	if !slices.Equal(meta.Factories, sushiswap_v3.MainnetFactories) {
		t.Errorf("factories = %v, want %v", meta.Factories, sushiswap_v3.MainnetFactories)
	}
	if meta.CreationSym != sushiswap_v3.EventPoolCreated {
		t.Errorf("creation symbol = %q, want %q", meta.CreationSym, sushiswap_v3.EventPoolCreated)
	}
	if meta.Genesis != sushiswap_v3.FactoryGenesisLedger {
		t.Errorf("genesis = %d, want %d", meta.Genesis, sushiswap_v3.FactoryGenesisLedger)
	}
	if meta.NewDecoder == nil {
		t.Fatal("no NewDecoder — the genesis-seed CLI cannot drive this source")
	}
	dec := meta.NewDecoder(contractid.WithSeed([]string{"CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"}))
	if dec.Name() != source {
		t.Errorf("constructed decoder names itself %q", dec.Name())
	}
	if !slices.Contains(pipeline.GatedSourceNames(), source) {
		t.Errorf("%s missing from the gated source list", source)
	}
}

// TestRegistration_NetworkApplicability — a source not classified here is
// silently dropped from every non-pubnet coverage verdict AND from the gap
// detector, which is exactly the "indexes but never appears in the verdict"
// class.
func TestRegistration_NetworkApplicability(t *testing.T) {
	if !sourcenet.Known(source) {
		t.Fatalf("%s is not classified in sourcenet — it would be missing from every network's verdict", source)
	}
	if ok, _ := sourcenet.Applicable(source, "pubnet"); !ok {
		t.Error("source is not applicable on pubnet")
	}
	if ok, reason := sourcenet.Applicable(source, "testnet"); ok {
		t.Errorf("source claims to be applicable on testnet (%q) — the protocol is pubnet-only", reason)
	}
}

// TestRegistration_SourceMetadata — omission here excludes the source from
// VWAP, from /v1/sources, and from every USD-volume tier that reads
// dexSourceNames().
func TestRegistration_SourceMetadata(t *testing.T) {
	meta := external.Lookup(source)
	if meta.Class != external.ClassExchange || meta.Subclass != external.SubclassDEX {
		t.Fatalf("class/subclass = %v/%v, want exchange/dex", meta.Class, meta.Subclass)
	}
	if !meta.IncludeInVWAP {
		t.Error("source is excluded from VWAP")
	}
	// BackfillSafe stays false until a WASM audit page records that the
	// decoder handles every version that ran over a replay range.
	if meta.BackfillSafe {
		t.Error("BackfillSafe is true without a WASM audit page")
	}
}

// TestRegistration_GapDetectorTarget — without a target there is no density
// tripwire, so a stalled source is invisible until someone looks.
func TestRegistration_GapDetectorTarget(t *testing.T) {
	var found bool
	for _, tgt := range timescale.DefaultGapDetectorTargets {
		if tgt.Source != source {
			continue
		}
		found = true
		if tgt.Table != "trades" {
			t.Errorf("target table = %s, want trades", tgt.Table)
		}
		if tgt.WhereFilter != "source = '"+source+"'" {
			t.Errorf("target filter = %q", tgt.WhereFilter)
		}
		if tgt.Genesis != int64(sushiswap_v3.FactoryGenesisLedger) {
			t.Errorf("target genesis = %d, want %d", tgt.Genesis, sushiswap_v3.FactoryGenesisLedger)
		}
		if !sourcenet.Known(tgt.SourceNetKey()) {
			t.Errorf("target %q resolves to an unclassified source key %q", tgt.Source, tgt.SourceNetKey())
		}
	}
	if !found {
		t.Fatalf("no gap-detector target for %s — a stall in this source would raise nothing", source)
	}
}

// TestRegistration_AMMSignerSource — the decoder leaves maker empty (an AMM
// has no resting counterparty), so the signer sweeper must know to tag the
// tx source account as the initiator for this source's rows.
func TestRegistration_AMMSignerSource(t *testing.T) {
	if !slices.Contains(timescale.AMMSignerSources, source) {
		t.Fatalf("%s missing from AMMSignerSources — its trades would carry neither maker nor signer", source)
	}
}
