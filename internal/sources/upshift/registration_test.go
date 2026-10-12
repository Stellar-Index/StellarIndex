// This file is an external test package on purpose: the registries it checks (projector, pipeline sink, gated
// registry) import the source package, so an in-package test would create an import cycle.
package upshift_test

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const source = upshift.SourceName

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
	// The contract-id prefilter is not optional: this source's symbols are deposit, withdraw and transfer, and
	// without it a far-behind catch-up window streams the CAP-67 firehose, of which transfer alone is the large
	// majority.
	for _, vault := range upshift.MainnetGatedSet() {
		if !slices.Contains(got.PrefilterContractIDs(), vault) {
			t.Errorf("the prefilter omits curated vault %s — its events would never be read", vault)
		}
	}
	// The topic-exclusion the DEX sources use lists `transfer` and
	// `approve`, two of this source's own symbols, so it must stay unset.
	if len(got.ExcludeTopic0Syms) != 0 {
		t.Errorf("ExcludeTopic0Syms = %v — it would drop this source's own transfer events",
			got.ExcludeTopic0Syms)
	}
}

// TestRegistration_SinkProjectsTheVaultEvent — the sink arm that decides
// whether the projector or the dispatcher goroutine owns the write.
func TestRegistration_SinkProjectsTheVaultEvent(t *testing.T) {
	if !pipeline.IsProjectedEvent(upshift.Event{}) {
		t.Fatal("upshift.Event is not recognised as projected — the projector would write nothing " +
			"and the dispatcher would double-write")
	}
}

// TestRegistration_GatedRegistryCarriesTheCuratedSet covers the ADR-0035/0040 warm: a missing entry means the
// identity gate never resumes from protocol_contracts across a restart, silently dropping events of an
// operator-admitted vault. Factories and CreationSym must stay empty: neither vault has a creation event.
func TestRegistration_GatedRegistryCarriesTheCuratedSet(t *testing.T) {
	meta, ok := pipeline.GatedMetaFor(source)
	if !ok {
		t.Fatalf("%s is not registered as a gated source", source)
	}
	if len(meta.Factories) != 0 || meta.CreationSym != "" {
		t.Errorf("factories=%v creationSym=%q — this protocol has no factory namespace",
			meta.Factories, meta.CreationSym)
	}
	if !slices.Equal(meta.CuratedSet, upshift.MainnetGatedSet()) {
		t.Errorf("curated set = %v, want %v", meta.CuratedSet, upshift.MainnetGatedSet())
	}
	if meta.Genesis != upshift.GenesisLedger {
		t.Errorf("genesis = %d, want %d", meta.Genesis, upshift.GenesisLedger)
	}
	if meta.NewDecoder == nil {
		t.Fatal("no NewDecoder — the seed-protocol-contracts CLI cannot drive this source")
	}
	dec := meta.NewDecoder(contractid.WithSeed([]string{"CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"}))
	if dec.Name() != source {
		t.Errorf("constructed decoder names itself %q", dec.Name())
	}
	if !slices.Contains(pipeline.GatedSourceNames(), source) {
		t.Errorf("%s missing from the gated source list", source)
	}
}

// TestRegistration_NetworkApplicability pins that every source is classified: an unclassified source is silently
// dropped from every non-pubnet coverage verdict and the gap detector, indexing but never appearing in the verdict.
func TestRegistration_NetworkApplicability(t *testing.T) {
	if !sourcenet.Known(source) {
		t.Fatalf("%s is not classified in sourcenet — it would be missing from every network's verdict", source)
	}
	if ok, _ := sourcenet.Applicable(source, "pubnet"); !ok {
		t.Error("source is not applicable on pubnet")
	}
	if ok, reason := sourcenet.Applicable(source, "testnet"); ok {
		t.Errorf("source claims to be applicable on testnet (%q) — the vaults are pubnet-only", reason)
	}
}

// TestRegistration_SourceMetadata pins that omission makes the source invisible to /v1/sources and the aggregator's
// class lookup. The vaults must not be in VWAP: their events are derivative actions on other protocols' prices, not
// price observations.
func TestRegistration_SourceMetadata(t *testing.T) {
	meta := external.Lookup(source)
	if meta.Class != external.ClassRouter {
		t.Errorf("class = %v, want %v (aggregator vault, same as DeFindex)", meta.Class, external.ClassRouter)
	}
	if meta.IncludeInVWAP {
		t.Error("the vaults are in VWAP — a vault share is not a price observation")
	}
	// A single per-source decimals value cannot be right for a source
	// whose events carry `assets` and `shares` on different scales.
	if meta.AmountDecimals != 0 {
		t.Errorf("AmountDecimals = %d; assets and shares are on different scales, so no single "+
			"value is correct here", meta.AmountDecimals)
	}
	// BackfillSafe stays false until a WASM audit page records that the
	// decoder handles every version that ran over a replay range.
	if meta.BackfillSafe() {
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
		if tgt.Table != "upshift_vault_events" {
			t.Errorf("target table = %s, want upshift_vault_events", tgt.Table)
		}
		// The whole table belongs to this source — it writes no trades —
		// so a WHERE filter would be dead weight, and a WRONG one would
		// silently scope the density signal to a subset.
		if tgt.WhereFilter != "" {
			t.Errorf("target filter = %q, want empty (the whole table is this source)", tgt.WhereFilter)
		}
		if tgt.Genesis != int64(upshift.GenesisLedger) {
			t.Errorf("target genesis = %d, want %d", tgt.Genesis, upshift.GenesisLedger)
		}
		// Measured: the widest quiet window across both vaults' whole
		// history is 334,407 ledgers. A threshold at or below that would
		// page on the protocol simply being quiet.
		if tgt.MinGapSizeOverride <= 334_407 {
			t.Errorf("MinGapSizeOverride = %d, at or below the widest observed quiet window (334,407) — "+
				"the detector would fire on normal institutional-vault sparsity",
				tgt.MinGapSizeOverride)
		}
		if !sourcenet.Known(tgt.SourceNetKey()) {
			t.Errorf("target %q resolves to an unclassified source key %q", tgt.Source, tgt.SourceNetKey())
		}
	}
	if !found {
		t.Fatalf("no gap-detector target for %s — a stall in this source would raise nothing", source)
	}
}

// TestRegistration_NotAnAMMSignerSource is the deliberate NEGATIVE of sushiswap's
// AMMSignerSources check. That sweeper backfills a taker identity onto AMM rows in `trades`;
// this source writes no trades and its rows already carry the actor in `caller`.
func TestRegistration_NotAnAMMSignerSource(t *testing.T) {
	if slices.Contains(timescale.AMMSignerSources, source) {
		t.Errorf("%s is in AMMSignerSources but writes no trades rows", source)
	}
}
