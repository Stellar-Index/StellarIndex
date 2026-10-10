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

// Every source ch-rebuild can decode must have a definite answer that is
// NOT the unknown-name fallback by accident: a catalogue source outside
// both the registry and the replay namespace maps would be refused
// forever with a misleading "audit pending".
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

// ch-rebuild's BackfillSafe gate has two legs, and only the first
// is reachable from a test of the entry point.
//
//   - leg 1 asks about the sources the operator NAMED, before the config
//     load — TestCHRebuild_WriteRefusesNamedSourceThatIsNotBackfillSafe
//     drives chRebuild itself and would notice it missing.
//   - leg 2 asks about everything the run would decode, which with no
//     -sources is the WHOLE catalogue. It can only be asked once the
//     catalogue exists, i.e. after the config load and a Postgres open, so
//     TestCHRebuild_DefaultCatalogueWriteIsGated has to call
//     reDerivedSourcesInRun/checkCHRebuildBackfillSafe directly. Deleting
//     the CALL in chRebuild therefore failed no test, and a bare
//     `ch-rebuild -write` — the widest possible run — would decode every
//     unaudited source unasked.
//
// chRebuild needs ClickHouse and Postgres past that point, so pin the
// call sites at the source: both legs exist inside chRebuild, leg 1
// precedes the config load, and leg 2 sits after the catalogue (and the
// opt-in sep41 catalogue) is built and before anything reaches the
// writer.
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
	writer := strings.Index(body, "drainAndWrite(")

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

// The gate refuses a `-write` with no -sources (the whole catalogue holds
// unaudited decoders), so a runbook that prescribes one prescribes a
// command that cannot run. Two operations docs did:
// history-completeness-plan.md §2.2 (`-sdex-gaps … -write`, which also
// lacked the -sdex its pass needs) and sep41-mint-recovery.md §3
// (`ch-rebuild -sep41 -write -contracts …`). This drives the corrected
// flag sets through BOTH legs, and shows the forms without -sources
// are exactly what leg 2 refuses.
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
