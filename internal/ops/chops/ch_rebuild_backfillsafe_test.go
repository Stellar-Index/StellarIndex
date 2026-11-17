package chops

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// ch-rebuild -write runs the CURRENT decoders over a historical
// lake range and its rows win over the stored ones, yet only `backfill`
// consulted external.BackfillSafe.

func chRebuildArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	// The config does not exist: a run that gets PAST the named-source
	// gate fails on the config load, so the error identifies which
	// happened.
	missing := filepath.Join(t.TempDir(), "absent.toml")
	return append([]string{"-config", missing, "-from", "61000000", "-to", "61100000"}, extra...)
}

func TestCHRebuild_WriteRefusesNamedSourceThatIsNotBackfillSafe(t *testing.T) {
	t.Parallel()
	if external.BackfillSafe(upshift.SourceName) {
		t.Fatalf("%s is now BackfillSafe — re-point this test at a source that is still unaudited", upshift.SourceName)
	}
	err := chRebuild(chRebuildArgs(t, "-write", "-sources", "aquarius,"+upshift.SourceName))
	if err == nil {
		t.Fatal("ch-rebuild -write over an unaudited source returned nil")
	}
	if !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("ch-rebuild -write -sources …,%s got past the BackfillSafe gate (F050); error was: %v",
			upshift.SourceName, err)
	}
	if !strings.Contains(err.Error(), "["+upshift.SourceName+"]") {
		t.Errorf("refusal must name exactly the unaudited source, not the audited one beside it: %v", err)
	}
}

// The default mode writes nothing and is how an unaudited decoder gets
// evaluated against history; the audited -write is the sanctioned
// rebuild. Neither may be refused.
func TestCHRebuild_GateLeavesDryRunAndAuditedWriteAlone(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"dry-run of an unaudited source": {"-sources", upshift.SourceName},
		// scripts/ops/ch-rebuild-projected.sh's SRC default, verbatim.
		"sanctioned audited write": {"-write", "-sources", "aquarius,soroswap,phoenix,comet,blend,cctp,rozo,defindex"},
		"sep41 + backstop write":   {"-write", "-sep41", "-sources", strings.Join([]string{sep41supply.SourceName, sep41transfers.SourceName, blend_backstop.SourceName}, ",")},
	} {
		err := chRebuild(chRebuildArgs(t, args...))
		if err == nil {
			t.Fatalf("%s: expected the absent config to fail the run", name)
		}
		if strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("%s: refused by the BackfillSafe gate: %v", name, err)
		}
	}
}

// With no -sources the run selects the whole catalogue, so the second
// leg of the gate has to see every source each pass would decode.
func TestCHRebuild_DefaultCatalogueWriteIsGated(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	cat = dropReconSources(cat, sep41transfers.SourceName, sep41supply.SourceName)
	all := func(string) bool { return true }

	inRun := reDerivedSourcesInRun(cat, nil, chRebuildPasses{}, all)
	if len(inRun) == 0 {
		t.Fatal("no event sources in the default run — this test is asserting nothing")
	}
	gerr := checkCHRebuildBackfillSafe(inRun)
	if gerr == nil {
		t.Fatal("a default-catalogue -write decodes unaudited sources and was not refused (F050)")
	}
	// The refusal must name exactly the catalogue sources the registry
	// does not attest — derived here from the registry, not hard-coded.
	var want []string
	for _, name := range inRun {
		if !external.ReplayBackfillSafe(name) {
			want = append(want, name)
		}
	}
	got := external.UnsafeReplaySources(inRun)
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) || !containsStr(got, upshift.SourceName) {
		t.Errorf("unsafe set = %v, want %v (must include %s)", got, want, upshift.SourceName)
	}
	for _, name := range got {
		if !strings.Contains(gerr.Error(), name) {
			t.Errorf("refusal does not name %s: %v", name, gerr)
		}
	}
}

// reDerivedSourcesInRun must mirror each pass's own selection: an opt-in
// pass that is off contributes nothing, -sources narrows every pass.
func TestReDerivedSourcesInRun_MirrorsPassSelection(t *testing.T) {
	t.Parallel()
	cat := []reconSource{
		{name: "aquarius", dec: aquarius.NewDecoder()},
		{name: "sdex", census: true},
		{name: "band", callDec: band.NewDecoder("CBANDCONTRACT")},
	}
	sep41Cat := []reconSource{{name: sep41supply.SourceName}, {name: sep41transfers.SourceName}}
	all := func(string) bool { return true }

	cases := []struct {
		name    string
		passes  chRebuildPasses
		enabled func(string) bool
		want    []string
	}{
		{"event pass only", chRebuildPasses{}, all, []string{"aquarius"}},
		{
			"every pass",
			chRebuildPasses{sep41: true, contractCalls: true, sdex: true},
			all,
			[]string{"aquarius", "sdex", "band", sep41supply.SourceName, sep41transfers.SourceName},
		},
		{
			"-sources narrows",
			chRebuildPasses{sep41: true, contractCalls: true, sdex: true},
			func(n string) bool { return n == "band" || n == sep41supply.SourceName },
			[]string{"band", sep41supply.SourceName},
		},
	}
	for _, tc := range cases {
		got := reDerivedSourcesInRun(cat, sep41Cat, tc.passes, tc.enabled)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every source ch-rebuild can decode must have a definite answer, not the unknown-name fallback by accident: a catalogue
// source outside both the registry and the replay namespace maps would be refused forever with a misleading "audit
// pending".
func TestCHRebuild_EveryCatalogueSourceResolvesInTheReplayNamespace(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	registryless := map[string]bool{
		blend_backstop.SourceName: true,
		sep41supply.SourceName:    true,
		sep41transfers.SourceName: true,
	}
	for _, src := range cat {
		_, registered := external.Registry[src.name]
		switch {
		case registered:
		case registryless[src.name]:
			if !external.ReplayBackfillSafe(src.name) {
				t.Errorf("%s has no registry row and is not resolved by the replay namespace maps", src.name)
			}
		default:
			t.Errorf("catalogue source %q is neither in external.Registry nor a declared registry-less "+
				"projected source — ch-rebuild -write and projector-replay will refuse it; register it "+
				"(BackfillSafe=false until audited) or declare it in registry.go", src.name)
		}
	}
}

// BackfillSafe has two legs: leg 1 (sources named, before config load) runs via chRebuild; leg 2 (whole catalogue) needs
// config. Deleting either call would fail no test, so call sites are pinned at the source: leg 1 before config load, leg
// 2 before the writer.
func TestCHRebuild_BothBackfillSafeLegsAreCalledInOrder(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}

	leg1 := strings.Index(body, "checkCHRebuildBackfillSafe(parseCSVList(*only))")
	leg2 := strings.Index(body, "checkCHRebuildBackfillSafe(reDerivedSourcesInRun(cat, sep41Cat, passes, enabled))")
	cfgLoad := strings.Index(body, "config.LoadWithEnv(")
	catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
	sep41Catalogue := strings.Index(body, "buildSEP41ReconSources(cfg)")
	passes := strings.Index(body, "passes := chRebuildPasses{sep41: *includeSEP41, contractCalls: *contractCalls, sdex: *includeSDEX}")
	writer := strings.Index(body, "sink.drain(")

	if cfgLoad < 0 || catalogue < 0 || sep41Catalogue < 0 || writer < 0 {
		t.Fatalf("anchors not found (config load %d, catalogue %d, sep41 catalogue %d, writer %d) — this test is asserting nothing",
			cfgLoad, catalogue, sep41Catalogue, writer)
	}
	switch {
	case leg1 < 0:
		t.Error("chRebuild never asks the BackfillSafe gate about the NAMED sources (leg 1, F050)")
	case leg1 > cfgLoad:
		t.Error("leg 1 of the BackfillSafe gate runs AFTER the config load — the refusal now needs a reachable database")
	}
	switch {
	case leg2 < 0:
		t.Error("chRebuild never asks the BackfillSafe gate about the sources the run would DECODE (leg 2, F050): " +
			"a `ch-rebuild -write` with no -sources selects the whole catalogue and would decode every unaudited source unasked")
	case leg2 < catalogue || leg2 < sep41Catalogue:
		t.Error("leg 2 of the BackfillSafe gate runs BEFORE the catalogues are built — it cannot see what the run decodes")
	case passes < 0 || passes > leg2:
		t.Error("leg 2 is not fed the run's real pass selection (-sep41 / -contract-calls / -sdex) — an opt-in pass would go unasked")
	case leg2 > writer:
		t.Error("leg 2 of the BackfillSafe gate runs AFTER the writer — unaudited rows are already stored when it refuses")
	}
}

// funcBodyFrom returns the source text of the top-level function `name`
// (from its `func` keyword up to the next top-level func), or "" when it
// is absent.
func funcBodyFrom(src, name string) string {
	start := strings.Index(src, "\nfunc "+name+"(")
	if start < 0 {
		return ""
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	return body
}

// The gate refuses `-write` with no -sources (the whole catalogue holds unaudited decoders), so a runbook prescribing
// one cannot run. This drives the documented flag sets (history-completeness-plan.md 2.2, sep41-mint-recovery.md 3)
// through both legs and shows forms without -sources are what leg 2 refuses.
func TestCHRebuild_DocumentedWriteCommandsPassBothGateLegs(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	cat = dropReconSources(cat, sep41transfers.SourceName, sep41supply.SourceName)
	sep41Cat := []reconSource{{name: sep41supply.SourceName}, {name: sep41transfers.SourceName}}

	cases := []struct {
		doc     string
		sources string
		passes  chRebuildPasses
		flags   []string
	}{
		{
			doc:     "docs/operations/history-completeness-plan.md §2.2",
			sources: "sdex",
			passes:  chRebuildPasses{sdex: true},
			flags:   []string{"-sdex", "-sdex-gaps"},
		},
		{
			doc:     "docs/operations/sep41-mint-recovery.md §3",
			sources: "sep41_supply,sep41_transfers",
			passes:  chRebuildPasses{sep41: true},
			flags:   []string{"-sep41", "-contracts", "CWATCHEDCONTRACT"},
		},
	}
	for _, tc := range cases {
		// Leg 1, through the real entry point: past the gate, onto the
		// absent config.
		args := append([]string{"-write", "-sources", tc.sources}, tc.flags...)
		rerr := chRebuild(chRebuildArgs(t, args...))
		if rerr == nil || strings.Contains(rerr.Error(), "not BackfillSafe") {
			t.Errorf("%s: documented command refused at leg 1 (or ran without a config): %v", tc.doc, rerr)
		}
		// Leg 2, over the real catalogue with the command's own selection.
		named := map[string]bool{}
		for _, s := range parseCSVList(tc.sources) {
			named[s] = true
		}
		inRun := reDerivedSourcesInRun(cat, sep41Cat, tc.passes, func(n string) bool { return named[n] })
		if len(inRun) == 0 {
			t.Errorf("%s: documented command decodes nothing — -sources %q does not meet its pass flags", tc.doc, tc.sources)
		}
		if gerr := checkCHRebuildBackfillSafe(inRun); gerr != nil {
			t.Errorf("%s: documented command refused at leg 2: %v", tc.doc, gerr)
		}
		// The form without -sources is refused.
		all := func(string) bool { return true }
		if gerr := checkCHRebuildBackfillSafe(reDerivedSourcesInRun(cat, sep41Cat, tc.passes, all)); gerr == nil {
			t.Errorf("%s: the same command WITHOUT -sources is no longer refused — the doc's -sources note is stale", tc.doc)
		}
	}
}

// And the docs themselves: every fenced command block in the two runbooks
// that runs `ch-rebuild … -write` must scope it with -sources.
func TestCHRebuild_RunbookWriteCommandsNameTheirSources(t *testing.T) {
	t.Parallel()
	blocks := 0
	for _, rel := range []string{
		"../../../docs/operations/history-completeness-plan.md",
		"../../../docs/operations/sep41-mint-recovery.md",
	} {
		b, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		parts := strings.Split(string(b), "\n```")
		// Odd indexes are the insides of fenced blocks.
		for i := 1; i < len(parts); i += 2 {
			block := parts[i]
			if !strings.Contains(block, "ch-rebuild") || !strings.Contains(block, "-write") {
				continue
			}
			blocks++
			if !strings.Contains(block, "-sources ") {
				t.Errorf("%s: a `ch-rebuild … -write` command block has no -sources — the BackfillSafe gate "+
					"refuses a whole-catalogue -write, so this runbook step cannot run as written (F050):\n%s", rel, block)
			}
		}
	}
	if blocks < 3 {
		t.Fatalf("found %d ch-rebuild -write command blocks, want at least 3 — this test is asserting nothing", blocks)
	}
}

func TestCHRebuild_PreflightWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-preflight", "-sources", "aquarius"))
	if err == nil || !strings.Contains(err.Error(), "-preflight") {
		t.Fatalf("a bare -preflight runs none of the -write guards and must not answer ok; got: %v", err)
	}
}

func TestCHRebuild_PreflightRunsTheNamedSourceGate(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-write", "-preflight", "-sources", "aquarius,upshift"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("-write -preflight over an unaudited source was not refused by the BackfillSafe gate: %v", err)
	}
}

// chRebuild needs Postgres and ClickHouse past config load, so, like the BackfillSafe legs, the preflight's position is
// pinned at the source: after the last refusal and before the first lake read. It is executed for real in
// test/integration/lake_test.go.
func TestCHRebuild_PreflightReturnsAfterEveryGuardAndBeforeTheLake(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	exit := strings.Index(body, "return reportCHRebuildPreflight(")
	leg2 := strings.Index(body, "checkCHRebuildBackfillSafe(reDerivedSourcesInRun(")
	overlap := strings.Index(body, "checkCHRebuildLiveOverlap(")
	rangeGuard := strings.Index(body, "> maxBufferedRange")
	lake := strings.Index(body, "clickhouse.Stream")
	if exit < 0 || leg2 < 0 || overlap < 0 || rangeGuard < 0 || lake < 0 {
		t.Fatalf("anchors not found (exit %d, leg2 %d, overlap %d, range %d, lake %d) — this test is asserting nothing",
			exit, leg2, overlap, rangeGuard, lake)
	}
	for name, pos := range map[string]int{"BackfillSafe leg 2": leg2, "live-cursor guard": overlap, "buffered-range guard": rangeGuard} {
		if exit < pos {
			t.Errorf("-preflight returns BEFORE the %s — it would answer ok to a run the guard refuses", name)
		}
	}
	if exit > lake {
		t.Error("-preflight returns AFTER the first lake read — it is no longer a preflight")
	}
}

// TestCHRebuild_ModeFlagsFollowTheSharedWriteGate pins ch-rebuild to the shared -write/-dry-run convention. The named-
// source BackfillSafe refusal is armed only in write mode, so it tells the modes apart before any datastore; a dry run
// falls through to the absent config.
func TestCHRebuild_ModeFlagsFollowTheSharedWriteGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode      []string
		wantWrite bool
	}{
		{nil, false},
		{[]string{"-dry-run"}, false},
		{[]string{"-write"}, true},
		{[]string{"-dry-run", "-write"}, true},
	} {
		args := append(append([]string{}, tc.mode...), "-sources", upshift.SourceName)
		err := chRebuild(chRebuildArgs(t, args...))
		if err == nil {
			t.Fatalf("%v: chRebuild succeeded against a config that does not exist", tc.mode)
		}
		if got := strings.Contains(err.Error(), "not BackfillSafe"); got != tc.wantWrite {
			t.Errorf("%v: write-mode refusal = %v, want %v (err: %v)", tc.mode, got, tc.wantWrite, err)
		}
		if !tc.wantWrite && !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("%v: a dry run should reach the config load, got: %v", tc.mode, err)
		}
	}
}

// The predicate is only a guard if chRebuild consults it at the range check.
func TestCHRebuild_RangeGuardUsesThePassSelection(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if !strings.Contains(body, "chRebuildBuffers(cat, sep41Cat, passes, enabled) && *to-*from > maxBufferedRange") {
		t.Fatal("chRebuild's maxBufferedRange guard no longer keys on chRebuildBuffers(cat, sep41Cat, passes, enabled)")
	}
}

// TestCHRebuild_ValidatesSourcesAgainstTheCatalogue pins the call site of the gate above.
// chRebuild needs Postgres and ClickHouse past the catalogue build, so no test reaches the
// refusal through the entry point; without this, deleting the call leaves every unit test green.
func TestCHRebuild_ValidatesSourcesAgainstTheCatalogue(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	call := strings.Index(body, "checkCHRebuildSources(cat, parseCSVList(*only))")
	catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
	warm := strings.Index(body, "warmCatalogueGates(")
	writer := strings.Index(body, "sink.drain(")
	if catalogue < 0 || warm < 0 || writer < 0 {
		t.Fatalf("anchors not found (catalogue %d, warm %d, writer %d) — this test is asserting nothing", catalogue, warm, writer)
	}
	switch {
	case call < 0:
		t.Error("chRebuild never validates -sources against the catalogue (K015): an unrecognised name makes " +
			"enabled() false for every real source, so the run decodes nothing, reports a clean count and exits 0")
	case call < catalogue:
		t.Error("the -sources check runs BEFORE the catalogue is built — it cannot know which names exist")
	case call > warm:
		t.Error("the -sources check runs AFTER the gate warm-up — a typo pays for the registry reads before being refused")
	case call > writer:
		t.Error("the -sources check runs AFTER the writer")
	}
}

// A -write run rewrites served rows below the completeness watermark. Before writing it must file the same obligation
// projector-replay and projected-rebuild file, one per re-derived source, or the next verdict keeps its clean claim over
// the rewritten range.
func TestCHRebuild_WriteRecordsDirtyWindowBeforeWriting(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	record := strings.Index(body, "recordCHRebuildDirtyWindows(ctx, store, os.Stderr, lo, hi, rederived, timescale.CHRebuildWriteReason)")
	preflight := strings.Index(body, "return reportCHRebuildPreflight(")
	writer := strings.Index(body, "sink.drain(")
	if preflight < 0 || writer < 0 {
		t.Fatalf("anchors not found (preflight %d, writer %d) — this test is asserting nothing", preflight, writer)
	}
	switch {
	case record < 0:
		t.Fatal("chRebuild -write records no projection dirty window: the next completeness verdict carries its prior clean claim over the rewritten range")
	case record < preflight:
		t.Error("the record precedes the -preflight stop: a preflight, which writes nothing, would file an obligation")
	case record > writer:
		t.Error("the record follows the writer: a run that fails partway leaves rewritten rows certified by stale evidence")
	}
	if !strings.Contains(body[record-300:record], "if write {") {
		t.Error("the record is not gated on -write: a dry run would file an obligation")
	}
	if !strings.Contains(body[record:record+300], "return ") {
		t.Error("a failed record does not stop the run: it would write without the obligation filed")
	}
}

// The documented invocation must clear every refusal that runs before the config load (flag conflicts, BackfillSafe leg 1).
func TestCHRebuild_TradesByAccountInvocationPassesEarlyRefusals(t *testing.T) {
	err := chRebuild(chRebuildArgs(t, "-write", "-sdex", "-trades-by-account"))
	if err == nil || strings.Contains(err.Error(), "refusing") || strings.Contains(err.Error(), "-trades-by-account") {
		t.Fatalf("err = %v, want only the absent-config failure", err)
	}
}

// Pins the ch-rebuild call site at the source (chRebuild needs ClickHouse and Postgres): the warm runs on the fresh
// catalogue and precedes both the preseed (which seeds INTO the warmed decoders) and the writer.
func TestCHRebuild_WarmsTheCatalogueGatesBeforeAnythingReadsThem(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
	preseed := strings.Index(body, "preseedFactoryChildren(")
	writer := strings.Index(body, "sink.drain(")
	if catalogue < 0 || preseed < 0 || writer < 0 {
		t.Fatalf("anchors not found (catalogue %d, preseed %d, writer %d) — this test is asserting nothing",
			catalogue, preseed, writer)
	}
	warm := strings.Index(body, "warmCatalogueGates(ctx, store, logger, cat)")
	switch {
	case warm < 0:
		t.Error("chRebuild never warms the catalogue's gated decoders from protocol_contracts: " +
			"`ch-rebuild -write` re-derives on the bare in-code seed and rebuilds a table without " +
			"the contracts an operator admitted (RLT-430)")
	case warm < catalogue:
		t.Error("the gate warm runs BEFORE the catalogue is built")
	case warm > preseed:
		t.Error("the gate warm runs AFTER preseedFactoryChildren — it rebuilds the decoders and discards what the preseed registered")
	case warm > writer:
		t.Error("the gate warm runs AFTER the writer")
	}
}

func TestCHRebuild_CreditRefusalsWithWriteIsRefused(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-write", "-credit-refusals", "-sources", "aquarius"))
	if err == nil || !strings.Contains(err.Error(), "-credit-refusals") {
		t.Fatalf("-write -credit-refusals would charge the budget twice per print; got: %v", err)
	}
}
