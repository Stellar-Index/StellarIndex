// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// testConfigWithAllSources enables the config-gated catalogue entries (oracles
// + band); the catalogue only checks the addresses are non-empty.
func testConfigWithAllSources() config.Config {
	cfg := config.Config{}
	cfg.Oracle.Reflector.DEXContract = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	cfg.Oracle.Reflector.CEXContract = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6ZLSDJLGA"
	cfg.Oracle.Reflector.FXContract = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
	cfg.Oracle.Redstone.AdapterContract = "CBCIXRPTFeu6M2Q6ISDIT3QQBAYXC4YIIFCTVKC5FGZALVQAQ2QLDLQ4"
	cfg.Oracle.Band.StandardReferenceContract = "CDEGQ2P4RXDT7BXCOAJB4MDNMSTOTBBHNS7HHRZ7ZKBWHSPQXNSMPPMV"
	return cfg
}

// A test net (soroban_genesis_ledger = 1) must scan [1, tip], not the inverted
// [pubnet-activation, tip] that reads nothing.
func TestSorobanEraFloor_NetworkAware(t *testing.T) {
	const testnetTip = 4_927_174
	testnet := config.Default()
	testnet.Stellar.SorobanGenesisLedger = 1
	cases := []struct {
		name string
		cfg  config.Config
		want uint32
	}{
		{"testnet", testnet, 1},
		{"pubnet default", config.Default(), 50_457_424},
		{"zero-valued config", config.Config{}, 50_457_424},
	}
	for _, tc := range cases {
		if got := sorobanEraFloor(tc.cfg); got != tc.want {
			t.Errorf("%s: sorobanEraFloor = %d, want %d", tc.name, got, tc.want)
		}
	}
	err := recognitionScanEmptyErr(0, sorobanEraFloor(testnet), testnetTip)
	if err == nil || !strings.Contains(err.Error(), "[1, 4927174]") {
		t.Fatalf("empty test-net scan: err = %v, want a fail-closed error naming [1, 4927174]", err)
	}
}

// A scan ERROR must fail the run closed: an empty gap slice would read as
// recognition_ok=true and publish a false complete. -skip-recognition is a
// deliberate trust flag, not a swallowed error; a real gap passes through.
func TestRunRecognitionScan(t *testing.T) {
	scanErr := errors.New("clickhouse: memory limit (for query) exceeded in DistinctTopicShapes")
	gaps, err := runRecognitionScan(false, func() ([]completeness.RecognitionGap, error) {
		return nil, scanErr
	})
	if err == nil {
		t.Fatal("recognition scan error was swallowed; want a returned error that fails the run closed")
	}
	if !errors.Is(err, scanErr) {
		t.Errorf("returned error must wrap the underlying scan error, got: %v", err)
	}
	if gaps != nil {
		t.Errorf("a failed scan must yield NO gaps, got: %v", gaps)
	}

	scanned := false
	gaps, err = runRecognitionScan(true, func() ([]completeness.RecognitionGap, error) {
		scanned = true
		return nil, errors.New("scan must not run under -skip-recognition")
	})
	if scanned || err != nil || gaps != nil {
		t.Errorf("-skip-recognition: scanned=%v err=%v gaps=%v, want no scan, no error, no gaps", scanned, err, gaps)
	}

	want := []completeness.RecognitionGap{{ContractID: "CBID", Topic0Sym: "transfer", MinLedger: 51_000_000}}
	gaps, err = runRecognitionScan(false, func() ([]completeness.RecognitionGap, error) { return want, nil })
	if err != nil {
		t.Fatalf("a successful scan must not error, got: %v", err)
	}
	if len(gaps) != 1 || gaps[0].MinLedger != 51_000_000 {
		t.Errorf("a real recognition gap must pass through unchanged, got: %v", gaps)
	}
}

// Excluding the full ClassicTokenTopic0Syms hides the Blend/Comet set_admin
// collision and makes a watched SEP-41 source's own event kinds unreachable.
func TestRecognitionGlobalExcludeSyms_ExcludesFirehoseNotClassicToken(t *testing.T) {
	if !reflect.DeepEqual(recognitionGlobalExcludeSyms, clickhouse.FirehoseExcludeSyms) {
		t.Fatalf("global recognition census must exclude clickhouse.FirehoseExcludeSyms, got %v", recognitionGlobalExcludeSyms)
	}
	if reflect.DeepEqual(recognitionGlobalExcludeSyms, clickhouse.ClassicTokenTopic0Syms) {
		t.Fatal("global recognition census must NOT exclude the full ClassicTokenTopic0Syms")
	}
}

func TestWatchedSep41RecognitionShapes_EmptyWatchListScansNothing(t *testing.T) {
	shapes, err := watchedSep41RecognitionShapes(context.Background(), config.Config{}, "unreachable:0", 0, 100)
	if err != nil {
		t.Fatalf("empty watch list must short-circuit without dialing ClickHouse, got err: %v", err)
	}
	if shapes != nil {
		t.Errorf("empty watch list must scan nothing, got %v", shapes)
	}
}

// Strict per-ledger compare catches a drop netted by a phantom; an aggregate
// waiver tolerates a keying shift only below its vintage boundary, and a waiver
// without a boundary reconciles strict.
func TestProjectionDelta(t *testing.T) {
	strict := reconSource{name: "soroswap"}
	agg := reconSource{name: "reflector-dex", aggregate: &aggregateWaiver{reason: "keying vintages", boundary: 1000}}
	split := reconSource{name: "phoenix", aggregate: &aggregateWaiver{reason: "sweep shift", boundary: 200}}
	whole := reconSource{name: "phoenix-whole", aggregate: &aggregateWaiver{reason: "sweep shift", boundary: 300}}
	noBoundary := reconSource{name: "reflector-dex", aggregate: &aggregateWaiver{reason: "keying vintages"}}
	m := func(kv ...uint32) map[uint32]int {
		out := map[uint32]int{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = int(kv[i+1])
		}
		return out
	}
	cases := []struct {
		name             string
		src              reconSource
		expected, actual map[uint32]int
		lo, hi           uint32
		wantDelta        int
		wantNoDetail     bool
		wantIn           []string
	}{
		{"strict catches netting", strict, m(100, 5, 200, 3, 300, 2), m(100, 4, 200, 4, 300, 2), 100, 300, 2, false, []string{"2 mismatched ledger(s)", "ledger=100"}},
		{"strict clean", reconSource{name: "strict"}, m(100, 5, 200, 3), m(100, 5, 200, 3), 100, 200, 0, true, nil},
		{"aggregate clean", reconSource{name: "agg", aggregate: &aggregateWaiver{reason: "test reason", boundary: 1000}}, m(100, 5, 200, 3), m(100, 5, 200, 3), 100, 200, 0, true, nil},
		{"aggregate tolerates keying shift", agg, m(100, 5, 200, 3), m(101, 5, 201, 3), 100, 201, 0, false, nil},
		{"aggregate catches net loss", agg, m(100, 5), m(100, 3), 100, 100, 2, false, []string{"aggregate compare"}},
		{"aggregate counts phantoms", agg, m(100, 5), m(100, 5, 999, 2), 100, 999, 2, false, nil},
		{"all-pre-boundary nets the hole", whole, m(100, 5, 300, 5), m(100, 7, 300, 3), 100, 300, 0, false, nil},
		{"vintage split surfaces the netted gap", split, m(100, 5, 300, 5), m(100, 7, 300, 3), 100, 300, 4, false, []string{"post-vintage strict", "300"}},
		{"pre-boundary keying shift still nets", split, m(100, 5), m(150, 5), 100, 200, 0, false, nil},
		{"post-boundary drop is strict", split, m(300, 5, 400, 5), m(300, 5, 400, 3), 300, 400, 2, false, nil},
		{"waiver without boundary is strict", noBoundary, m(63_001_900, 10, 63_004_102, 10), m(63_001_900, 13, 63_004_102, 7), 63_000_000, 63_017_280, 6, false, []string{"without boundary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, detail := projectionDelta(tc.src, "trades", tc.expected, tc.actual, tc.lo, tc.hi)
			if delta != tc.wantDelta {
				t.Fatalf("delta = %d, want %d (detail %q)", delta, tc.wantDelta, detail)
			}
			if tc.wantNoDetail && detail != "" {
				t.Errorf("clean compare produced detail %q", detail)
			}
			for _, w := range tc.wantIn {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q missing %q", detail, w)
				}
			}
		})
	}
}

// No source may opt out of strict per-ledger reconcile without a documented
// ledger-keying reason, and every opt-out must carry its vintage boundary.
func TestReconciliationCatalogue_AggregateWaiversAreBounded(t *testing.T) {
	allowedAggregate := map[string]bool{}
	cat, _, err := buildReconciliationCatalogue(testConfigWithAllSources())
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	if len(cat) < 10 {
		t.Fatalf("catalogue unexpectedly small (%d) — test config not enabling sources?", len(cat))
	}
	for _, src := range cat {
		w := src.aggregate
		if w == nil {
			continue
		}
		if !allowedAggregate[src.name] {
			t.Errorf("%s opted out of strict per-ledger reconcile (%q) — only allow-listed sources may", src.name, w.reason)
		}
		if w.reason == "" || w.boundary == 0 {
			t.Errorf("%s: aggregate waiver needs a reason and a boundary > 0 (got reason=%q boundary=%d)", src.name, w.reason, w.boundary)
		}
	}
}

// lake_complete is read straight off srW and is never gated by projection;
// the combined (served) axis is the AND of both.
func TestCombineWatermark(t *testing.T) {
	srW := completeness.Watermark{
		Genesis: 61_500_000, Tip: 63_305_532, Ledger: 63_305_532,
		Complete: true, CoveragePct: 1,
	}
	lakeComplete := srW.Complete

	if combineWatermark(srW, false).Complete {
		t.Fatal("combined (served) watermark should be Complete=false when projOK=false")
	}
	if !lakeComplete {
		t.Fatal("lake_complete must stay true — it must never be gated by projection")
	}
	combinedOK := combineWatermark(srW, true)
	if !combinedOK.Complete {
		t.Error("combined watermark should be Complete=true when both srW and projOK hold")
	}
	if combinedOK.Ledger != srW.Ledger || combinedOK.CoveragePct != srW.CoveragePct {
		t.Errorf("combineWatermark must not otherwise mutate the lake watermark's fields: got %+v, want ledger/coverage from %+v", combinedOK, srW)
	}
	if !srW.Complete {
		t.Error("combineWatermark must not mutate its srW argument")
	}

	lakeBad := completeness.Watermark{Genesis: 100, Tip: 200, Ledger: 150, Complete: false, FirstProblem: 151}
	if combineWatermark(lakeBad, true).Complete {
		t.Error("combined watermark cannot be Complete=true when the lake watermark itself is incomplete")
	}
}

// combineWatermark leaves FirstProblem at 0 for a failed CH
// reconcile, so the find itself must be reported for the verdict write.
func TestProjectionFoundProblem(t *testing.T) {
	cases := []struct {
		name      string
		delta     int
		blind     completeness.BlindSpots
		floorLoss []string
		want      bool
	}{
		{"clean", 0, completeness.BlindSpots{}, nil, false},
		{"nonzero delta", -3, completeness.BlindSpots{}, nil, true},
		{"blind spots", 0, completeness.BlindSpots{Ledgers: []uint32{62_000_001}, UndecodableMatched: 1}, nil, true},
		{"floor loss", 0, completeness.BlindSpots{}, []string{"projection: served floor rose"}, true},
	}
	for _, tc := range cases {
		if got := projectionFoundProblem(tc.delta, tc.blind, tc.floorLoss); got != tc.want {
			t.Errorf("%s: projectionFoundProblem = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The projection floor comes from the served tier's own MIN(ledger), never
// from `tip - 1_500_000`: trades has no retention policy, so a loss older than
// that window would otherwise sit permanently outside the reconcile scope.
func TestTargetScope_DataDerivedFloorSeesLossBelowTheOldRetentionWindow(t *testing.T) {
	const (
		genesis   = uint32(50_746_266)
		tip       = uint32(63_305_532)
		servedMin = uint32(61_500_000)
		hole      = uint32(61_600_000)
	)
	oldFloor := tip - 1_500_000
	if hole >= oldFloor || hole < servedMin {
		t.Fatalf("fixture invalid: the hole (%d) must sit BELOW the old floor (%d) and inside the served range (>= %d)", hole, oldFloor, servedMin)
	}

	scope := targetScope(servedMin, true, genesis, 0, tip)
	if scope.From != servedMin || scope.To != tip {
		t.Fatalf("scope = [%d,%d], want [%d,%d] (the served tier's own MIN(ledger) to tip)", scope.From, scope.To, servedMin, tip)
	}

	expected := map[uint32]int{hole: 5, 62_000_000: 3, 63_000_000: 1}
	served := map[uint32]int{hole: 3, 62_000_000: 3, 63_000_000: 1}
	src := reconSource{name: "soroswap"}
	delta, detail := projectionDelta(src, "trades",
		clipCounts(expected, scope), clipCounts(served, scope), scope.From, scope.To)
	if delta != 2 {
		t.Fatalf("Σ|Δ| = %d, want 2 (|5-3| at ledger %d); detail=%q", delta, hole, detail)
	}
	if !strings.Contains(detail, "ledger=61600000") {
		t.Errorf("detail must localize the loss to ledger %d, got: %s", hole, detail)
	}

	oldScope := projectionScope{From: oldFloor, To: tip}
	if d, _ := projectionDelta(src, "trades",
		clipCounts(expected, oldScope), clipCounts(served, oldScope), oldScope.From, oldScope.To); d != 0 {
		t.Fatalf("fixture invalid: the hole must be INVISIBLE under a tip-1.5M floor, got Δ=%d", d)
	}
}

// Each target is scoped by its OWN served floor; an empty target scopes from
// genesis (fail closed); -from may only RAISE the floor.
func TestTargetScope_From(t *testing.T) {
	const tip = uint32(63_305_532)
	cases := []struct {
		name                    string
		servedMin               uint32
		has                     bool
		genesis, from, wantFrom uint32
	}{
		{"trades never backfilled below 61.5M", 61_500_000, true, 50_746_266, 0, 61_500_000},
		{"full-history sibling keeps its genesis", 50_746_266, true, 50_746_266, 0, 50_746_266},
		{"empty target fails closed at genesis", 0, false, 51_499_546, 0, 51_499_546},
		{"-from above the served floor raises it", 61_500_000, true, 50_746_266, 63_300_000, 63_300_000},
		{"-from below the served floor cannot lower it", 61_500_000, true, 50_746_266, 51_000_000, 61_500_000},
	}
	for _, tc := range cases {
		if got := targetScope(tc.servedMin, tc.has, tc.genesis, tc.from, tip).From; got != tc.wantFrom {
			t.Errorf("%s: floor = %d, want %d", tc.name, got, tc.wantFrom)
		}
	}

	sc := targetScope(0, false, 51_499_546, 0, tip)
	if delta, _ := projectionDelta(reconSource{name: "comet"}, "comet_liquidity",
		clipCounts(map[uint32]int{52_000_000: 4}, sc), map[uint32]int{}, sc.From, sc.To); delta != 4 {
		t.Errorf("a wiped served table must reconcile as a 4-row loss, got Δ=%d", delta)
	}
}

// A partial run may CONFIRM or DOWNGRADE the served axis, never UPGRADE it:
// -from = min(watermark) sits at tip whenever the lake is clean, so it never
// re-sees the mismatch that pinned complete=false.
func TestProjectionClaim_IncrementalRunCannotUpgradeAFailingVerdict(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		hi         = uint32(63_305_532)
		runFrom    = uint32(63_300_000)
	)
	failingPrior := priorProjection{known: true, ok: false, tip: 63_300_000}

	ok, detail := projectionClaim(servedFrom, runFrom, hi, true, "", failingPrior, testScope)
	if ok {
		t.Fatalf("an incremental run that reconciled only [%d,%d] UPGRADED a failing verdict without re-checking [%d,%d]; detail=%q", runFrom, hi, servedFrom, runFrom-1, detail)
	}
	if !strings.Contains(detail, "61500000") || !strings.Contains(detail, "63299999") {
		t.Errorf("detail must name the range that was NOT reconciled, got: %s", detail)
	}

	if full, d := projectionClaim(servedFrom, servedFrom, hi, true, "", failingPrior, testScope); !full {
		t.Errorf("a full-scope clean run must be able to clear a failing prior verdict, got false: %s", d)
	}

	cleanPrior := priorProjection{known: true, ok: true, tip: 63_300_000}
	carry, carryDetail := projectionClaim(servedFrom, runFrom, hi, true, "", cleanPrior, testScope)
	if !carry {
		t.Errorf("a contiguous clean prior must carry the skipped prefix, got false: %s", carryDetail)
	}
	if !strings.Contains(carryDetail, "carried from the prior clean verdict") {
		t.Errorf("a carried claim must say so, got: %s", carryDetail)
	}

	if noPrior, d := projectionClaim(servedFrom, runFrom, hi, true, "", priorProjection{}, testScope); noPrior {
		t.Errorf("a partial run with NO prior verdict must not claim the skipped prefix: %s", d)
	}
	if stale, d := projectionClaim(servedFrom, runFrom, hi, true, "", priorProjection{known: true, ok: true, tip: 62_000_000}, testScope); stale {
		t.Errorf("a stale prior leaves an unverified band — must not claim it: %s", d)
	}
	if found, d := projectionClaim(servedFrom, servedFrom, hi, false, "trades: 1 mismatched ledger(s)", priorProjection{known: true, ok: true, tip: hi}, testScope); found {
		t.Errorf("a mismatch found by this run can never be laundered by a clean prior: %s", d)
	}
}

// A prior clean projection verdict carries only as far as its Watermark (the
// range it reconciled), never to Tip, which sits above a recognition gap.
func TestBuildPriorVerdicts_ProjectionCarryBoundsToWatermarkNotTip(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		watermark  = uint32(62_000_000)
		tip        = uint32(62_500_000)
	)
	snaps := []timescale.CompletenessSnapshot{
		{Source: "soroswap", ProjectionOK: true, SubstrateOK: true, RecognitionOK: true, Tip: tip, Watermark: watermark},
	}
	priorProj, _, _, _ := buildPriorVerdicts(snaps)

	prior := priorProj["soroswap"]
	if prior.tip != watermark {
		t.Fatalf("priorProj[soroswap].tip = %d, want %d (Watermark, not Tip=%d)", prior.tip, watermark, tip)
	}
	ok, detail := projectionClaim(servedFrom, tip, tip, true, "", prior, testScope)
	if ok {
		t.Fatalf("projectionClaim carried a prior verdict over [%d,%d], a band the prior run never reconciled", watermark+1, tip-1)
	}
	if !strings.Contains(detail, fmt.Sprintf("%d", watermark)) {
		t.Errorf("rejection detail must name the prior verdict's true reach (watermark=%d), got: %s", watermark, detail)
	}
}

// The expected census spans the union of a source's target scopes, so each
// target compares only the ledgers inside its own scope.
func TestClipCounts_BoundsTheExpectedSideToTheTargetScope(t *testing.T) {
	m := map[uint32]int{50_800_000: 7, 61_500_000: 2, 62_000_000: 1, 64_000_000: 9}
	got := clipCounts(m, projectionScope{From: 61_500_000, To: 63_305_532})
	want := map[uint32]int{61_500_000: 2, 62_000_000: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clipCounts = %v, want %v", got, want)
	}
	if len(m) != 4 {
		t.Error("clipCounts must not mutate its input")
	}
}

// A lake COVERAGE failure (problem = tip) must fail a high-genesis source,
// else an empty lake would certify substrate-OK for every source above ledger 2.
func TestSourceSubstrateOK(t *testing.T) {
	const soroswap = uint32(50_746_266)
	const tip = uint32(63_000_000)
	cases := []struct {
		name       string
		problem    uint32
		hasProblem bool
		genesis    uint32
		want       bool
	}{
		{"no problem is OK", 0, false, soroswap, true},
		{"interior break below genesis is OK (before source data)", 30_000_000, true, soroswap, true},
		{"interior break at/after genesis fails", 55_000_000, true, soroswap, false},
		{"empty lake (problem=tip) fails soroswap", tip, true, soroswap, false},
		{"empty lake (problem=tip) fails SDEX too", tip, true, 2, false},
		{"missing head (problem=haveMin-1) fails source starting in the head", 49_999_999, true, 40_000_000, false},
		{"missing head does NOT fail a source whose data is fully present", 49_999_999, true, soroswap, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceSubstrateOK(tc.problem, tc.hasProblem, tc.genesis); got != tc.want {
				t.Fatalf("sourceSubstrateOK(problem=%d, has=%v, genesis=%d) = %v, want %v",
					tc.problem, tc.hasProblem, tc.genesis, got, tc.want)
			}
		})
	}
}

// fakeSubstrateLake mirrors clickhouse.SubstrateProblem: a head guard that
// returns early when `from` is below the lake floor, else the first interior
// hole in [from, to]. calls, if non-nil, records every (from,to).
func fakeSubstrateLake(haveMin uint32, calls *[]struct{ from, to uint32 }, interiorHoles ...uint32) substrateScanner {
	return func(_ context.Context, from, to uint32) (uint32, bool, string, error) {
		if calls != nil {
			*calls = append(*calls, struct{ from, to uint32 }{from, to})
		}
		if from < haveMin {
			return haveMin - 1, true, fmt.Sprintf("head truncated — first present is %d", haveMin), nil
		}
		for _, h := range interiorHoles {
			if h >= from && h <= to {
				return h, true, fmt.Sprintf("interior hole at %d", h), nil
			}
		}
		return 0, false, "", nil
	}
}

// A scan at the run's global floor returns only the lowest problem (a hole or
// a head truncation below the source's genesis), which reads as clean for a
// high-genesis source and masks its own hole. Scoping to the source's genesis
// must find it.
func TestSubstrateForGenesis_ScopedScanFindsTheSourcesOwnHole(t *testing.T) {
	const tip, floor = uint32(70_000_000), uint32(2)
	cases := []struct {
		name     string
		haveMin  uint32
		holes    []uint32
		genesis  uint32
		wantHole uint32
	}{
		{"lower hole must not mask", 2, []uint32{51_000_000, 62_500_000}, 55_000_000, 62_500_000},
		{"global head truncation must not skip the walk", 40_000_000, []uint32{55_000_000}, 50_746_266, 55_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan := fakeSubstrateLake(tc.haveMin, nil, tc.holes...)
			got, err := substrateForGenesis(context.Background(), scan, make(map[uint32]substrateScan), tc.genesis, floor, tip)
			if err != nil {
				t.Fatalf("substrateForGenesis: %v", err)
			}
			if !got.has || got.problem != tc.wantHole {
				t.Fatalf("substrateForGenesis(genesis=%d) = (problem=%d,has=%v), want (problem=%d,has=true)",
					tc.genesis, got.problem, got.has, tc.wantHole)
			}
			if sourceSubstrateOK(got.problem, got.has, tc.genesis) {
				t.Fatalf("sourceSubstrateOK reported clean for genesis=%d despite its own hole at %d", tc.genesis, tc.wantHole)
			}
		})
	}
}

// Two sources sharing a scan floor query the lake once; floor > tip
// (-skip-substrate) queries nothing.
func TestSubstrateForGenesis_MemoizesByScanFloorAndSkipsWhenFloorExceedsTip(t *testing.T) {
	var calls []struct{ from, to uint32 }
	scan := fakeSubstrateLake(2, &calls)
	cache := make(map[uint32]substrateScan)

	for i := 0; i < 2; i++ {
		if _, err := substrateForGenesis(context.Background(), scan, cache, 2, 2, 100); err != nil {
			t.Fatalf("substrateForGenesis: %v", err)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("substrateForGenesis queried the lake %d times for the same floor, want 1 (memoised)", len(calls))
	}

	if res, err := substrateForGenesis(context.Background(), scan, cache, 2, 101, 100); err != nil || res.has {
		t.Fatalf("substrateForGenesis(floor>tip) = (%+v,%v), want zero value with no query", res, err)
	}
	if len(calls) != 1 {
		t.Fatalf("substrateForGenesis queried the lake on a floor>tip (-skip-substrate) run; got %d total calls", len(calls))
	}
}

// targetScope floors each target at its own MIN(ledger), so a bottom-edge
// deletion raises the floor with the loss and reconciles clean; only the
// durable recorded floor can tell "dropped" from "never projected".
func TestDetectFloorLoss(t *testing.T) {
	src := reconSource{
		name: "soroswap",
		targets: []reconTarget{
			{table: "trades", whereFilter: "source = 'soroswap'"},
			{table: "soroswap_skim_events"},
		},
	}
	keyTrades := timescale.TargetFloorKey("soroswap", "trades", "source = 'soroswap'")
	keySkim := timescale.TargetFloorKey("soroswap", "soroswap_skim_events", "")

	floors := func(m map[string]uint32) map[string]timescale.CompletenessTargetFloor {
		out := make(map[string]timescale.CompletenessTargetFloor, len(m))
		for k, v := range m {
			out[k] = timescale.CompletenessTargetFloor{VerifiedFrom: v}
		}
		return out
	}

	tests := []struct {
		name      string
		served    []servedFloor
		floors    map[string]timescale.CompletenessTargetFloor
		wantCount int
		wantIn    string
	}{
		{
			// An absent floor is "nothing to compare against", never floor=0.
			name:      "no recorded floor is not loss",
			served:    []servedFloor{{min: 61_500_000, present: true}, {min: 61_500_000, present: true}},
			floors:    floors(nil),
			wantCount: 0,
		},
		{
			name:      "served min equal to the floor is not loss",
			served:    []servedFloor{{min: 61_500_000, present: true}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 0,
		},
		{
			name:      "served min BELOW the floor is not loss",
			served:    []servedFloor{{min: 100, present: true}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 0,
		},
		{
			name:      "served min ABOVE the floor is loss",
			served:    []servedFloor{{min: 71_000_000, present: true}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 1,
			wantIn:    "9500000 ledgers of served rows below the recorded floor are GONE",
		},
		{
			name:      "empty target with a prior floor is loss",
			served:    []servedFloor{{min: 0, present: false}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 1,
			wantIn:    "holds NO rows but was previously verified from ledger 61500000",
		},
		{
			name:      "empty target with no floor is not loss",
			served:    []servedFloor{{min: 0, present: false}, {min: 2, present: true}},
			floors:    floors(nil),
			wantCount: 0,
		},
		{
			name:      "both targets can fail independently",
			served:    []servedFloor{{min: 71_000_000, present: true}, {min: 500, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectFloorLoss(src, tc.served, tc.floors)
			if len(got) != tc.wantCount {
				t.Fatalf("detectFloorLoss returned %d finding(s), want %d: %v", len(got), tc.wantCount, got)
			}
			if tc.wantIn != "" {
				joined := strings.Join(got, " | ")
				if !strings.Contains(joined, tc.wantIn) {
					t.Errorf("detail %q does not contain %q", joined, tc.wantIn)
				}
			}
		})
	}
}

// An empty target's clean reconcile proves 0 == 0, not that rows were present
// from genesis; banking that floor would make its first-ever row read as
// permanent loss (LEAST() can never raise it back).
func TestFloorsToRecord_EmptyTargetEarnsNoFloor(t *testing.T) {
	const (
		genesis  = uint32(50_746_266)
		tip1     = uint32(63_000_000)
		firstRow = uint32(62_000_000)
		tip2     = uint32(63_300_000)
	)
	src := reconSource{
		name:    "phoenix",
		genesis: genesis,
		targets: []reconTarget{
			{table: "trades", whereFilter: "source = 'phoenix'"},
			{table: "phoenix_stake_events"},
		},
	}

	run1Served := []servedFloor{
		{min: 61_500_000, present: true},
		{min: 0, present: false},
	}
	run1Scopes := []projectionScope{
		targetScope(run1Served[0].min, true, genesis, 0, tip1),
		targetScope(0, false, genesis, 0, tip1),
	}
	if run1Scopes[1].From != genesis {
		t.Fatalf("fixture invalid: an empty target must scope from genesis (fail closed), got %d", run1Scopes[1].From)
	}

	recorded := floorsToRecord(src, run1Scopes, run1Served)
	for _, f := range recorded {
		if f.Table == "phoenix_stake_events" {
			t.Fatalf("floorsToRecord banked verified_from=%d for the EMPTY target %s", f.VerifiedFrom, f.Table)
		}
	}
	if len(recorded) != 1 || recorded[0].Table != "trades" || recorded[0].VerifiedFrom != 61_500_000 {
		t.Fatalf("floorsToRecord = %+v, want exactly the non-empty target trades@61500000", recorded)
	}

	floors := make(map[string]timescale.CompletenessTargetFloor, len(recorded))
	for _, f := range recorded {
		floors[timescale.TargetFloorKey(f.Source, f.Table, f.Filter)] = f
	}
	run2Served := []servedFloor{
		{min: 61_500_000, present: true},
		{min: firstRow, present: true},
	}
	if loss := detectFloorLoss(src, run2Served, floors); len(loss) != 0 {
		t.Fatalf("a target's FIRST-EVER row was reported as served-tier loss: %v", loss)
	}

	// Once the table HAS a floor, losing rows below it still fails.
	run2Scopes := []projectionScope{
		targetScope(run2Served[0].min, true, genesis, 0, tip2),
		targetScope(run2Served[1].min, true, genesis, 0, tip2),
	}
	for _, f := range floorsToRecord(src, run2Scopes, run2Served) {
		floors[timescale.TargetFloorKey(f.Source, f.Table, f.Filter)] = f
	}
	truncated := []servedFloor{
		{min: 61_500_000, present: true},
		{min: firstRow + 1_000_000, present: true},
	}
	loss := detectFloorLoss(src, truncated, floors)
	if len(loss) != 1 || !strings.Contains(loss[0], "1000000 ledgers of served rows") {
		t.Fatalf("real bottom-edge loss below an established floor must still fail, got: %v", loss)
	}
}

// A scope clipped above the true served minimum has no evidence below the
// clip; banking it would disarm detectFloorLoss over that gap.
func TestFloorsToRecord_IncrementalScopeCannotBankAFloorAboveTheServedMin(t *testing.T) {
	const (
		genesis    = uint32(50_746_266)
		trueMin    = uint32(55_000_000)
		clippedLow = uint32(62_000_000)
		tip        = uint32(63_000_000)
	)
	src := reconSource{
		name:    "phoenix",
		genesis: genesis,
		targets: []reconTarget{{table: "trades", whereFilter: "source = 'phoenix'"}},
	}
	served := []servedFloor{{min: trueMin, present: true}}
	scopes := []projectionScope{targetScope(trueMin, true, genesis, clippedLow, tip)}
	if scopes[0].From != clippedLow {
		t.Fatalf("fixture invalid: expected the incremental floor to clip the scope to %d, got %d", clippedLow, scopes[0].From)
	}
	if recorded := floorsToRecord(src, scopes, served); len(recorded) != 0 {
		t.Fatalf("floorsToRecord = %+v, want no floor recorded for a clipped incremental scope (true served min %d)", recorded, trueMin)
	}
}

// The lake-axis twin of the projection upgrade guard: a scan bounded by -from
// may not publish substrate_ok over [genesis, tip].
func TestSubstrateClaim_IncrementalRunCannotUpgradeAFailingLakeVerdict(t *testing.T) {
	const (
		genesis  = uint32(50_457_424)
		hi       = uint32(63_305_532)
		scanFrom = uint32(63_300_000)
	)
	failingPrior := priorProjection{known: true, ok: false, tip: 63_300_000}

	ok, detail := substrateClaim(genesis, hi, scanFrom, true, 0, failingPrior)
	if ok {
		t.Fatalf("an incremental run that scanned only [%d,%d] UPGRADED a failing lake verdict without re-scanning [%d,%d]; detail=%q",
			scanFrom, hi, genesis, scanFrom-1, detail)
	}
	if !strings.Contains(detail, "50457424") || !strings.Contains(detail, "63299999") {
		t.Errorf("detail must name the range that was NOT scanned, got: %s", detail)
	}

	full, fullDetail := substrateClaim(genesis, hi, 2, true, 0, failingPrior)
	if !full {
		t.Errorf("a full-range clean scan must be able to clear a failing prior verdict, got false: %s", fullDetail)
	}
	if !strings.Contains(fullDetail, "from this source's genesis") {
		t.Errorf("a full claim must say it reached genesis, got: %s", fullDetail)
	}

	cleanPrior := priorProjection{known: true, ok: true, tip: 63_300_000}
	carry, carryDetail := substrateClaim(genesis, hi, scanFrom, true, 0, cleanPrior)
	if !carry {
		t.Errorf("a contiguous clean prior must carry the skipped prefix, got false: %s", carryDetail)
	}
	if !strings.Contains(carryDetail, "carried from the prior clean verdict") {
		t.Errorf("a carried claim must say so — that string IS the published verified-floor disclosure, got: %s", carryDetail)
	}

	if noPrior, d := substrateClaim(genesis, hi, scanFrom, true, 0, priorProjection{}); noPrior {
		t.Errorf("a partial scan with NO prior verdict must not claim the skipped prefix: %s", d)
	}
	if stale, d := substrateClaim(genesis, hi, scanFrom, true, 0, priorProjection{known: true, ok: true, tip: 62_000_000}); stale {
		t.Errorf("a stale prior leaves an unverified band — must not claim it: %s", d)
	}
	found, foundDetail := substrateClaim(genesis, hi, 2, false, 61_234_567, priorProjection{known: true, ok: true, tip: hi})
	if found {
		t.Errorf("a lake gap found by this run can never be laundered by a clean prior: %s", foundDetail)
	}
	if !strings.Contains(foundDetail, "61234567") {
		t.Errorf("detail must name the problem ledger, got: %s", foundDetail)
	}
}

// -skip-substrate scans nothing (encoded as scanFrom > hi), so it carries the
// prior verdict instead of asserting one.
func TestSubstrateClaim_SkipSubstrateCarriesRatherThanAsserts(t *testing.T) {
	const (
		genesis = uint32(50_457_424)
		hi      = uint32(63_305_532)
	)
	noScan := hi + 1

	if ok, d := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{known: true, ok: false, tip: hi}); ok {
		t.Errorf("-skip-substrate must not upgrade a FAILING prior verdict: %s", d)
	}
	if ok, d := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{}); ok {
		t.Errorf("-skip-substrate with no prior verdict must not claim anything: %s", d)
	}
	ok, detail := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{known: true, ok: true, tip: hi})
	if !ok {
		t.Errorf("-skip-substrate must still confirm a prior clean verdict that reached this tip: %s", detail)
	}
	if !strings.Contains(detail, "-skip-substrate") {
		t.Errorf("the detail must disclose that nothing was scanned, got: %s", detail)
	}
	if ok, d := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{known: true, ok: true, tip: hi - 10_000}); ok {
		t.Errorf("-skip-substrate must not extend a prior verdict past the tip it reached: %s", d)
	}
}

// recOK is true both for a clean scan and for -skip-recognition; the detail
// must tell them apart.
func TestRecognitionClaim_SkipRecognitionIsLabeledCarriedNotProven(t *testing.T) {
	skipped := recognitionClaim(true, true)
	if !strings.Contains(skipped, "-skip-recognition") || !strings.Contains(skipped, "carried") {
		t.Errorf("recognitionClaim(true, skip=true) must disclose that nothing was scanned, got: %q", skipped)
	}
	proven := recognitionClaim(true, false)
	if strings.Contains(proven, "-skip-recognition") || strings.Contains(proven, "carried") {
		t.Errorf("recognitionClaim(true, skip=false) must not claim a carry it didn't make, got: %q", proven)
	}
	if proven == skipped {
		t.Fatal("a genuinely clean scan and a skipped scan must not render the identical detail")
	}
	if got := recognitionClaim(false, false); !strings.Contains(got, "unhandled topic") {
		t.Errorf("recognitionClaim(false, false) = %q, want the unhandled-topic detail", got)
	}
}

// The numeric coverage_pct / watermark must track substrate_ok: a clean
// suffix over an unproven prefix leaves `problems` empty, so the unproven
// floor is injected.
func TestLakeCoverageProblem_UnprovenSubstratePinsNumericWatermark(t *testing.T) {
	const (
		genesis = uint32(50_457_424)
		tip     = uint32(63_305_532)
	)
	failingPrior := priorProjection{known: true, ok: false, tip: 63_300_000}
	p := lakeCoverageProblem(genesis, true, false, failingPrior)
	if p == 0 {
		t.Fatal("a clean suffix over an unproven prefix (failing prior) injected NO coverage problem")
	}
	if p < genesis || p > tip {
		t.Fatalf("injected problem %d must lie in [genesis,tip] [%d,%d], else ComputeWatermark ignores it", p, genesis, tip)
	}

	w := completeness.ComputeWatermark(genesis, tip, []uint32{p})
	if w.CoveragePct >= 1.0 {
		t.Fatalf("coverage_pct = %v; a false substrate claim must publish < 1.0", w.CoveragePct)
	}
	if w.Ledger == tip {
		t.Fatalf("watermark_ledger = tip (%d); a false substrate claim must not read 'verified to tip'", tip)
	}
	if w.Complete {
		t.Fatal("a false substrate claim must not publish Complete=true")
	}
	if w.FirstProblem == 0 {
		t.Fatal("a false substrate claim must publish a first_problem ledger, not leave it absent")
	}

	stalePrior := priorProjection{known: true, ok: true, tip: 62_000_000}
	cases := []struct {
		name             string
		scanClean, subOK bool
		prior            priorProjection
		want             uint32
	}{
		{"no prior: unproven from genesis", true, false, priorProjection{}, genesis},
		{"stale clean prior: band opens at prior.tip+1", true, false, stalePrior, stalePrior.tip + 1},
		{"proven claim injects nothing", true, true, priorProjection{known: true, ok: true, tip: tip}, 0},
		{"raw-scan problem is fed separately", false, false, failingPrior, 0},
	}
	for _, tc := range cases {
		if got := lakeCoverageProblem(genesis, tc.scanClean, tc.subOK, tc.prior); got != tc.want {
			t.Errorf("%s: lakeCoverageProblem = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A projector-replay that rewinds below a source's watermark rewrites rows a
// carried claim still certifies; the pending dirty window must force the
// reconcile floor down over the rewound range, regardless of -from.
func TestDirtyReconcileFloor_ReplayRewindForcesTheRangeBackIntoScope(t *testing.T) {
	const (
		genesis   = uint32(62_146_641)
		tip       = uint32(63_600_000)
		watermark = uint32(63_550_000)
	)
	dirty := timescale.ProjectionDirtyWindow{Source: "cctp", From: 62_270_000, To: 63_550_000}

	preFix := targetScope(genesis, true, genesis, watermark, tip)
	if preFix.From != watermark {
		t.Fatalf("fixture invalid: without the dirty window the scope must start at -from %d, got %d", watermark, preFix.From)
	}

	floor := dirtyReconcileFloor(watermark, genesis, dirty)
	if floor != dirty.From {
		t.Fatalf("dirtyReconcileFloor = %d, want the window bottom %d", floor, dirty.From)
	}
	scope := targetScope(genesis, true, genesis, floor, tip)
	if scope.From > dirty.From || scope.To < dirty.To {
		t.Fatalf("verify scope [%d,%d] does not include the rewound range [%d,%d]", scope.From, scope.To, dirty.From, dirty.To)
	}
	if got := dirtyReconcileFloor(watermark, genesis, timescale.ProjectionDirtyWindow{From: 2, To: 63_000_000}); got != genesis {
		t.Errorf("a sub-genesis window bottom must clamp to genesis %d, got %d", genesis, got)
	}
	if got := dirtyReconcileFloor(62_000_000, genesis, dirty); got != 62_000_000 {
		t.Errorf("a dirty window must only ever LOWER the floor, got %d from projFrom=62000000", got)
	}
}

// Only a CLEAN run whose scope covered the whole window may delete it.
func TestDirtyWindowSatisfied_OnlyACleanCoveringRunClears(t *testing.T) {
	const genesis = uint32(62_146_641)
	dirty := timescale.ProjectionDirtyWindow{Source: "cctp", From: 62_270_000, To: 63_550_000}
	subGenesis := timescale.ProjectionDirtyWindow{Source: "cctp", From: 2, To: 63_000_000}

	cases := []struct {
		name   string
		win    timescale.ProjectionDirtyWindow
		projOK bool
		floor  uint32
		hi     uint32
		want   bool
	}{
		{"clean run covering the window clears it", dirty, true, 62_270_000, 63_600_000, true},
		{"clean run from below the window bottom clears it", dirty, true, genesis, 63_600_000, true},
		{"a DIRTY verdict must not clear the obligation", dirty, false, genesis, 63_600_000, false},
		{"a floor above the window bottom proved nothing about the rewound prefix", dirty, true, 62_300_000, 63_600_000, false},
		{"a watermark short of the window top leaves rewound ground unverified", dirty, true, genesis, 63_500_000, false},
		{"a genesis-floor clean run clears a sub-genesis window", subGenesis, true, genesis, 63_600_000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirtyWindowSatisfied(tc.win, tc.projOK, tc.floor, genesis, tc.hi); got != tc.want {
				t.Fatalf("dirtyWindowSatisfied(projOK=%v, floor=%d, hi=%d) = %v, want %v",
					tc.projOK, tc.floor, tc.hi, got, tc.want)
			}
		})
	}
}

// stubContractCallDecoder owns every call and fails Decode on one named
// function — the shape of a real decoder bug.
type stubContractCallDecoder struct{ badFunc string }

func (stubContractCallDecoder) Name() string             { return "stub" }
func (stubContractCallDecoder) Matches(_, _ string) bool { return true }
func (d stubContractCallDecoder) Decode(cc dispatcher.ContractCallContext) ([]consumer.Event, error) {
	if cc.FunctionName == d.badFunc {
		return nil, errors.New("decode: unsupported call variant")
	}
	return []consumer.Event{stubCallEvent{}}, nil
}

type stubCallEvent struct{}

func (stubCallEvent) Source() string    { return "stub" }
func (stubCallEvent) EventKind() string { return "stub.call" }

// The ContractCall census and its writer share decodeContractCallTree and both
// soft-fail per call, so a malformed call nets to zero in the diff; only the
// blind tracker can surface it. A clean tree must stay silent.
func TestDecodeContractCallTree_BlindTracksMalformedCalls(t *testing.T) {
	const badLedger uint32 = 51_000_123
	op := clickhouse.ContractCallOp{Ledger: badLedger, TxHash: "aa", Source: "GSOURCE", OpIndex: 0}
	calls := []dispatcher.ContractCall{
		{ContractID: "CAAA", FunctionName: "swap"},
		{ContractID: "CAAA", FunctionName: "relay"},
		{ContractID: "CAAA", FunctionName: "swap"},
	}

	blind := completeness.NewBlindTracker()
	var emitted int
	if err := decodeContractCallTree(op, calls, stubContractCallDecoder{badFunc: "relay"}, blind,
		func(uint32, consumer.Event) error { emitted++; return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2 (the malformed call is skipped by BOTH sides)", emitted)
	}
	got := blind.Result()
	if !got.Any() {
		t.Fatal("the malformed call was skipped silently; projection_ok would be certified on a ledger with a dropped row")
	}
	if got.UndecodableMatched != 1 {
		t.Errorf("UndecodableMatched = %d, want 1", got.UndecodableMatched)
	}
	if len(got.Ledgers) != 1 || got.Ledgers[0] != badLedger {
		t.Errorf("Ledgers = %v, want [%d]", got.Ledgers, badLedger)
	}

	clean := completeness.NewBlindTracker()
	if err := decodeContractCallTree(clickhouse.ContractCallOp{Ledger: 42}, calls[:1], stubContractCallDecoder{badFunc: "none"}, clean,
		func(uint32, consumer.Event) error { return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}
	if clean.Result().Any() {
		t.Errorf("a clean call tree reported blind spots: %+v", clean.Result())
	}
}

// Topic-matched sources have no static contractIDs, so without the registry
// fold a gap on their pool lands in `unattributed` and recognition_ok can never
// fail. Walks mergeRegistryOwners → attributeRecognitionGaps → sourceRecognitionOK.
func TestRecognitionAttribution_TopicMatchedSourceFailsOnItsPoolGap(t *testing.T) {
	const soroswapPool = "CPOOLSOROSWAPxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	const phoenixChild = "CPHOENIXCHILDyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	const foreignContract = "CFOREIGNZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	const soroswapGenesis = 50_746_266

	ownerOf := map[string][]string{}
	mergeRegistryOwners(ownerOf,
		map[string][]string{"phoenix": {phoenixChild}},
		[]string{soroswapPool},
	)
	if !reflect.DeepEqual(ownerOf[soroswapPool], []string{"soroswap"}) {
		t.Fatalf("soroswap pool not attributed: ownerOf[%s]=%q, want [soroswap]", soroswapPool, ownerOf[soroswapPool])
	}
	if !reflect.DeepEqual(ownerOf[phoenixChild], []string{"phoenix"}) {
		t.Fatalf("phoenix child not attributed: ownerOf[%s]=%q, want [phoenix]", phoenixChild, ownerOf[phoenixChild])
	}

	gaps := []completeness.RecognitionGap{
		{ContractID: soroswapPool, Topic0Sym: "swap", MinLedger: 61_000_000, Reason: "no decoder matches"},
		{ContractID: foreignContract, Topic0Sym: "mystery", MinLedger: 62_000_000, Reason: "no decoder matches"},
	}
	recBySource, unattributed := attributeRecognitionGaps(ownerOf, gaps)
	if len(recBySource["soroswap"]) != 1 || recBySource["soroswap"][0] != 61_000_000 {
		t.Fatalf("soroswap gap not attributed to soroswap: recBySource[soroswap]=%v", recBySource["soroswap"])
	}
	if len(unattributed) != 1 || unattributed[0].ContractID != foreignContract {
		t.Fatalf("foreign gap mis-attributed; unattributed=%+v", unattributed)
	}

	ok, problems := sourceRecognitionOK(soroswapGenesis, 61_000_000, recBySource["soroswap"], false, priorProjection{})
	if ok {
		t.Fatal("soroswap recognition_ok stayed TRUE over a dropped topic on its own pool")
	}
	if len(problems) != 1 || problems[0] != 61_000_000 {
		t.Fatalf("recognition problem ledger not pinned into the watermark: %v", problems)
	}
}

// -skip-recognition leaves `attributed` empty, so the verdict must come from
// the prior: confirm a clean prior that reached this tip, refuse the rest.
func TestSourceRecognitionOK_SkipRecognitionCarriesRatherThanAsserts(t *testing.T) {
	const (
		genesis = uint32(50_746_266)
		hi      = uint32(63_900_000)
	)
	for _, tc := range []struct {
		name  string
		prior priorProjection
	}{
		{"no prior verdict", priorProjection{}},
		{"failing prior", priorProjection{known: true, ok: false, tip: hi}},
		{"clean prior short of tip", priorProjection{known: true, ok: true, tip: hi - 10_000}},
	} {
		if ok, problems := sourceRecognitionOK(genesis, hi, nil, true, tc.prior); ok {
			t.Errorf("%s: -skip-recognition published recognition_ok=true, problems=%v", tc.name, problems)
		}
	}
	ok, problems := sourceRecognitionOK(genesis, hi, nil, true, priorProjection{known: true, ok: true, tip: hi})
	if !ok {
		t.Errorf("-skip-recognition must still confirm a prior clean verdict that reached this tip, got ok=false problems=%v", problems)
	}
	if problems != nil {
		t.Errorf("a confirmed carry must add no new problem ledgers, got %v", problems)
	}
}

func TestMergeRegistryOwners_StaticPinWins(t *testing.T) {
	const pinned = "CCCTPCONTRACTxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	ownerOf := map[string][]string{pinned: {"cctp"}}
	mergeRegistryOwners(ownerOf, map[string][]string{"phoenix": {pinned}}, []string{pinned})
	if !reflect.DeepEqual(ownerOf[pinned], []string{"cctp"}) {
		t.Fatalf("static contractID pin lost: ownerOf[%s]=%q, want cctp", pinned, ownerOf[pinned])
	}
}

// -pass resumes each source's projection from its own watermark when its prior
// verdict is clean (keeping the nightly cheap), from genesis when it is red or
// unseeded; outside -pass the floor is the operator-stated max(genesis, -from).
func TestProjectionFloor(t *testing.T) {
	const (
		aquariusGenesis     = uint32(52_728_375)
		healthyGenesis      = uint32(50_746_266)
		blendEmitterGenesis = uint32(51_499_914)
		tip                 = uint32(63_997_554)
	)
	clean := priorProjection{known: true, ok: true, tip: tip}
	clean63 := priorProjection{known: true, ok: true, tip: 63_000_000}
	failing63 := priorProjection{known: true, ok: false, tip: 63_000_000}
	cases := []struct {
		name      string
		genesis   uint32
		pass      bool
		prior     priorProjection
		watermark uint32
		from      uint
		want      uint32
	}{
		{"pass: healthy resumes at watermark", healthyGenesis, true, clean, tip - 100, 0, tip - 100},
		{"pass: recognition-capped resumes at its low watermark", aquariusGenesis, true, clean, 55_363_631, 0, 55_363_631},
		{"pass: never-seeded floors at genesis", blendEmitterGenesis, true, priorProjection{}, 0, 0, blendEmitterGenesis},
		{"pass: sub-genesis watermark clamps", blendEmitterGenesis, true, clean63, 40_000_000, 0, blendEmitterGenesis},
		{"pass: clean prior keeps the cheap resume", sushiGenesis, true, priorProjection{known: true, ok: true, tip: sushiTip}, sushiTip, 0, sushiTip},
		{"pass: failing prior re-verifies from genesis", sushiGenesis, true, priorProjection{known: true, ok: false, tip: sushiTip}, sushiTip, 0, sushiGenesis},
		{"non-pass -from, clean prior", healthyGenesis, false, clean63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, failing prior", healthyGenesis, false, failing63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, no prior", healthyGenesis, false, priorProjection{}, 999_999, 63_000_000, 63_000_000},
		{"non-pass full run ignores the watermark", healthyGenesis, false, clean63, 63_000_000, 0, healthyGenesis},
	}
	for _, tc := range cases {
		if got := projectionFloor(tc.genesis, tc.pass, tc.prior, tc.watermark, tc.from); got != tc.want {
			t.Errorf("%s: projectionFloor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The per-source / per-chunk knobs are incoherent with -pass and must fail
// closed rather than produce a partial pass that looks complete.
func TestValidatePassFlags_RejectsTheKnobsItReplaces(t *testing.T) {
	if err := validatePassFlags(true, "", 0, false, false); err != nil {
		t.Fatalf("a clean -pass must validate, got: %v", err)
	}
	if err := validatePassFlags(false, "aquarius", 55_000_000, true, true); err != nil {
		t.Fatalf("validatePassFlags must not constrain non-pass runs, got: %v", err)
	}

	cases := []struct {
		name    string
		source  string
		from    uint
		skipSub bool
		skipRec bool
		wantIn  string
	}{
		{"pass rejects -source", "aquarius", 0, false, false, "-source"},
		{"pass rejects -from", "", 55_000_000, false, false, "-from"},
		{"pass rejects -skip-substrate", "", 0, true, false, "-skip-substrate"},
		{"pass rejects -skip-recognition", "", 0, false, true, "-skip-recognition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePassFlags(true, tc.source, tc.from, tc.skipSub, tc.skipRec)
			if err == nil {
				t.Fatalf("-pass with %s must fail closed, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q must name the offending flag %q", err.Error(), tc.wantIn)
			}
		})
	}
}

// A caller still written for the removed Postgres soroban_events path fails
// before reading anything.
func TestComputeCompleteness_RequiresCH(t *testing.T) {
	for _, args := range [][]string{
		{"-config", "/nonexistent/stellarindex.toml"},
		{"-config", "/nonexistent/stellarindex.toml", "-source", "sdex", "-from", "60000000"},
		{"-config", "/nonexistent/stellarindex.toml", "-pass"},
	} {
		err := computeCompleteness(args)
		if err == nil || !strings.Contains(err.Error(), "-ch is required") {
			t.Errorf("computeCompleteness(%v) = %v, want the -ch is required error", args, err)
		}
	}
}

// A timer still calling without -write fails instead of exiting 0 having
// published nothing.
func TestComputeCompleteness_RequiresStatedMode(t *testing.T) {
	err := computeCompleteness([]string{"-config", "/nonexistent/stellarindex.toml", "-ch", "-pass"})
	if !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("computeCompleteness without -write or -dry-run = %v, want ErrWriteModeUnstated", err)
	}
}

// An incremental run that carried [genesis, subScanFrom] must fail substrate
// when the lake no longer holds the source's genesis ledger.
func TestSubstrateFloorLoss(t *testing.T) {
	const genesis = 50_746_266
	const watermark = 63_000_000

	if p, lost, detail := substrateFloorLoss(genesis, watermark, genesis, true); !lost {
		t.Fatal("bottom-edge loss NOT detected — a dropped genesis partition below the carried floor read as substrate-intact")
	} else if p != genesis {
		t.Errorf("problem ledger = %d, want genesis %d", p, genesis)
	} else if !strings.Contains(detail, "bottom-edge loss") || !strings.Contains(detail, "-from") {
		t.Errorf("detail must name the loss + the -from carry, got: %q", detail)
	}
	if _, lost, _ := substrateFloorLoss(genesis, watermark, 0, false); lost {
		t.Error("false positive: genesis present but reported bottom-edge loss")
	}
	// A full scan's own head guard owns the verdict; the probe must not double-count.
	if _, lost, _ := substrateFloorLoss(genesis, genesis, genesis, true); lost {
		t.Error("double-count: a full scan (subScanFrom<=genesis) must not also report floor loss")
	}
}

// passProjectionVerdict composes the -pass driver's pure decisions for one
// single-target source, in the driver's order and with its own functions:
// projectionFloor → targetScope → strictPerLedgerDelta → projectionClaim.
// `expected` stands in for the CH re-derive and `actual` for the served counts.
func passProjectionVerdict(genesis, hi uint32, expected, actual map[uint32]int, priorWatermark uint32, prior priorProjection) (bool, uint32, string) {
	servedMin, haveServedRows := hi, false
	for ledger := range actual {
		if ledger >= genesis && ledger <= hi && (!haveServedRows || ledger < servedMin) {
			servedMin, haveServedRows = ledger, true
		}
	}
	projFrom := projectionFloor(genesis, true, prior, priorWatermark, 0)
	servedFrom := targetScope(servedMin, haveServedRows, genesis, 0, hi).From
	sc := targetScope(servedMin, haveServedRows, genesis, projFrom, hi)
	delta, detail := strictPerLedgerDelta("trades", clipCounts(expected, sc), clipCounts(actual, sc), sc.From, sc.To)
	ok, claim := projectionClaim(servedFrom, sc.From, hi, delta == 0, detail, prior, testScope)
	return ok, servedFrom, claim
}

// sushiswap_v3 as measured: no trades in the 5,716 ledgers after factory deploy.
const (
	sushiGenesis    = uint32(61_487_379)
	sushiFirstTrade = uint32(61_493_095)
	sushiTip        = uint32(64_352_012)
)

func sushiCounts() map[uint32]int {
	return map[uint32]int{sushiFirstTrade: 3, 62_000_000: 4, 63_500_000: 7, sushiTip: 1}
}

// A source red before its backfill must go green once backfilled: -pass used
// to resume from the lake watermark (at tip), never re-see the failed range,
// and carry the red forever.
func TestPassProjection_RepairedSourceIsReVerifiedNotCarriedRed(t *testing.T) {
	counts := sushiCounts()
	ok, verifiedFrom, detail := passProjectionVerdict(
		sushiGenesis, sushiTip, counts, counts,
		sushiTip,
		priorProjection{known: true, ok: false, tip: sushiTip},
	)
	if !ok {
		t.Fatalf("projection_ok = false for a fully backfilled source: %s", detail)
	}
	if verifiedFrom != sushiFirstTrade {
		t.Errorf("projection_verified_from = %d, want %d (the served tier's own bottom edge)", verifiedFrom, sushiFirstTrade)
	}
	want := fmt.Sprintf("projection: verified [%d,%d] over the served range of the reconciled tables — %s", sushiFirstTrade, sushiTip, testScope.text)
	if detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

// The re-verify must not weaken the alert: a real hole fails on evidence,
// naming the ledger.
func TestPassProjection_RealHoleStillReadsIncomplete(t *testing.T) {
	const holeLedger = uint32(62_000_000)
	expected := sushiCounts()
	actual := sushiCounts()
	delete(actual, holeLedger)

	// prior and watermark come from the same snapshot row: known=false means watermark 0.
	for _, tc := range []struct {
		name      string
		prior     priorProjection
		watermark uint32
	}{
		{"already red", priorProjection{known: true, ok: false, tip: sushiTip}, sushiTip},
		{"never seeded", priorProjection{}, 0},
	} {
		ok, _, detail := passProjectionVerdict(sushiGenesis, sushiTip, expected, actual, tc.watermark, tc.prior)
		if ok {
			t.Fatalf("%s: projection_ok = true over a real hole at ledger %d", tc.name, holeLedger)
		}
		if want := fmt.Sprintf("ledger=%d expected=%d served=0", holeLedger, expected[holeLedger]); !strings.Contains(detail, want) {
			t.Errorf("%s: detail = %q, want it to localize the hole (%s)", tc.name, detail, want)
		}
	}
}

// An empty served tier floors at genesis and must fail expected>0 vs served=0
// under any prior, including a stale clean one.
func TestPassProjection_NeverBackfilledSourceStillReadsIncomplete(t *testing.T) {
	const upshiftGenesis = uint32(62_623_313)
	expected := map[uint32]int{upshiftGenesis: 2, 63_000_000: 5, sushiTip: 1}

	for _, tc := range []struct {
		name      string
		prior     priorProjection
		watermark uint32
	}{
		{"first pass ever", priorProjection{}, 0},
		{"already red", priorProjection{known: true, ok: false, tip: sushiTip}, upshiftGenesis - 1},
		{"stale clean verdict", priorProjection{known: true, ok: true, tip: sushiTip}, upshiftGenesis - 1},
	} {
		ok, verifiedFrom, detail := passProjectionVerdict(
			upshiftGenesis, sushiTip, expected, map[uint32]int{}, tc.watermark, tc.prior)
		if ok {
			t.Errorf("%s: projection_ok = true for a source with NOTHING projected: %s", tc.name, detail)
		}
		if verifiedFrom != upshiftGenesis {
			t.Errorf("%s: projection_verified_from = %d, want genesis %d", tc.name, verifiedFrom, upshiftGenesis)
		}
	}
}

// expected ∅ vs served ∅ (a wrong or redeployed contract identity) must not
// publish projection_ok=true.
func TestProjectionWithoutEvidence(t *testing.T) {
	const genesis, hi = uint32(57_056_338), uint32(64_000_000)
	empty := []servedFloor{{present: false}, {present: false}}
	servedFrom := targetScope(hi, false, genesis, 0, hi).From
	delta, runDetail := strictPerLedgerDelta("trades", map[uint32]int{}, map[uint32]int{}, servedFrom, hi)
	projOK, _ := projectionClaim(servedFrom, servedFrom, hi, delta == 0, runDetail, priorProjection{}, testScope)
	if !projOK {
		t.Fatal("precondition: projectionClaim certifies ∅ == ∅ over the full range")
	}
	vacuous, d := projectionWithoutEvidence(projOK, len(empty), empty, genesis, hi)
	if !vacuous || !strings.Contains(d, "no evidence") || !strings.Contains(d, "[57056338,64000000]") {
		t.Errorf("empty served tier + empty expectation = (%v, %q), want refused with a no-evidence detail naming the range", vacuous, d)
	}
	if vacuous, _ := projectionWithoutEvidence(true, 0, nil, genesis, hi); !vacuous {
		t.Error("a source with no reconcile targets proves nothing and must be refused")
	}
	served := []servedFloor{{present: false}, {min: 60_000_000, present: true}}
	if vacuous, d := projectionWithoutEvidence(true, len(served), served, genesis, hi); vacuous {
		t.Errorf("a target holding served rows is evidence; the clean claim must stand, got %q", d)
	}
	if vacuous, _ := projectionWithoutEvidence(false, len(empty), empty, genesis, hi); vacuous {
		t.Error("an already-failing claim needs no second refusal")
	}
}

// /v1/coverage's denominator (completeness.AuditedSources) must equal the set
// of sources this catalogue publishes verdicts for, under each config gate.
func TestAuditedSources_LockstepWithCatalogue(t *testing.T) {
	withSEP41 := testConfigWithAllSources()
	withSEP41.Supply.WatchedSEP41Contracts = []string{"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"}
	for name, cfg := range map[string]config.Config{
		"bare":        {},
		"all oracles": testConfigWithAllSources(),
		"with sep41":  withSEP41,
	} {
		cat, _, err := buildReconciliationCatalogue(cfg)
		if err != nil {
			t.Fatalf("%s: build catalogue: %v", name, err)
		}
		got := make([]string, 0, len(cat))
		for _, src := range cat {
			got = append(got, src.name)
		}
		sort.Strings(got)
		want := completeness.AuditedSources(cfg)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: catalogue sources = %v, completeness.AuditedSources = %v — keep them in lockstep", name, got, want)
		}
	}
}

// A short contract_events partition inside a source's range fails it at the
// first affected ledger (never below genesis); one wholly below genesis, or a
// source reading stellar.operations, is untouched.
func TestEventCensusLoss(t *testing.T) {
	short := []clickhouse.EventCensusShortfall{
		{Partition: 50, FirstEventLedger: 50_457_424, Expected: 40, Present: 0},
		{Partition: 62, FirstEventLedger: 62_000_010, Expected: 9, Present: 3},
	}
	soroswapSrc := reconSource{name: "soroswap"}
	p, lost, d := eventCensusLoss(soroswapSrc, 50_746_266, short)
	if !lost || p != 50_746_266 || !strings.Contains(d, "partition 50") {
		t.Errorf("soroswap = (%d, %v, %q), want the straddling partition 50 to fail it at its genesis 50746266", p, lost, d)
	}
	if p, lost, _ := eventCensusLoss(reconSource{name: "cctp"}, 62_146_641, short); !lost || p != 62_146_641 {
		t.Errorf("cctp = (%d, %v), want partition 62 to fail it at genesis 62146641 (partition 50 lies wholly below)", p, lost)
	}
	if _, lost, _ := eventCensusLoss(reconSource{name: "late"}, 63_000_000, short); lost {
		t.Error("a source whose genesis is above every short partition must not fail")
	}
	if _, lost, _ := eventCensusLoss(reconSource{name: "sdex", census: true}, 2, short); lost {
		t.Error("sdex reads stellar.operations, not contract_events; the event census must not fail it")
	}
	if _, lost, _ := eventCensusLoss(soroswapSrc, 50_746_266, nil); lost {
		t.Error("no shortfall must not fail any source")
	}
}

// A -pass defers only sdex/sep41 windows wider than it can re-check (their own
// timers re-prove them); other sources and -source runs lower the floor.
func TestProjectionPlan_DefersOnlyOutlastsPassSourcesInAPass(t *testing.T) {
	sdex := reconSource{name: "sdex", genesis: 2, census: true}
	sep41 := reconSource{name: "sep41_transfers", genesis: 2, reproofOutlastsPass: "heavy"}
	aquarius := reconSource{name: "aquarius", genesis: 2}
	win := timescale.ProjectionDirtyWindow{From: 1_000, To: 2_000}
	cleanPrior := priorProjection{known: true, ok: true, tip: 5_000, verifiedFrom: 2}
	for _, tc := range []struct {
		src       reconSource
		pass      bool
		wantFrom  uint32
		wantDefer bool
	}{
		{sdex, true, 5_000, true},
		{sep41, true, 5_000, true},
		{aquarius, true, 1_000, false},
		{sdex, false, 1_000, false},
		{sep41, false, 1_000, false},
	} {
		from, deferred := projectionPlan(tc.src, tc.pass, cleanPrior, 5_000, 4_000, 50_000, win, true)
		if deferred != tc.wantDefer || (!deferred && from != tc.wantFrom) {
			t.Errorf("projectionPlan(%s, pass=%v) = (%d, %v), want (%d, %v)", tc.src.name, tc.pass, from, deferred, tc.wantFrom, tc.wantDefer)
		}
	}
	if _, deferred := projectionPlan(sdex, true, cleanPrior, 5_000, 0, 50_000, win, false); deferred {
		t.Error("no pending window must never defer")
	}
}

// A deferred window withholds complete and stops the watermark below it.
func TestDeferredDirtyWindow_WithholdsComplete(t *testing.T) {
	srW := completeness.ComputeWatermark(2, 10_000, nil)
	got := deferredDirtyWatermark(srW, timescale.ProjectionDirtyWindow{From: 6_000, To: 7_000})
	if got.Complete || got.Ledger != 5_999 || got.CoveragePct >= 1 || got.FirstProblem != 0 {
		t.Fatalf("deferred = %+v, want complete=false, watermark 5999, coverage < 1, no first problem", got)
	}
	if got := deferredDirtyWatermark(srW, timescale.ProjectionDirtyWindow{From: 0, To: 7_000}); got.Complete || got.Ledger != 1 || got.CoveragePct != 0 {
		t.Errorf("window below genesis: %+v, want watermark genesis-1 and zero coverage", got)
	}
	lakeGap := completeness.ComputeWatermark(2, 10_000, []uint32{3_000})
	if got := deferredDirtyWatermark(lakeGap, timescale.ProjectionDirtyWindow{From: 6_000, To: 7_000}); got != lakeGap {
		t.Errorf("an earlier lake problem must keep its own watermark: %+v, want %+v", got, lakeGap)
	}
}

// A deferring pass's verdict (projection_ok=false, no found problem) must not
// turn the next pass into a from-genesis sdex re-proof.
func TestDeferredDirtyWindow_NextPassKeepsDeferring(t *testing.T) {
	sdex := reconSource{name: "sdex", genesis: 2, census: true}
	win := timescale.ProjectionDirtyWindow{Source: "sdex", From: 60_000, To: 70_000}
	w := deferredDirtyWatermark(completeness.ComputeWatermark(2, 100_000, nil), win)
	published := timescale.CompletenessSnapshot{
		Source: "sdex", Genesis: 2, Tip: 100_000,
		Watermark: w.Ledger, CoveragePct: w.CoveragePct, Complete: w.Complete,
		LakeComplete: true, FirstProblem: w.FirstProblem,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: false,
	}
	if published.FirstProblem != 0 || published.FoundProblem {
		t.Fatalf("a deferred window is pending, not a found problem: %+v", published)
	}
	prior, _, _, priorWM := buildPriorVerdicts([]timescale.CompletenessSnapshot{published})
	if _, deferred := projectionPlan(sdex, true, prior["sdex"], priorWM["sdex"], 0, 100_100, win, true); !deferred {
		t.Fatal("the next pass reconciled sdex from genesis instead of deferring the still-pending window")
	}
	if again := deferredDirtyWatermark(completeness.ComputeWatermark(2, 100_100, nil), win); again.Complete || again.Ledger != w.Ledger {
		t.Errorf("next pass republished %+v, want the same capped, incomplete verdict", again)
	}
}

// A window that fits the pass is re-checked from its bottom and can clear the
// same night; one that outruns it is deferred whatever its Reason says (a
// widened window keeps only its last writer's Reason).
func TestProjectionPlan_WindowFitDecidesRecheckOrDefer(t *testing.T) {
	sdex := reconSource{name: "sdex", genesis: 2, census: true}
	sep41 := reconSource{name: "sep41_transfers", genesis: 2, reproofOutlastsPass: "heavy"}
	const hi = 100_000
	cleanPrior := priorProjection{known: true, ok: true, tip: 99_000, verifiedFrom: 2}
	for _, tc := range []struct {
		src reconSource
		win timescale.ProjectionDirtyWindow
	}{
		{sdex, timescale.ProjectionDirtyWindow{From: 95_000, To: 96_000, Reason: timescale.CHRebuildWriteReason(95_000, 96_000)}},
		{sep41, timescale.ProjectionDirtyWindow{From: 97_500, To: hi, Reason: timescale.ProjectorReplayReason(97_500, hi)}},
		{sdex, timescale.ProjectionDirtyWindow{From: hi - passDirtySpanLedgers, To: hi - passDirtySpanLedgers}},
	} {
		from, deferred := projectionPlan(tc.src, true, cleanPrior, 99_000, 0, hi, tc.win, true)
		if deferred || from != tc.win.From {
			t.Errorf("%s window %+v: projectionPlan = (%d, deferred=%v), want re-checked from %d", tc.src.name, tc.win, from, deferred, tc.win.From)
		}
		if !dirtyWindowSatisfied(tc.win, true, from, tc.src.genesis, hi) {
			t.Errorf("%s window %+v: a clean run from %d must clear it", tc.src.name, tc.win, from)
		}
	}

	const bigHi = 60_000_000
	bigPrior := priorProjection{known: true, ok: true, tip: bigHi, verifiedFrom: 2}
	for _, win := range []timescale.ProjectionDirtyWindow{
		{From: 1_000_000, To: 1_040_000, Reason: timescale.BackfillWriteReason(1_000_000, 1_040_000)},
		{From: 1_000_000, To: bigHi, Reason: timescale.CHRebuildWriteReason(59_999_000, bigHi)},
		{From: bigHi - passDirtySpanLedgers - 1, To: bigHi},
	} {
		from, deferred := projectionPlan(sdex, true, bigPrior, bigHi, 0, bigHi, win, true)
		if !deferred || from != bigHi {
			t.Errorf("window %+v: projectionPlan = (%d, deferred=%v), want deferred at the incremental floor %d", win, from, deferred, bigHi)
		}
	}
}

// Whatever the lake and reconcile said, a deferred window withholds complete
// and holds the watermark at or below window.From-1.
func TestServedAxisVerdict_DeferredNeverAboveWindow(t *testing.T) {
	srW := completeness.ComputeWatermark(2, 100_000, nil)
	for _, from := range []uint32{0, 2, 3, 50_000, 100_000} {
		win := timescale.ProjectionDirtyWindow{From: from, To: 100_000}
		got := servedAxisVerdict(srW, true, true, win)
		limit := max(from, srW.Genesis) - 1
		if got.Complete || got.Ledger > limit {
			t.Errorf("deferred window from %d: %+v, want complete=false and watermark <= %d", from, got, limit)
		}
	}
	if got := servedAxisVerdict(srW, true, false, timescale.ProjectionDirtyWindow{From: 50_000, To: 60_000}); got != combineWatermark(srW, true) {
		t.Errorf("not deferred: %+v, want the combined verdict", got)
	}
	if got := servedAxisVerdict(srW, false, false, timescale.ProjectionDirtyWindow{}); got.Complete {
		t.Errorf("a failing served claim published complete: %+v", got)
	}
}

var testScope = claimScope{text: "scope: reconciled 1 table(s) [t]"}

// complete=true never reads as a genesis-to-tip claim: the detail states the
// range verified and the reconcile scope, also when carried.
func TestProjectionClaim_DetailStatesRangeAndScope(t *testing.T) {
	const servedFrom, hi = uint32(61_500_000), uint32(63_305_532)
	ok, detail := projectionClaim(servedFrom, servedFrom, hi, true, "", priorProjection{known: true, ok: true, tip: hi}, testScope)
	if !ok {
		t.Fatalf("a clean full-scope run must publish true, got: %s", detail)
	}
	for _, want := range []string{"61500000", "63305532"} {
		if !strings.Contains(detail, want) {
			t.Errorf("a passing projection verdict must state the range it verified (missing %s), got: %s", want, detail)
		}
	}

	ok, d := projectionClaim(100, 100, 200, true, "", priorProjection{}, testScope)
	if !ok || !strings.Contains(d, testScope.text) || strings.Contains(d, "the full range the served tier holds") {
		t.Errorf("ok=%v detail=%q", ok, d)
	}
	_, c := projectionClaim(100, 150, 200, true, "", priorProjection{known: true, ok: true, tip: 199, verifiedFrom: 100}, testScope)
	if !strings.HasSuffix(c, "— "+testScope.text) {
		t.Errorf("carried detail lacks scope: %q", c)
	}
}

// A carried prefix is only as proven as its least-proven target: a present
// target with no recorded floor was never reconciled over that prefix.
func TestUnprovenCarryTargets(t *testing.T) {
	src := reconSource{name: "soroswap", targets: []reconTarget{
		{table: "trades", whereFilter: "source = 'soroswap'"},
		{table: "soroswap_skim_events"},
	}}
	keyTrades := timescale.TargetFloorKey("soroswap", "trades", "source = 'soroswap'")
	keySkim := timescale.TargetFloorKey("soroswap", "soroswap_skim_events", "")
	const genesis, runFrom, hi = uint32(100), uint32(500), uint32(600)
	inc := func(served []servedFloor) []projectionScope {
		sc, _, _ := scopesFromServed(src, served, genesis, runFrom, hi)
		return sc
	}
	present := []servedFloor{{min: 150, present: true}, {min: 200, present: true}}
	tests := []struct {
		name   string
		served []servedFloor
		floors map[string]uint32
		want   []string
	}{
		{"skim never floored", present, map[string]uint32{keyTrades: 150}, []string{"soroswap_skim_events"}},
		{"both floored", present, map[string]uint32{keyTrades: 150, keySkim: 200}, nil},
		{"rows projected below the floor since", present, map[string]uint32{keyTrades: 150, keySkim: 300}, []string{"soroswap_skim_events"}},
		{"empty target has no floor to compare", []servedFloor{{min: 150, present: true}, {}}, map[string]uint32{keyTrades: 150}, nil},
		{"this run reached the target's bottom edge", []servedFloor{{min: 150, present: true}, {min: 550, present: true}}, map[string]uint32{keyTrades: 150}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			floors := map[string]timescale.CompletenessTargetFloor{}
			for k, v := range tc.floors {
				floors[k] = timescale.CompletenessTargetFloor{VerifiedFrom: v}
			}
			if got := unprovenCarryTargets(src, inc(tc.served), tc.served, floors); !slices.Equal(got, tc.want) {
				t.Errorf("unprovenCarryTargets = %q, want %q", got, tc.want)
			}
		})
	}
}

// The claim scope built at the call site must refuse a carry over a target no
// clean reconcile ever covered, and name it.
func TestProjectionClaim_CarryRefusesUnprovenTarget(t *testing.T) {
	src := reconSource{name: "soroswap", targets: []reconTarget{
		{table: "trades", whereFilter: "source = 'soroswap'"},
		{table: "soroswap_skim_events"},
	}}
	const genesis, runFrom, hi = uint32(100), uint32(500), uint32(600)
	served := []servedFloor{{min: 150, present: true}, {min: 200, present: true}}
	scopes, servedFrom, runLo := scopesFromServed(src, served, genesis, runFrom, hi)
	prior := priorProjection{known: true, ok: true, tip: runFrom - 1, verifiedFrom: genesis}
	floors := map[string]timescale.CompletenessTargetFloor{
		timescale.TargetFloorKey("soroswap", "trades", "source = 'soroswap'"): {VerifiedFrom: 150},
	}

	ok, d := projectionClaim(servedFrom, runLo, hi, true, "", prior, newClaimScope(src, scopes, served, floors))
	if ok || !strings.Contains(d, "never reconciled soroswap_skim_events") || !strings.Contains(d, "re-run without -from") {
		t.Fatalf("carry over an unfloored present target: ok=%v detail=%q, want false naming soroswap_skim_events", ok, d)
	}

	floors[timescale.TargetFloorKey("soroswap", "soroswap_skim_events", "")] = timescale.CompletenessTargetFloor{VerifiedFrom: 200}
	if ok, d := projectionClaim(servedFrom, runLo, hi, true, "", prior, newClaimScope(src, scopes, served, floors)); !ok {
		t.Fatalf("every present target floored: carry refused: %q", d)
	}
}

// reconcileTarget skips an empty scope without counting it, so the scope may
// name as reconciled only a target the run counted.
func TestProjectionScope_NamesOnlyCountedTargets(t *testing.T) {
	src := reconSource{name: "soroswap", targets: []reconTarget{
		{table: "trades", whereFilter: "source = 'soroswap'"},
		{table: "soroswap_skim_events"},
	}}
	got := src.projectionScope([]projectionScope{{From: 1, To: 9}, {From: 10, To: 9}})
	want := "scope: reconciled 1 table(s) [trades[source = 'soroswap']], not reconciled: soroswap_skim_events (empty scope this run)"
	if got != want {
		t.Errorf("projectionScope = %q, want %q", got, want)
	}
}
