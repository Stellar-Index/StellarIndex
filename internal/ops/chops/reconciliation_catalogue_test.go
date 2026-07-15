// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// testWatchedSEP41 is a syntactically valid C-strkey watched set; the
// catalogue builders only check non-emptiness of each entry.
var testWatchedSEP41 = []string{
	"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
	"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
}

// TestBuildReconciliationCatalogue_PromotesSEP41WhenWatched pins the
// 2026-07-11 promotion: now that the full-history truncate+re-derive
// (`ch-rebuild -sep41 -write`, windows 50.0M→63.42M, rc=0) has purged
// every pre-migration-0057 collapsed row, a configured watched set makes
// the DEFAULT catalogue (compute-completeness / ch-reproject /
// verify-reconciliation) carry sep41_transfers + sep41_supply with the
// same wiring buildSEP41ReconSources documents (genesis, contractIDs
// prefilter, kinds, strict per-ledger reconcile) — see
// TestBuildSEP41ReconSources_OptIn for the field-level assertions this
// mirrors.
func TestBuildReconciliationCatalogue_PromotesSEP41WhenWatched(t *testing.T) {
	cfg := testConfigWithAllSources()
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41

	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	want := map[string]bool{"sep41_transfers": false, "sep41_supply": false}
	for _, src := range cat {
		if _, ok := want[src.name]; ok {
			want[src.name] = true
			if src.genesis != 50_457_424 {
				t.Errorf("%s: genesis = %d, want 50_457_424 (sorobanEraGenesis)", src.name, src.genesis)
			}
			if src.aggregateReconcile != "" {
				t.Errorf("%s: must stay on the strict per-ledger reconcile (CS-084), got opt-out %q", src.name, src.aggregateReconcile)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("default catalogue missing %s — buildReconciliationCatalogue must promote it when a watched set is configured", name)
		}
	}
}

// TestBuildReconciliationCatalogue_NoSEP41WithoutWatchedSet asserts the
// promotion is silent-skip, not an error, for a deployment that never
// opted into SEP-41 capture — mirroring the dispatcher's own
// non-opted-in behavior (buildSEP41ReconSources itself errors on an
// empty watched set; buildReconciliationCatalogue must never surface
// that error for a plain unconfigured deployment).
func TestBuildReconciliationCatalogue_NoSEP41WithoutWatchedSet(t *testing.T) {
	cfg := testConfigWithAllSources() // WatchedSEP41Contracts left empty

	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: unexpected error with no watched set: %v", err)
	}
	for _, src := range cat {
		if src.name == "sep41_transfers" || src.name == "sep41_supply" {
			t.Errorf("catalogue contains %s with an empty watched set — promotion must be gated on non-emptiness", src.name)
		}
	}
}

// TestBuildSEP41ReconSources_OptIn asserts the opt-in variant produces
// both sep41 sources with the documented kind + genesis + prefilter
// wiring (CLAUDE.md recipe: cfg.Supply.WatchedSEP41Contracts +
// contractIDs prefilter; kinds "sep41_transfers.event" /
// "sep41_supply.event"; genesis 50_457_424).
func TestBuildSEP41ReconSources_OptIn(t *testing.T) {
	cfg := config.Config{}
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41

	cat, err := buildSEP41ReconSources(cfg)
	if err != nil {
		t.Fatalf("buildSEP41ReconSources: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("got %d sources, want 2 (sep41_transfers + sep41_supply)", len(cat))
	}

	want := map[string]struct {
		table string
		kind  string
	}{
		"sep41_transfers": {table: "sep41_transfers", kind: "sep41_transfers.event"},
		"sep41_supply":    {table: "sep41_supply_events", kind: "sep41_supply.event"},
	}
	for _, src := range cat {
		w, ok := want[src.name]
		if !ok {
			t.Errorf("unexpected source %q", src.name)
			continue
		}
		delete(want, src.name)
		if src.genesis != 50_457_424 {
			t.Errorf("%s: genesis = %d, want 50_457_424 (sorobanEraGenesis)", src.name, src.genesis)
		}
		if src.dec == nil {
			t.Errorf("%s: nil decoder", src.name)
		}
		if len(src.contractIDs) != len(testWatchedSEP41) {
			t.Errorf("%s: contractIDs prefilter = %v, want the watched set %v", src.name, src.contractIDs, testWatchedSEP41)
		}
		if len(src.targets) != 1 {
			t.Fatalf("%s: %d targets, want 1", src.name, len(src.targets))
		}
		tgt := src.targets[0]
		if tgt.table != w.table {
			t.Errorf("%s: table = %q, want %q", src.name, tgt.table, w.table)
		}
		if len(tgt.kinds) != 1 || tgt.kinds[0] != w.kind {
			t.Errorf("%s: kinds = %v, want [%q]", src.name, tgt.kinds, w.kind)
		}
		if tgt.whereFilter != "" {
			t.Errorf("%s: whereFilter = %q, want whole-table ownership", src.name, tgt.whereFilter)
		}
		if src.aggregateReconcile != "" {
			t.Errorf("%s: must stay on the strict per-ledger reconcile (CS-084), got opt-out %q", src.name, src.aggregateReconcile)
		}
	}
	for name := range want {
		t.Errorf("missing source %q", name)
	}
}

// TestBuildSEP41ReconSources_EmptyWatchedSetErrors — -sep41 with no
// configured watched set is an operator error, not a silent no-op.
func TestBuildSEP41ReconSources_EmptyWatchedSetErrors(t *testing.T) {
	if _, err := buildSEP41ReconSources(config.Config{}); err == nil {
		t.Fatal("expected error for empty [supply] watched_sep41_contracts, got nil")
	}
}

// TestCatalogue_OpArgsOnlyForRedstone pins the 2026-07-08 wide-column trim:
// redstone is the ONLY decoder that consumes events.Event.OpArgs (write_prices
// feed-id zip, PR 166), so it alone may ask the -ch reconcile to read the wide
// op_args_xdr column. Every other source — critically the sep41 pair (promoted
// into the catalogue as of 2026-07-11, whose reconcile streams the CAP-67
// firehose) — must keep needsOpArgs false, or the lake read regrows the memory
// profile that OOM-killed compute-completeness at any ClickHouse server cap.
func TestCatalogue_OpArgsOnlyForRedstone(t *testing.T) {
	cfg := testConfigWithAllSources()
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41

	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	sawSEP41 := map[string]bool{"sep41_transfers": false, "sep41_supply": false}
	sawRedstone := false
	for _, src := range cat {
		if _, ok := sawSEP41[src.name]; ok {
			sawSEP41[src.name] = true
		}
		if src.name == "redstone" {
			sawRedstone = true
			if !src.needsOpArgs {
				t.Error("redstone must set needsOpArgs (its decoder zips feed_ids from op args)")
			}
			continue
		}
		if src.needsOpArgs {
			t.Errorf("%s: needsOpArgs set but its decoder never reads events.Event.OpArgs — this re-widens the reconcile's lake read", src.name)
		}
	}
	if !sawRedstone {
		t.Fatal("catalogue missing redstone (test config sets its adapter contract)")
	}
	for name, seen := range sawSEP41 {
		if !seen {
			t.Fatalf("catalogue missing %s (test config sets a watched set — promotion should have included it)", name)
		}
	}
}
