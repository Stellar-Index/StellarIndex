// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"strings"
	"testing"

	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
)

// sourcesTestCatalogue is a stand-in for buildReconciliationCatalogue's
// output shaped like the real one: an event source with a decoder, a
// ContractCall source, and the census-only sdex entry. Only the names
// matter to the -sources gate.
func sourcesTestCatalogue() []reconSource {
	return []reconSource{
		{name: "soroswap"},
		{name: "aquarius"},
		{name: "soroswap_router"},
		{name: "sdex", census: true},
	}
}

// TestCheckCHRebuildSources_unknownNameRefused pins the K015 DO-NOTHING
// trap: `ch-rebuild -sources sdx` (a typo for sdex) made enabled() false
// for every real source, so the run decoded nothing, printed its report
// and exited 0 — a rebuild-of-nothing reported as success.
func TestCheckCHRebuildSources_unknownNameRefused(t *testing.T) {
	err := checkCHRebuildSources(sourcesTestCatalogue(), []string{"sdx"})
	if err == nil {
		t.Fatal("checkCHRebuildSources accepted -sources sdx — an unknown name selects nothing and the run exits 0 having re-derived nothing")
	}
	if !strings.Contains(err.Error(), "sdx") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	// The operator has to be able to see what they meant to type.
	if !strings.Contains(err.Error(), "sdex") {
		t.Errorf("refusal must list the known sources; got: %v", err)
	}
}

// TestCheckCHRebuildSources_partialTypoRefused: one good name does not
// launder a typo beside it. The run would re-derive soroswap and silently
// nothing for the misspelling.
func TestCheckCHRebuildSources_partialTypoRefused(t *testing.T) {
	err := checkCHRebuildSources(sourcesTestCatalogue(), []string{"soroswap", "aquaris"})
	if err == nil {
		t.Fatal("checkCHRebuildSources accepted -sources soroswap,aquaris — the misspelled name is silently dropped")
	}
	if !strings.Contains(err.Error(), "aquaris") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	if strings.Contains(err.Error(), "[soroswap ") {
		t.Errorf("refusal must not name the VALID source as unknown; got: %v", err)
	}
}

// TestCheckCHRebuildSources_knownNamesAccepted is the other half of the
// contract, and the one that keeps the gate from over-refusing: a known
// name is accepted even when THIS invocation would not engage its pass.
// scripts/ops/ch-rebuild-projected.sh asks -preflight for its whole source
// set and deletes only the subset the verdict names back, so narrowing
// must stay a legal answer — refusing here would abort that script on
// every window.
func TestCheckCHRebuildSources_knownNamesAccepted(t *testing.T) {
	cat := sourcesTestCatalogue()
	for _, named := range [][]string{
		nil,                         // no -sources: the whole catalogue
		{"soroswap"},                // plain event source
		{"soroswap_router"},         // ContractCall source, no -contract-calls
		{"sdex"},                    // census source, no -sdex
		{"soroswap", "aquarius"},    // several
		{sep41transfers.SourceName}, // sep41 pair, no -sep41 and not promoted
		{sep41supply.SourceName},    //
	} {
		if err := checkCHRebuildSources(cat, named); err != nil {
			t.Errorf("checkCHRebuildSources(%v) = %v, want nil — a known source this run would not engage still narrows legitimately", named, err)
		}
	}
}

// TestCHRebuildSourceUniverse_carriesTheSEP41Pair guards the promotion
// caveat: buildReconciliationCatalogue only promotes the SEP-41 sources
// when [supply] watched_sep41_contracts is configured, so the universe
// must add them unconditionally or `-sources sep41_supply` would be
// refused as unknown on a host without the watched set.
func TestCHRebuildSourceUniverse_carriesTheSEP41Pair(t *testing.T) {
	got := chRebuildSourceUniverse([]reconSource{{name: "soroswap"}})
	want := map[string]bool{
		"soroswap":                false,
		sep41transfers.SourceName: false,
		sep41supply.SourceName:    false,
	}
	for _, name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected source %q in the universe", name)
			continue
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("source %q missing from the universe (got %v)", name, got)
		}
	}
}

// TestCHRebuild_ValidatesSourcesAgainstTheCatalogue pins the CALL SITE the
// gate above is worthless without: chRebuild needs Postgres and ClickHouse
// past the catalogue build, so no test can reach the refusal through the
// entry point. Deleting the call would leave every unit test green while
// `-sources sdx` went back to re-deriving nothing and exiting 0. Mirrors
// TestCHRebuild_BothBackfillSafeLegsAreCalledInOrder's approach.
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
	writer := strings.Index(body, "drainAndWrite(")
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
