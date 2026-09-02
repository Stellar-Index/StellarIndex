// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// testWatchedSEP41 is a syntactically valid C-strkey watched set; the
// catalogue builders only check non-emptiness of each entry.
var testWatchedSEP41 = []string{
	"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
	"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
}

// wantSEP41Filter is the served-side predicate both sep41 targets must
// carry for testWatchedSEP41 — sorted, single-quoted, `contract_id IN`.
// Spelled out literally rather than re-derived via contractIDFilter so
// the test cannot agree with a broken builder.
const wantSEP41Filter = "contract_id IN (" +
	"'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75', " +
	"'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC')"

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

// TestBuildReconciliationCatalogue_GenesisMirrorsProtocolRegistry pins the
// lake-derived exact genesis values for the two sources whose catalogue
// floors had drifted (2026-07-31): cctp's true first on-chain event is
// ledger 62,146,641 (the MessageTransmitter's first event; the stale
// 62_403_000 ingestion-config floor left 410 real served rows permanently
// BELOW the verify floor, structurally out of every verdict) and rozo's is
// 60,829,397 (first event across all four Rozo contracts). The values
// mirror internal/api/v1/protocols_registry.go (fixed there 07-30) —
// spelled as literals here so this test cannot agree with a re-drifted
// catalogue.
func TestBuildReconciliationCatalogue_GenesisMirrorsProtocolRegistry(t *testing.T) {
	want := map[string]uint32{
		"cctp": 62_146_641,
		"rozo": 60_829_397,
	}
	cat, _, err := buildReconciliationCatalogue(testConfigWithAllSources())
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	for _, src := range cat {
		g, ok := want[src.name]
		if !ok {
			continue
		}
		delete(want, src.name)
		if src.genesis != g {
			t.Errorf("%s: genesis = %d, want %d (lake-derived exact first event, mirrors protocols_registry.go) — a late floor leaves real served rows below the verify floor, permanently out-of-verdict", src.name, src.genesis, g)
		}
	}
	for name := range want {
		t.Errorf("catalogue missing source %s", name)
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
		// The sep41 targets are watched-set SLICES of their tables, not
		// whole tables. This assertion used to demand whereFilter == ""
		// (whole-table ownership) — it encoded the defect: the served
		// side then counted every contract's rows while the expected
		// side was gated on the watched set by dec/contractIDs, so any
		// row from a since-unwatched contract was a permanent, never-
		// closing surplus. Pinned to the exact predicate, not merely
		// non-empty, because this string is part of the durable
		// completeness_target_floors key.
		if tgt.whereFilter != wantSEP41Filter {
			t.Errorf("%s: whereFilter = %q, want %q (watched-set scoping matching the expected side)",
				src.name, tgt.whereFilter, wantSEP41Filter)
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

// ─── sep41 served-vs-expected scoping (reconcile defect 4a) ────────
//
// The expected side of the sep41 reconcile is gated on the watched set
// twice over (the decoders' own watched map, and the contractIDs lake
// prefilter). The served side is a plain row count over the target
// table. If the served count is not scoped to the SAME contracts, rows
// for a contract that is no longer watched — dropped from `[supply]
// watched_sep41_contracts`, or captured under a wider set during an
// earlier backfill — inflate served forever while expected can never
// grow to match. The reconcile then reports a surplus that no re-derive
// can close.

// TestSEP41Filter_ScopesToWatchedSetOnly builds two catalogues from
// watched sets that differ by one contract and asserts the served-side
// predicate differs with them. A whole-table filter ("") is identical
// for both — which is exactly the bug — so this fails on any fix that
// leaves the served side unscoped.
func TestSEP41Filter_ScopesToWatchedSetOnly(t *testing.T) {
	const (
		a = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		b = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	)
	filterFor := func(t *testing.T, watched []string) string {
		t.Helper()
		cfg := config.Config{}
		cfg.Supply.WatchedSEP41Contracts = watched
		cat, err := buildSEP41ReconSources(cfg)
		if err != nil {
			t.Fatalf("buildSEP41ReconSources(%v): %v", watched, err)
		}
		return cat[0].targets[0].whereFilter
	}

	both := filterFor(t, []string{a, b})
	justA := filterFor(t, []string{a})

	if both == justA {
		t.Fatalf("served-side filter is identical for watched sets {a,b} and {a}: %q — "+
			"the served count is not scoped to the watched set, so %s's rows count on the "+
			"served side while the expected side cannot produce them", both, b)
	}
	if !strings.Contains(justA, a) {
		t.Errorf("filter for {a} = %q, want it to select %s", justA, a)
	}
	if strings.Contains(justA, b) {
		t.Errorf("filter for {a} = %q, want it NOT to select the unwatched %s", justA, b)
	}
}

// TestSEP41Filter_StableAcrossOrdering — whereFilter is part of the
// durable completeness_target_floors key (timescale.TargetFloorKey), so
// the same watched set listed in a different order must render the same
// predicate. An order-sensitive filter would orphan the recorded floor
// on every config reshuffle and silently disable detectFloorLoss.
func TestSEP41Filter_StableAcrossOrdering(t *testing.T) {
	fwd := config.Config{}
	fwd.Supply.WatchedSEP41Contracts = []string{testWatchedSEP41[0], testWatchedSEP41[1]}
	rev := config.Config{}
	rev.Supply.WatchedSEP41Contracts = []string{testWatchedSEP41[1], testWatchedSEP41[0]}

	catF, err := buildSEP41ReconSources(fwd)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	catR, err := buildSEP41ReconSources(rev)
	if err != nil {
		t.Fatalf("reversed: %v", err)
	}
	if catF[0].targets[0].whereFilter != catR[0].targets[0].whereFilter {
		t.Errorf("filter is order-sensitive:\n forward  = %q\n reversed = %q",
			catF[0].targets[0].whereFilter, catR[0].targets[0].whereFilter)
	}
}

// TestSEP41Filter_RejectsNonStrkey — whereFilter is interpolated into
// SQL by timescale.Store's row-count helpers, and the watched set comes
// from operator config, which only checks non-emptiness. Anything that
// is not a contract C-strkey must fail the build loudly rather than
// reach the query text.
func TestSEP41Filter_RejectsNonStrkey(t *testing.T) {
	for _, bad := range []string{
		"C'); DROP TABLE sep41_transfers; --",
		"GCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75", // account, not contract
		"CCW67TSZ", // truncated
	} {
		cfg := config.Config{}
		cfg.Supply.WatchedSEP41Contracts = []string{bad}
		if _, err := buildSEP41ReconSources(cfg); err == nil {
			t.Errorf("buildSEP41ReconSources accepted %q as a watched contract", bad)
		}
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

// TestBlendEmitterDropFanoutWaived pins the 2026-08-18 blend_emitter
// projection false-red fix. The `drop` event is a FAN-OUT: one decoder
// DropEvent carries N recipients and the sink writes one blend_emitter_events
// row per recipient (recipient_index is a PK component), so a per-ledger
// event-count-vs-served-row-count reconcile false-flags every drop ledger
// (r1-measured 2026-08-18: ledger 51,499,914 = 13 rows / 1 event identity;
// ledger 57,467,292 = 3 / 1 → Σ|Δ|=14, data CORRECT).
//
// Unlike the aquarius_reserves/liquidity fan-out tables — which are ENTIRELY
// fan-out and are waived whole — blend_emitter_events is MIXED: `distribute`
// (465 events) and `q_swap`/`swap` (2) are strictly 1:1. So instead of waiving
// the whole table and losing that 1:1 coverage, the catalogue carves ONLY the
// fan-out drop rows out of the served side (whereFilter `event_kind <> 'drop'`)
// and omits the drop kind from the re-derive: the 467/469 1:1 events keep exact
// per-ledger reconciliation and the 2 drop ledgers are covered by the density
// gap-detector (per_source_gaps.go). This test fails against the pre-fix
// catalogue (kinds included "blend_emitter.drop"; whereFilter was "").
func TestBlendEmitterDropFanoutWaived(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(testConfigWithAllSources())
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	var found bool
	for _, src := range cat {
		if src.name != "blend_emitter" {
			continue
		}
		found = true
		if len(src.targets) != 1 {
			t.Fatalf("blend_emitter: %d targets, want 1", len(src.targets))
		}
		tgt := src.targets[0]
		if tgt.table != "blend_emitter_events" {
			t.Errorf("blend_emitter target table = %q, want blend_emitter_events", tgt.table)
		}
		// The fan-out `drop` kind must NOT be reconciled by count.
		if contains(tgt.kinds, "blend_emitter.drop") {
			t.Errorf("blend_emitter_events reconciles kind %q — the drop fan-out (N recipient rows per event) false-flags every drop ledger; it must be waived, not counted", "blend_emitter.drop")
		}
		// The 1:1 kinds must STILL be reconciled per-ledger (coverage preserved).
		for _, want := range []string{"blend_emitter.distribute", "blend_emitter.swap_config"} {
			if !contains(tgt.kinds, want) {
				t.Errorf("blend_emitter_events must still reconcile the 1:1 kind %q (coverage preserved); kinds=%v", want, tgt.kinds)
			}
		}
		// The served side must exclude the fanned-out drop rows so the count
		// matches the drop-free expected side.
		if tgt.whereFilter != "event_kind <> 'drop'" {
			t.Errorf("blend_emitter_events whereFilter = %q, want \"event_kind <> 'drop'\" (carve the fan-out drop rows out of the served count)", tgt.whereFilter)
		}
	}
	if !found {
		t.Fatal("catalogue missing blend_emitter source")
	}
}

// TestValidateSourceFilter is the F7 fail-open regression: a -source
// filter that names no catalogue source must fail CLOSED, not silently
// skip every source and report success. Empty (all sources) and a real
// name both pass.
func TestValidateSourceFilter(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "sdex"}}
	cases := []struct {
		name    string
		only    string
		wantErr bool
	}{
		{"empty means all sources", "", false},
		{"exact match", "aquarius", false},
		{"first entry", "soroswap", false},
		{"typo fails closed", "aquariuss", true},
		{"unknown fails closed", "not-a-source", true},
		{"case-sensitive (no fuzzy match)", "Soroswap", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceFilter(tc.only, cat)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateSourceFilter(%q) err=%v, wantErr=%v", tc.only, err, tc.wantErr)
			}
			// A rejection must name what the operator can actually pass.
			if err != nil && !strings.Contains(err.Error(), "soroswap") {
				t.Errorf("rejection should list known sources; got %q", err.Error())
			}
		})
	}
}

// TestFilterCatalogueByNetwork pins #483: on a test net the pubnet-anchored
// protocol sources leave the catalogue entirely (their decoders match
// nothing there and their pubnet genesis floors sit above the network's
// tip), while the ledger-anchored ones stay. On pubnet the filter is the
// identity — that is the safety argument for the whole change.
func TestFilterCatalogueByNetwork(t *testing.T) {
	cat := []reconSource{
		{name: "soroswap"},
		{name: "sdex"},
		{name: "blend"},
		{name: "sep41_transfers"},
		{name: "reflector-dex"},
	}

	kept, dropped := filterCatalogueByNetwork(cat, "pubnet")
	if len(kept) != len(cat) || len(dropped) != 0 {
		t.Errorf("pubnet must be the identity: kept=%d dropped=%v", len(kept), dropped)
	}

	kept, dropped = filterCatalogueByNetwork(cat, "testnet")
	var names []string
	for _, s := range kept {
		names = append(names, s.name)
	}
	if len(names) != 2 || names[0] != "sdex" || names[1] != "sep41_transfers" {
		t.Errorf("testnet kept = %v, want [sdex sep41_transfers] in input order", names)
	}
	if len(dropped) != 3 || dropped[0] != "blend" || dropped[1] != "reflector-dex" || dropped[2] != "soroswap" {
		t.Errorf("testnet dropped = %v, want blend, reflector-dex, soroswap (sorted)", dropped)
	}

	// The real catalogue must survive the filter on pubnet with every
	// source intact — a name that sourcenet does not classify would
	// silently vanish here, so this is the cross-check that matters.
	full, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Skipf("catalogue build unavailable in this environment: %v", err)
	}
	keptFull, droppedFull := filterCatalogueByNetwork(full, "pubnet")
	if len(keptFull) != len(full) || len(droppedFull) != 0 {
		t.Errorf("real catalogue on pubnet: kept %d of %d, dropped %v", len(keptFull), len(full), droppedFull)
	}
}
