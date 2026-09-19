package chops

import (
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
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// F050: ch-rebuild -write runs the CURRENT decoders over a historical
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
	if external.BackfillSafe(sushiswap_v3.SourceName) {
		t.Fatalf("%s is now BackfillSafe — re-point this test at a source that is still unaudited", sushiswap_v3.SourceName)
	}
	err := chRebuild(chRebuildArgs(t, "-write", "-sources", "aquarius,"+sushiswap_v3.SourceName))
	if err == nil {
		t.Fatal("ch-rebuild -write over an unaudited source returned nil")
	}
	if !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("ch-rebuild -write -sources …,%s got past the BackfillSafe gate (F050); error was: %v",
			sushiswap_v3.SourceName, err)
	}
	if !strings.Contains(err.Error(), "["+sushiswap_v3.SourceName+"]") {
		t.Errorf("refusal must name exactly the unaudited source, not the audited one beside it: %v", err)
	}
}

// The default mode writes nothing and is how an unaudited decoder gets
// evaluated against history; the audited -write is the sanctioned
// rebuild. Neither may be refused.
func TestCHRebuild_GateLeavesDryRunAndAuditedWriteAlone(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"dry-run of an unaudited source": {"-sources", sushiswap_v3.SourceName},
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
	if !reflect.DeepEqual(got, want) || !containsStr(got, sushiswap_v3.SourceName) {
		t.Errorf("unsafe set = %v, want %v (must include %s)", got, want, sushiswap_v3.SourceName)
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
