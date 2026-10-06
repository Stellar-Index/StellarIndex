// External test package: the registries checked here import the source
// package, so an in-package test would be an import cycle.
package spectra_test

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const source = spectra.SourceName

func TestRegistration_ConfigAcceptsTheSource(t *testing.T) {
	if _, ok := config.KnownSources[source]; !ok {
		t.Fatalf("%s missing from config.KnownSources — enabling it would fail config validation", source)
	}
}

func TestRegistration_DispatcherBuildsTheDecoder(t *testing.T) {
	if _, err := pipeline.BuildDispatcher([]string{source}, config.OracleConfig{}, nil); err != nil {
		t.Fatalf("BuildDispatcher(%s): %v", source, err)
	}
}

// TestRegistration_ProjectorBuildsTheSource — the projector is the one
// writer for spectra_events (ADR-0031/0032).
func TestRegistration_ProjectorBuildsTheSource(t *testing.T) {
	reg, err := projector.BuildRegistry([]string{source}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry(%s): %v", source, err)
	}
	if len(reg.Sources) != 1 || reg.Sources[0].Name != source {
		t.Fatalf("registry = %+v, want exactly %s", reg.Sources, source)
	}
	got := reg.Sources[0]
	if got.Genesis != spectra.GenesisLedger {
		t.Errorf("genesis = %d, want %d", got.Genesis, spectra.GenesisLedger)
	}
	// Every hand-kept contract and the infrastructure must be read; the
	// factory above all, since it is the trust root that admits new PTs.
	want := append(spectra.MainnetGatedSet(), spectra.MainnetInfrastructure...)
	for _, id := range want {
		if !slices.Contains(got.PrefilterContractIDs(), id) {
			t.Errorf("the prefilter omits %s — its events would never be read", id)
		}
	}
	// The DEX firehose exclusion lists `transfer`, a PT/YT row kind.
	if len(got.ExcludeTopic0Syms) != 0 {
		t.Errorf("ExcludeTopic0Syms = %v — it would drop PT/YT transfer rows", got.ExcludeTopic0Syms)
	}
}

func TestRegistration_SinkProjectsTheEvent(t *testing.T) {
	if !pipeline.IsProjectedEvent(spectra.Event{}) {
		t.Fatal("spectra.Event is not recognised as projected — the projector would write nothing " +
			"and the dispatcher would double-write")
	}
}

func TestRegistration_NetworkApplicability(t *testing.T) {
	if !sourcenet.Known(source) {
		t.Fatalf("%s is not classified in sourcenet — it would be missing from every network's verdict", source)
	}
	if ok, _ := sourcenet.Applicable(source, "pubnet"); !ok {
		t.Error("source is not applicable on pubnet")
	}
	if ok, reason := sourcenet.Applicable(source, "testnet"); ok {
		t.Errorf("source claims to be applicable on testnet (%q) — its contracts are pubnet-only", reason)
	}
}

// TestRegistration_SourceMetadata — PT/YT activity is derivative of the
// underlying IBT, so it must never feed a price.
func TestRegistration_SourceMetadata(t *testing.T) {
	meta := external.Lookup(source)
	if meta.Class != external.ClassRouter {
		t.Errorf("class = %v, want %v", meta.Class, external.ClassRouter)
	}
	if meta.IncludeInVWAP {
		t.Error("spectra is in VWAP — a PT/YT movement is not a price observation")
	}
	// Decimals are per market (MainnetContracts), so no single value is right.
	if meta.AmountDecimals != 0 {
		t.Errorf("AmountDecimals = %d, want 0 (decimals are per market)", meta.AmountDecimals)
	}
	// The replay gate checks each active hash against audited_wasm.json.
	if meta.Backfill != external.BackfillPerWASM {
		t.Errorf("Backfill = %v, want BackfillPerWASM (docs/operations/wasm-audits/spectra.md)", meta.Backfill)
	}
}

func TestRegistration_GapDetectorTarget(t *testing.T) {
	var found bool
	for _, tgt := range timescale.DefaultGapDetectorTargets {
		if tgt.Source != source {
			continue
		}
		found = true
		if tgt.Table != "spectra_events" {
			t.Errorf("target table = %s, want spectra_events", tgt.Table)
		}
		if tgt.WhereFilter != "" {
			t.Errorf("target filter = %q, want empty (the whole table is this source)", tgt.WhereFilter)
		}
		if tgt.Genesis != int64(spectra.GenesisLedger) {
			t.Errorf("target genesis = %d, want %d", tgt.Genesis, spectra.GenesisLedger)
		}
		if !sourcenet.Known(tgt.SourceNetKey()) {
			t.Errorf("target %q resolves to an unclassified source key %q", tgt.Source, tgt.SourceNetKey())
		}
	}
	if !found {
		t.Fatalf("no gap-detector target for %s — a stall in this source would raise nothing", source)
	}
}

// TestRegistration_NotAnAMMSignerSource — spectra writes no trades rows, so
// the taker-identity sweeper has nothing to backfill for it.
func TestRegistration_NotAnAMMSignerSource(t *testing.T) {
	if slices.Contains(timescale.AMMSignerSources, source) {
		t.Errorf("%s is in AMMSignerSources but writes no trades rows", source)
	}
}
