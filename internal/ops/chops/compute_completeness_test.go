// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// testConfigWithAllSources enables the config-gated catalogue entries
// (oracles + band) so the opt-out audit sees the full source set. The
// addresses are syntactically valid C-strkeys; the catalogue only
// checks non-emptiness.
func testConfigWithAllSources() config.Config {
	cfg := config.Config{}
	cfg.Oracle.Reflector.DEXContract = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	cfg.Oracle.Reflector.CEXContract = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6ZLSDJLGA"
	cfg.Oracle.Reflector.FXContract = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
	cfg.Oracle.Redstone.AdapterContract = "CBCIXRPTFeu6M2Q6ISDIT3QQBAYXC4YIIFCTVKC5FGZALVQAQ2QLDLQ4"
	cfg.Oracle.Band.StandardReferenceContract = "CDEGQ2P4RXDT7BXCOAJB4MDNMSTOTBBHNS7HHRZ7ZKBWHSPQXNSMPPMV"
	return cfg
}

// TestProjectionDelta_PerLedgerCatchesNetting pins the CS-084 fix:
// a real drop in one ledger masked by a phantom overcount in another
// nets to Δ=0 under a totals compare — the strict per-ledger default
// must catch it.
func TestProjectionDelta_PerLedgerCatchesNetting(t *testing.T) {
	expected := map[uint32]int{100: 5, 200: 3, 300: 2}
	actual := map[uint32]int{100: 4, 200: 4, 300: 2} // drop@100 + phantom@200 → totals equal

	src := reconSource{name: "soroswap"} // strict default
	delta, detail := projectionDelta(src, "trades", expected, actual, 100, 300)
	if delta != 2 {
		t.Fatalf("delta = %d, want 2 (|5-4| + |3-4|) — netting must not cancel", delta)
	}
	if !strings.Contains(detail, "2 mismatched ledger(s)") || !strings.Contains(detail, "ledger=100") {
		t.Errorf("detail should name the mismatch count + first ledger, got: %s", detail)
	}
}

// TestProjectionDelta_CleanIsClean — identical maps produce zero
// delta and no detail on both modes.
func TestProjectionDelta_CleanIsClean(t *testing.T) {
	counts := map[uint32]int{100: 5, 200: 3}
	for _, src := range []reconSource{
		{name: "strict"},
		{name: "agg", aggregateReconcile: "test reason"},
	} {
		delta, detail := projectionDelta(src, "trades", counts, map[uint32]int{100: 5, 200: 3}, 100, 200)
		if delta != 0 || detail != "" {
			t.Errorf("%s: clean compare produced delta=%d detail=%q", src.name, delta, detail)
		}
	}
}

// TestProjectionDelta_AggregateModeToleratesShift — an opted-out
// source (oracle keying vintages) compares totals: a count shifted
// across ledgers within the scope is tolerated (the documented
// residual), while a real net loss still fails.
func TestProjectionDelta_AggregateModeToleratesShift(t *testing.T) {
	src := reconSource{name: "reflector-dex", aggregateReconcile: "keying vintages"}

	// Shift: same total, different ledgers — tolerated by design.
	delta, _ := projectionDelta(src, "oracle_updates",
		map[uint32]int{100: 5, 200: 3},
		map[uint32]int{101: 5, 201: 3}, 100, 201)
	if delta != 0 {
		t.Errorf("aggregate mode: pure keying shift should be tolerated, got delta=%d", delta)
	}

	// Real net loss still caught.
	delta, detail := projectionDelta(src, "oracle_updates",
		map[uint32]int{100: 5},
		map[uint32]int{100: 3}, 100, 100)
	if delta != 2 {
		t.Errorf("aggregate mode: net loss delta = %d, want 2", delta)
	}
	if !strings.Contains(detail, "aggregate compare") {
		t.Errorf("detail should mark the aggregate mode, got: %s", detail)
	}

	// Phantoms in unexpected ledgers count too (sumCounts covers all keys).
	delta, _ = projectionDelta(src, "oracle_updates",
		map[uint32]int{100: 5},
		map[uint32]int{100: 5, 999: 2}, 100, 999)
	if delta != 2 {
		t.Errorf("aggregate mode: phantom delta = %d, want 2", delta)
	}
}

// TestReconciliationCatalogue_OracleSourcesOptOut — only the oracle
// sources may carry aggregateReconcile; every other source must stay
// on the strict per-ledger default. Guards against someone quietly
// opting a trade source out of CS-084 strictness.
func TestReconciliationCatalogue_OracleSourcesOptOut(t *testing.T) {
	allowedAggregate := map[string]bool{
		"reflector-dex": true, "reflector-cex": true, "reflector-fx": true,
		"redstone": true,
	}
	cfg := testConfigWithAllSources()
	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	if len(cat) < 10 {
		t.Fatalf("catalogue unexpectedly small (%d) — test config not enabling sources?", len(cat))
	}
	for _, src := range cat {
		if src.aggregateReconcile != "" && !allowedAggregate[src.name] {
			t.Errorf("%s opted out of strict per-ledger reconcile (%q) — only oracle sources with documented keying vintages may", src.name, src.aggregateReconcile)
		}
		if allowedAggregate[src.name] && src.aggregateReconcile == "" {
			t.Errorf("%s should carry aggregateReconcile (documented oracle keying vintages)", src.name)
		}
	}
}

// TestCombineWatermark_LakeDecouplesFromProjection pins the
// ADR-0033/0034 two-axis verdict (decision brief
// notes/DECISION-genesis-complete-verdict-2026-07-16.md, Option B): a
// source whose substrate+recognition watermark reaches tip (srW.Complete
// = lake_complete = true) but whose served-tier projection fails
// (projOK = false) must report complete=false while lake_complete stays
// true — the lake (archive) axis is never gated by the retention-scoped
// projection reconcile. combineWatermark is what compute-completeness's
// CH branch calls to derive `w` (the served/combined axis); lake_complete
// itself is read straight off srW, never off the return of this call.
func TestCombineWatermark_LakeDecouplesFromProjection(t *testing.T) {
	srW := completeness.Watermark{
		Genesis: 61_500_000, Tip: 63_305_532, Ledger: 63_305_532,
		Complete: true, CoveragePct: 1,
	}
	lakeComplete := srW.Complete // exactly what the compute loop does

	// Projection fails (retention-scoped reconcile found a mismatch) —
	// the combined/served axis must go false.
	combined := combineWatermark(srW, false)
	if combined.Complete {
		t.Fatal("combined (served) watermark should be Complete=false when projOK=false")
	}
	if !lakeComplete {
		t.Fatal("lake_complete must stay true — it must never be gated by projection")
	}

	// Projection also holds — combined matches the lake watermark.
	combinedOK := combineWatermark(srW, true)
	if !combinedOK.Complete {
		t.Error("combined watermark should be Complete=true when both srW and projOK hold")
	}
	if combinedOK.Ledger != srW.Ledger || combinedOK.CoveragePct != srW.CoveragePct {
		t.Errorf("combineWatermark must not otherwise mutate the lake watermark's fields: got %+v, want ledger/coverage from %+v", combinedOK, srW)
	}

	// srW itself must be untouched by combineWatermark (no aliasing bug).
	if !srW.Complete {
		t.Error("combineWatermark must not mutate its srW argument")
	}
}

// TestCombineWatermark_LakeIncompleteStaysIncomplete — when the lake
// axis itself has a problem, the combined axis can never be true
// regardless of projOK (AND, not OR).
func TestCombineWatermark_LakeIncompleteStaysIncomplete(t *testing.T) {
	srW := completeness.Watermark{Genesis: 100, Tip: 200, Ledger: 150, Complete: false, FirstProblem: 151}
	if combined := combineWatermark(srW, true); combined.Complete {
		t.Error("combined watermark cannot be Complete=true when the lake watermark itself is incomplete")
	}
}
