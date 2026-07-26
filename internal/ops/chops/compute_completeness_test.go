// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// TestRunRecognitionScan_ScanErrorFailsClosed pins C2-5 (RFC-8
// detector-fail-open): a recognition scan ERROR — CH unreachable, a query
// timeout, or the load-heaviest DistinctTopicShapes hitting the CH memory
// cap — must FAIL CLOSED. The unfixed compute-completeness logged the error
// and continued with an empty gap slice; the per-source loop reads that as
// "no recognition gaps" → recognition_ok=true, and with substrate ∧
// projection clean writes lake_complete=true / complete=true to
// completeness_snapshots — a FALSE "complete" verdict on the public
// /v1/coverage. The scan error must instead surface as a returned error so
// the run aborts before any snapshot write and the cron sees a non-zero
// exit.
func TestRunRecognitionScan_ScanErrorFailsClosed(t *testing.T) {
	scanErr := errors.New("clickhouse: memory limit (for query) exceeded in DistinctTopicShapes")
	gaps, err := runRecognitionScan(false, func() ([]completeness.RecognitionGap, error) {
		return nil, scanErr
	})
	if err == nil {
		t.Fatal("recognition scan error was swallowed — compute-completeness would launder it into recognition_ok=true / lake_complete=true / complete=true (C2-5); want a returned error that fails the run closed")
	}
	if !errors.Is(err, scanErr) {
		t.Errorf("returned error must wrap the underlying scan error for operator diagnosis, got: %v", err)
	}
	if gaps != nil {
		t.Errorf("a failed scan must yield NO gaps (never an empty-looking clean set the loop reads as recognition_ok=true), got: %v", gaps)
	}
}

// TestRunRecognitionScan_SkipTrustsPriorAudit — -skip-recognition is an
// explicit operator trust flag (trust the prior recognition_ok): no scan is
// run, and no gaps / no error are returned. It must be DISTINCT from a scan
// FAILURE (which fails closed) — the operator's deliberate choice to trust a
// prior audit is not a swallowed error.
func TestRunRecognitionScan_SkipTrustsPriorAudit(t *testing.T) {
	scanned := false
	gaps, err := runRecognitionScan(true, func() ([]completeness.RecognitionGap, error) {
		scanned = true
		return nil, errors.New("scan must not run under -skip-recognition")
	})
	if scanned {
		t.Error("-skip-recognition must NOT invoke the scan")
	}
	if err != nil {
		t.Errorf("-skip-recognition must not error, got: %v", err)
	}
	if gaps != nil {
		t.Errorf("-skip-recognition returns no gaps, got: %v", gaps)
	}
}

// TestRunRecognitionScan_CleanScanPassesGapsThrough — the fail-closed fix
// must NOT change the genuine-INCOMPLETE path: a scan that SUCCEEDS and
// finds a real recognition gap still returns that gap, so the per-source
// loop writes recognition_ok=false / complete=false exactly as before. Only
// ERRORS fail closed.
func TestRunRecognitionScan_CleanScanPassesGapsThrough(t *testing.T) {
	want := []completeness.RecognitionGap{{ContractID: "CBID", Topic0Sym: "transfer", MinLedger: 51_000_000}}
	gaps, err := runRecognitionScan(false, func() ([]completeness.RecognitionGap, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("a successful scan must not error, got: %v", err)
	}
	if len(gaps) != 1 || gaps[0].MinLedger != 51_000_000 {
		t.Errorf("a real recognition gap must pass through unchanged, got: %v", gaps)
	}
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

// oldRetentionStart reproduces the PRE-FIX projection floor exactly as
// compute-completeness computed it (`retentionStart = tip - 1_500_000`, applied
// to any source with a trades target). It exists only so the tests below can
// prove the blind band that floor created — production must never derive a
// floor from tip again (DAT-09 / N-F2).
func oldRetentionStart(tip uint32) uint32 { return tip - 1_500_000 }

// TestTargetScope_DataDerivedFloorSeesLossBelowTheOldRetentionWindow pins
// DAT-09 / N-F2. The projection floor was `tip - 1_500_000` — a hardcoded ~100d
// retention assumption that has been WRONG since migration 0031 removed the
// retention policy on `trades` ("operator wants every raw trade preserved
// forever"). The served tier keeps everything it was ever given, so a real
// served-tier loss older than ~100 days sat permanently outside the reconcile
// scope: Δ=0, projection_ok=true, complete=true, over a hole. The floor must
// instead come from the served tier's OWN data (MIN(ledger) for that target).
func TestTargetScope_DataDerivedFloorSeesLossBelowTheOldRetentionWindow(t *testing.T) {
	const (
		genesis   = uint32(50_746_266) // soroswap
		tip       = uint32(63_305_532)
		servedMin = uint32(61_500_000) // real MIN(ledger) of trades WHERE source='soroswap'
		hole      = uint32(61_600_000) // a REAL served-tier loss, below the old floor
	)
	oldFloor := oldRetentionStart(tip) // 61_805_532
	if hole >= oldFloor || hole < servedMin {
		t.Fatalf("fixture invalid: the hole (%d) must sit BELOW the old floor (%d) and inside the served range (>= %d)", hole, oldFloor, servedMin)
	}

	scope := targetScope(servedMin, true, genesis, 0 /* full run */, tip)
	if scope.From != servedMin {
		t.Fatalf("projection floor = %d, want the served tier's own MIN(ledger) %d — a floor derived from tip (tip-1_500_000 = %d) leaves every served ledger below it structurally unverifiable (DAT-09/N-F2)", scope.From, servedMin, oldFloor)
	}
	if scope.To != tip {
		t.Fatalf("scope.To = %d, want tip %d", scope.To, tip)
	}

	// Two rows lost at `hole`; everything else faithful.
	expected := map[uint32]int{hole: 5, 62_000_000: 3, 63_000_000: 1}
	served := map[uint32]int{hole: 3, 62_000_000: 3, 63_000_000: 1}
	src := reconSource{name: "soroswap"} // strict per-ledger default

	delta, detail := projectionDelta(src, "trades",
		clipCounts(expected, scope), clipCounts(served, scope), scope.From, scope.To)
	if delta != 2 {
		t.Fatalf("Σ|Δ| = %d, want 2 (|5-3| at ledger %d) — the served-tier hole must flip projection_ok/complete to false; detail=%q", delta, hole, detail)
	}
	if !strings.Contains(detail, "ledger=61600000") {
		t.Errorf("detail must localize the loss to ledger %d, got: %s", hole, detail)
	}

	// And prove the fixture is the real defect: under the pre-fix floor the
	// SAME data reconciles clean, because the hole is outside the scope.
	oldScope := projectionScope{From: oldFloor, To: tip}
	if d, _ := projectionDelta(src, "trades",
		clipCounts(expected, oldScope), clipCounts(served, oldScope), oldScope.From, oldScope.To); d != 0 {
		t.Fatalf("fixture invalid: the hole must be INVISIBLE under the pre-fix tip-1.5M floor, got Δ=%d", d)
	}
}

// TestTargetScope_PerTargetNotPerSource — the pre-fix floor was applied at
// SOURCE level (`hasTradesTarget(src)`), so a trades source's FULL-HISTORY
// tables (soroswap_skim_events, phoenix_liquidity, comet_liquidity) were
// un-verified below tip-1.5M too, purely because a sibling table was named
// `trades`. Each target must be scoped by its OWN served floor.
func TestTargetScope_PerTargetNotPerSource(t *testing.T) {
	const (
		genesis = uint32(50_746_266)
		tip     = uint32(63_305_532)
	)
	trades := targetScope(61_500_000, true, genesis, 0, tip) // never backfilled below 61.5M
	skim := targetScope(genesis, true, genesis, 0, tip)      // full history
	if trades.From != 61_500_000 {
		t.Errorf("trades floor = %d, want 61500000", trades.From)
	}
	if skim.From != genesis {
		t.Fatalf("soroswap_skim_events floor = %d, want the source genesis %d — a full-history target must not inherit a sibling table's floor (nor tip-1_500_000 = %d)", skim.From, genesis, oldRetentionStart(tip))
	}
}

// TestTargetScope_EmptyTargetFailsClosed — a target with NO served rows must
// scope from genesis so the reconcile compares expected>0 against served=0 and
// FAILS. Excusing an empty table ("no data, nothing to check") is the exact
// fail-open shape this verdict exists to prevent.
func TestTargetScope_EmptyTargetFailsClosed(t *testing.T) {
	const (
		genesis = uint32(51_499_546)
		tip     = uint32(63_305_532)
	)
	sc := targetScope(0, false, genesis, 0, tip)
	if sc.From != genesis {
		t.Fatalf("empty-target floor = %d, want genesis %d (fail closed)", sc.From, genesis)
	}
	src := reconSource{name: "comet"}
	expected := map[uint32]int{52_000_000: 4}
	if delta, _ := projectionDelta(src, "comet_liquidity",
		clipCounts(expected, sc), map[uint32]int{}, sc.From, sc.To); delta != 4 {
		t.Errorf("a wiped served table must reconcile as a 4-row loss, got Δ=%d", delta)
	}
}

// TestTargetScope_IncrementalOnlyRaises — -from may only RAISE the floor; it
// can never lower it below what the served tier holds (that would re-introduce
// false "served=0" gaps under a never-backfilled prefix).
func TestTargetScope_IncrementalOnlyRaises(t *testing.T) {
	const (
		genesis = uint32(50_746_266)
		tip     = uint32(63_305_532)
	)
	if sc := targetScope(61_500_000, true, genesis, 63_300_000, tip); sc.From != 63_300_000 {
		t.Errorf("-from above the served floor must raise the scope: got %d, want 63300000", sc.From)
	}
	if sc := targetScope(61_500_000, true, genesis, 51_000_000, tip); sc.From != 61_500_000 {
		t.Errorf("-from below the served floor must not lower it: got %d, want 61500000", sc.From)
	}
}

// TestProjectionClaim_IncrementalRunCannotUpgradeAFailingVerdict pins INV-5:
// the served (`complete`) axis silently regressed from false to TRUE.
//
// completeness-incremental.sh (hourly) passes `-from = min(watermark)`, but
// watermark_ledger is the LAKE (substrate∧recognition) axis, which sits AT tip
// whenever the lake is clean. So the run reconciled only the newest ~hour of
// ledgers, never re-saw the projection mismatch that had pinned complete=false,
// and published complete=true — a verdict improving with no evidence, which
// ADR-0033 forbids (complete through W requires every claim to hold
// contiguously to W). A partial run must be able to CONFIRM or DOWNGRADE, never
// UPGRADE.
func TestProjectionClaim_IncrementalRunCannotUpgradeAFailingVerdict(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		hi         = uint32(63_305_532)
		runFrom    = uint32(63_300_000) // -from = min(watermark) ≈ the prior tip
	)
	failingPrior := priorProjection{known: true, ok: false, tip: 63_300_000}

	ok, detail := projectionClaim(servedFrom, runFrom, hi, true /* this run's window is clean */, "", failingPrior)
	if ok {
		t.Fatalf("an incremental run that reconciled only [%d,%d] UPGRADED a failing verdict to complete=true without ever re-checking [%d,%d] (INV-5); detail=%q", runFrom, hi, servedFrom, runFrom-1, detail)
	}
	if !strings.Contains(detail, "61500000") || !strings.Contains(detail, "63299999") {
		t.Errorf("detail must name the range that was NOT reconciled, got: %s", detail)
	}

	// A run that DID cover the whole served range is self-evidencing — this is
	// the deliberate, full re-verify that clears a failing verdict.
	if full, d := projectionClaim(servedFrom, servedFrom, hi, true, "", failingPrior); !full {
		t.Errorf("a full-scope clean run must be able to clear a failing prior verdict, got false: %s", d)
	}

	// A clean prior contiguous with this run's window may be carried forward.
	cleanPrior := priorProjection{known: true, ok: true, tip: 63_300_000}
	carry, carryDetail := projectionClaim(servedFrom, runFrom, hi, true, "", cleanPrior)
	if !carry {
		t.Errorf("a contiguous clean prior must carry the skipped prefix, got false: %s", carryDetail)
	}
	if !strings.Contains(carryDetail, "carried from the prior clean verdict") {
		t.Errorf("a carried claim must say so (the operator has to know it was not re-verified), got: %s", carryDetail)
	}

	// No prior verdict at all → fail closed.
	if noPrior, d := projectionClaim(servedFrom, runFrom, hi, true, "", priorProjection{}); noPrior {
		t.Errorf("a partial run with NO prior verdict must not claim the skipped prefix: %s", d)
	}

	// A stale prior leaves [prior.tip+1, runFrom-1] verified by nobody.
	if stale, d := projectionClaim(servedFrom, runFrom, hi, true, "", priorProjection{known: true, ok: true, tip: 62_000_000}); stale {
		t.Errorf("a stale prior leaves an unverified band — must not claim it: %s", d)
	}

	// A mismatch found by THIS run always fails, whatever the prior said.
	if found, d := projectionClaim(servedFrom, servedFrom, hi, false, "trades: 1 mismatched ledger(s)", priorProjection{known: true, ok: true, tip: hi}); found {
		t.Errorf("a mismatch found by this run can never be laundered by a clean prior: %s", d)
	}
}

// TestProjectionClaim_DetailAlwaysStatesTheVerifiedRange — `complete=true` must
// never read as a genesis-to-tip claim. The served tier legitimately holds no
// trades below ~61.5M (they were never projected — see
// notes/DECISION-genesis-complete-verdict-2026-07-16.md), so the verdict has to
// say what it actually reconciled; the genesis claim is the separate
// lake_complete axis.
func TestProjectionClaim_DetailAlwaysStatesTheVerifiedRange(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		hi         = uint32(63_305_532)
	)
	ok, detail := projectionClaim(servedFrom, servedFrom, hi, true, "", priorProjection{known: true, ok: true, tip: hi})
	if !ok {
		t.Fatalf("a clean full-scope run must publish true, got: %s", detail)
	}
	for _, want := range []string{"61500000", "63305532"} {
		if !strings.Contains(detail, want) {
			t.Errorf("a passing projection verdict must state the range it verified (missing %s), got: %s", want, detail)
		}
	}
}

// TestClipCounts_BoundsTheExpectedSideToTheTargetScope — the expected census is
// re-derived once over the UNION of a source's target scopes, so each target
// must compare only the ledgers inside its own scope. Without the clip, a
// full-history sibling target (soroswap_skim_events from genesis) would drag
// pre-61.5M ledgers into the trades compare and manufacture false gaps.
func TestClipCounts_BoundsTheExpectedSideToTheTargetScope(t *testing.T) {
	m := map[uint32]int{50_800_000: 7, 61_500_000: 2, 62_000_000: 1, 64_000_000: 9}
	got := clipCounts(m, projectionScope{From: 61_500_000, To: 63_305_532})
	want := map[uint32]int{61_500_000: 2, 62_000_000: 1}
	if len(got) != len(want) {
		t.Fatalf("clipCounts = %v, want %v", got, want)
	}
	for ledger, n := range want {
		if got[ledger] != n {
			t.Errorf("clipCounts[%d] = %d, want %d", ledger, got[ledger], n)
		}
	}
	if len(m) != 4 {
		t.Error("clipCounts must not mutate its input")
	}
}

// TestSourceSubstrateOK pins the F1 consumer fail-open fix (reviewer CONFIRMED):
// the per-source substrate verdict must fail a high-genesis source when the lake
// reports a COVERAGE failure (empty/tail → problem = tip). Pre-fix, an empty
// lake reported problem=2 and soroswap (genesis 50_746_266) read `2 < 50.7M =
// true`, certifying substrate-OK on an empty lake.
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
		{"empty lake (problem=tip) fails soroswap — the F1 regression", tip, true, soroswap, false},
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

// TestDetectFloorLoss pins the N-F2 residual fix: a bottom-edge truncation of
// the served tier must FAIL the projection axis instead of quietly becoming
// the new reconcile scope.
//
// The reason this needs its own check at all is that the reconcile cannot
// catch it. targetScope floors each target at its own MIN(ledger), so when the
// oldest rows are deleted the floor rises with the loss, the surviving rows
// reconcile perfectly, and the verdict reads complete. "Someone dropped the
// oldest 10M ledgers of trades" and "we never projected below there" are
// indistinguishable from inside the reconcile. Only the durable floor
// (migration 0116) separates them.
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
			// First run after the migration. An absent floor must be
			// "nothing to compare against", never floor=0 — the latter
			// would fail every target at once.
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
			// Deeper than ever verified — new ground, not loss.
			name:      "served min BELOW the floor is not loss",
			served:    []servedFloor{{min: 100, present: true}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 0,
		},
		{
			// The case the whole fix exists for.
			name:      "served min ABOVE the floor is loss",
			served:    []servedFloor{{min: 71_000_000, present: true}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 1,
			wantIn:    "9500000 ledgers of served rows below the recorded floor are GONE",
		},
		{
			// Maximal loss: the table is empty but we had verified it.
			// Must be reported, not skipped as "no rows, nothing to check".
			name:      "empty target with a prior floor is loss",
			served:    []servedFloor{{min: 0, present: false}, {min: 2, present: true}},
			floors:    floors(map[string]uint32{keyTrades: 61_500_000, keySkim: 2}),
			wantCount: 1,
			wantIn:    "holds NO rows but was previously verified from ledger 61500000",
		},
		{
			// An empty target with NO floor stays the first-run case.
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

// TestFloorsToRecord_EmptyTargetEarnsNoFloor closes the N-F2 residual left by
// migration 0116's own wiring: recordFloors banked scopes[i].From for EVERY
// target, and targetScope floors an EMPTY target at `genesis` (fail closed, so
// expected>0 vs served=0 reconciles as loss). A source whose target has no
// rows yet — a rare skim event, a SEP-41 contract newly promoted into the
// catalogue — reconciles clean (0 expected, 0 served) and banked
// verified_from=genesis, asserting the rows had been verified present from
// genesis when the run had never seen a row.
//
// The next run, once the table legitimately acquires its first row, then reads
// MIN=firstRow > genesis and detectFloorLoss reports every ledger between as
// GONE — projection_ok=false, complete=false, PERMANENTLY: LEAST() can never
// raise the recorded floor back, and detectFloorLoss's own verdict blocks
// re-recording. Only a manual DELETE from completeness_target_floors clears it.
func TestFloorsToRecord_EmptyTargetEarnsNoFloor(t *testing.T) {
	const (
		genesis  = uint32(50_746_266)
		tip1     = uint32(63_000_000)
		firstRow = uint32(62_000_000) // the target's first-ever row, run 2
		tip2     = uint32(63_300_000)
	)
	src := reconSource{
		name:    "phoenix",
		genesis: genesis,
		targets: []reconTarget{
			{table: "trades", whereFilter: "source = 'phoenix'"},
			{table: "phoenix_stake_events"}, // no stake has ever happened
		},
	}

	// ── Run 1: the stake table is empty; the reconcile is clean. ──
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
			t.Fatalf("floorsToRecord banked verified_from=%d for the EMPTY target %s — "+
				"a clean reconcile of an empty table proves 0 expected == 0 served, not that its "+
				"rows were verified present from ledger %d; the first row it ever receives now "+
				"reads as loss", f.VerifiedFrom, f.Table, f.VerifiedFrom)
		}
	}
	if len(recorded) != 1 || recorded[0].Table != "trades" || recorded[0].VerifiedFrom != 61_500_000 {
		t.Fatalf("floorsToRecord = %+v, want exactly the non-empty target trades@61500000", recorded)
	}

	// ── Run 2: the table's first row lands. This must NOT read as loss. ──
	floors := make(map[string]timescale.CompletenessTargetFloor, len(recorded))
	for _, f := range recorded {
		floors[timescale.TargetFloorKey(f.Source, f.Table, f.Filter)] = f
	}
	run2Served := []servedFloor{
		{min: 61_500_000, present: true},
		{min: firstRow, present: true},
	}
	if loss := detectFloorLoss(src, run2Served, floors); len(loss) != 0 {
		t.Fatalf("a target's FIRST-EVER row was reported as served-tier loss: %v — "+
			"projection_ok/complete are now false for %s permanently (LEAST() cannot raise the "+
			"floor and detectFloorLoss blocks re-recording)", loss, src.name)
	}

	// And the fix must not blunt the real check: once the table HAS a floor,
	// losing rows below it still fails.
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

// TestSubstrateClaim_IncrementalRunCannotUpgradeAFailingLakeVerdict is the
// C4-057 regression, and the exact twin of
// TestProjectionClaim_IncrementalRunCannotUpgradeAFailingVerdict one axis over.
//
// The substrate scan is bounded by the incremental `-from`
// (compute_completeness.go: `subScanFrom = *fromLedger`), but `substrate_ok`
// and `lake_complete` were published over [genesis, tip]. On a clean suffix
// the `problems` slice comes back EMPTY, so the verdict asserted "the
// certified archive is contiguous + hash-chained from genesis" on evidence
// covering only the newest window — and, because
// run-compute-completeness.sh re-runs every source from its prior watermark
// on the daily timer, a source pinned false by a real gap BELOW the floor
// flipped silently to true on the next pass.
func TestSubstrateClaim_IncrementalRunCannotUpgradeAFailingLakeVerdict(t *testing.T) {
	const (
		genesis  = uint32(50_457_424)
		hi       = uint32(63_305_532)
		scanFrom = uint32(63_300_000) // -from = the prior watermark
	)
	failingPrior := priorProjection{known: true, ok: false, tip: 63_300_000}

	ok, detail := substrateClaim(genesis, hi, scanFrom, true /* the scanned suffix is clean */, 0, failingPrior)
	if ok {
		t.Fatalf("an incremental run that scanned only [%d,%d] UPGRADED a failing lake verdict to substrate_ok=true without re-scanning [%d,%d]; detail=%q",
			scanFrom, hi, genesis, scanFrom-1, detail)
	}
	if !strings.Contains(detail, "50457424") || !strings.Contains(detail, "63299999") {
		t.Errorf("detail must name the range that was NOT scanned, got: %s", detail)
	}

	// A full scan (floor at or below genesis) is self-evidencing — the
	// deliberate re-verify that clears a failing verdict.
	full, fullDetail := substrateClaim(genesis, hi, 2, true, 0, failingPrior)
	if !full {
		t.Errorf("a full-range clean scan must be able to clear a failing prior verdict, got false: %s", fullDetail)
	}
	if !strings.Contains(fullDetail, "from this source's genesis") {
		t.Errorf("a full claim must say it reached genesis, got: %s", fullDetail)
	}

	// A clean prior contiguous with this run's window may be carried — this
	// is the production daily driver's normal path and must keep working.
	cleanPrior := priorProjection{known: true, ok: true, tip: 63_300_000}
	carry, carryDetail := substrateClaim(genesis, hi, scanFrom, true, 0, cleanPrior)
	if !carry {
		t.Errorf("a contiguous clean prior must carry the skipped prefix, got false: %s", carryDetail)
	}
	if !strings.Contains(carryDetail, "carried from the prior clean verdict") {
		t.Errorf("a carried claim must say so — that string IS the published verified-floor disclosure, got: %s", carryDetail)
	}

	// No prior verdict at all → fail closed.
	if noPrior, d := substrateClaim(genesis, hi, scanFrom, true, 0, priorProjection{}); noPrior {
		t.Errorf("a partial scan with NO prior verdict must not claim the skipped prefix: %s", d)
	}

	// A stale prior leaves [prior.tip+1, scanFrom-1] scanned by nobody.
	if stale, d := substrateClaim(genesis, hi, scanFrom, true, 0, priorProjection{known: true, ok: true, tip: 62_000_000}); stale {
		t.Errorf("a stale prior leaves an unverified band — must not claim it: %s", d)
	}

	// A gap/break found by THIS run always fails, whatever the prior said.
	found, foundDetail := substrateClaim(genesis, hi, 2, false, 61_234_567, priorProjection{known: true, ok: true, tip: hi})
	if found {
		t.Errorf("a lake gap found by this run can never be laundered by a clean prior: %s", foundDetail)
	}
	if !strings.Contains(foundDetail, "61234567") {
		t.Errorf("detail must name the problem ledger, got: %s", foundDetail)
	}
}

// TestSubstrateClaim_SkipSubstrateCarriesRatherThanAsserts — `-skip-substrate`
// scans NOTHING, and used to publish substrate_ok=true unconditionally: a
// failing lake verdict was cleared by an operator convenience flag with zero
// evidence. It must now carry the prior verdict instead.
//
// scanFrom > hi is how the caller encodes "no scan happened".
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
	// A prior clean verdict that already reached this tip is confirmable.
	ok, detail := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{known: true, ok: true, tip: hi})
	if !ok {
		t.Errorf("-skip-substrate must still confirm a prior clean verdict that reached this tip: %s", detail)
	}
	if !strings.Contains(detail, "-skip-substrate") {
		t.Errorf("the detail must disclose that nothing was scanned, got: %s", detail)
	}
	// A prior clean verdict that stopped SHORT of this tip leaves a band
	// nobody verified — the tip advances every 5s, so this is the common case
	// and it must read false rather than silently extend the old claim.
	if ok, d := substrateClaim(genesis, hi, noScan, true, 0, priorProjection{known: true, ok: true, tip: hi - 10_000}); ok {
		t.Errorf("-skip-substrate must not extend a prior verdict past the tip it reached: %s", d)
	}
}

// ─── C4-059: the ContractCall census is per-row blind too ────────

// stubContractCallDecoder owns every call and fails Decode on one named
// function — the shape of a real decoder bug (a call variant it claims by
// contract+function and then cannot parse).
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

// TestDecodeContractCallTree_MalformedCallNetsToZero is the C4-059
// regression for the ContractCall census (band, soroswap-router).
//
// These sources have NO soroban_events landing zone, so
// reDeriveContractCallCensus IS the projection oracle — and it soft-fails
// per call, exactly as the ch-rebuild WRITER that shares the same function
// does. A call whose Decode errors is therefore missing from the expected
// side AND from the served side, the per-ledger diff nets to zero, and
// recordFloors runs while projection_ok is certified on a ledger with a
// provably-dropped row.
//
// expectedProjection's comment used to assert this could not happen ("the
// census oracles soft-fail per claim, never per row"). It was false.
func TestDecodeContractCallTree_MalformedCallNetsToZero(t *testing.T) {
	const badLedger uint32 = 51_000_123
	op := clickhouse.ContractCallOp{
		Ledger:  badLedger,
		TxHash:  "aa",
		Source:  "GSOURCE",
		OpIndex: 0,
	}
	calls := []dispatcher.ContractCall{
		{ContractID: "CAAA", FunctionName: "swap"},
		{ContractID: "CAAA", FunctionName: "relay"}, // decoder fails on this
		{ContractID: "CAAA", FunctionName: "swap"},
	}

	blind := completeness.NewBlindTracker()
	var emitted int
	if err := decodeContractCallTree(op, calls, stubContractCallDecoder{badFunc: "relay"}, blind,
		func(uint32, consumer.Event) error { emitted++; return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}

	// Pre-condition: the defect is invisible to the counts. Two calls
	// decoded, so the census expects 2 — and the writer (same function)
	// wrote 2 — so a per-ledger diff is clean.
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2 (the malformed call is skipped by BOTH sides)", emitted)
	}

	got := blind.Result()
	if !got.Any() {
		t.Fatal("the malformed call was skipped silently: the census and the writer drop it identically, " +
			"so the diff nets to zero and projection_ok is certified on a ledger with a dropped row")
	}
	if got.UndecodableMatched != 1 {
		t.Errorf("UndecodableMatched = %d, want 1", got.UndecodableMatched)
	}
	if len(got.Ledgers) != 1 || got.Ledgers[0] != badLedger {
		t.Errorf("Ledgers = %v, want [%d]", got.Ledgers, badLedger)
	}
}

// TestDecodeContractCallTree_CleanTreeIsNotBlind — the counter must be
// silent on the healthy path, or every ContractCall source would pin its
// watermark forever.
func TestDecodeContractCallTree_CleanTreeIsNotBlind(t *testing.T) {
	blind := completeness.NewBlindTracker()
	op := clickhouse.ContractCallOp{Ledger: 42}
	calls := []dispatcher.ContractCall{{ContractID: "CAAA", FunctionName: "swap"}}
	if err := decodeContractCallTree(op, calls, stubContractCallDecoder{badFunc: "none"}, blind,
		func(uint32, consumer.Event) error { return nil }); err != nil {
		t.Fatalf("decodeContractCallTree: %v", err)
	}
	if blind.Result().Any() {
		t.Errorf("a clean call tree reported blind spots: %+v", blind.Result())
	}
}
